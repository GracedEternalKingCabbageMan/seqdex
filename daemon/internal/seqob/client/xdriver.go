package client

// xdriver.go runs the CROSS-CHAIN lift handshake defined in xcourier.go over the
// opaque relay courier, settling with the proven pkg/xchain HTLC engine (the same
// engine the RFQ daemon drives; this file re-fronts it onto the order book, it does
// not reimplement settlement). Phase 2b implements the FORWARD direction
// (offer.direction = BTC_TO_ASSET: the taker pays BTC and holds the secret); the
// REVERSE direction (maker = secret holder) lands in phase 2e.
//
// Shape notes, mirrored from driver.go and the RFQ flows:
//   - The drivers own Seal/Open. The maker's FIRST act on any courier bytes is
//     Crypter.Open, so a relay substituting session keys can only ever deny
//     service, never elicit a signed/funded artifact (driver.go "ITEM C").
//   - Everything from the peer is untrusted: redeem scripts are re-derived from
//     the agreed terms and byte-compared by pkg/xchain's verifiers; amounts and
//     locktimes are checked against the SIGNED offer's expectations.
//   - On a live parent chain LockBTCLeg returns height 0 (broadcast-only); the
//     TAKER waits out MinBTCConf itself and computes the confirmation height
//     Hp = tip - confs + 1 before announcing the leg, exactly like the RFQ
//     maker's watcher does, so the maker's VerifyBTCLeg and the later anchor
//     gate (SEQ block anchored at/above Hp) line up.
//   - State is in-memory, like the RFQ MVP: a process restart mid-swap loses the
//     session and the CLTV refund paths are the safety net. Callers should
//     persist (secret, keys, legs, locktimes) before funding anything; the
//     taker result carries them all even on error for exactly that reason.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// XcSend delivers one sealed courier frame to the peer (a relay SwapMsg write).
type XcSend func(sealed []byte) error

// XcRecv blocks for the next sealed courier frame, up to timeout.
type XcRecv func(timeout time.Duration) ([]byte, error)

// XcOps is the narrow settlement seam the drivers run against. LiveXcOps binds it
// to a real *xchain.Swap + chains; tests substitute a fake to exercise the
// handshake without RPC. Method contracts are pkg/xchain's.
type XcOps interface {
	// BTC side.
	BtcTip() (int64, error)
	BtcConfirmations(txid string) (int, error)
	LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error)
	VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
		txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error)
	ClaimBTCLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error)
	RefundBTCLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error)

	// SEQ side (always the anchored Sequentia/Elements node).
	SeqTip() (int64, error)
	SeqAnchorHeightOf(blockHash string) (int64, error)
	// SeqAnchorTip reports the node's CURRENT anchor view (getanchorstatus): the
	// active tip's Bitcoin-anchor height and the health of that anchor. Unlike
	// SeqAnchorHeightOf — a FIXED per-block header commitment — this value
	// ADVANCES as the committee anchors forward, which is what makes it usable as
	// a precondition to wait on before funding an asset leg.
	SeqAnchorTip() (int64, string, error)
	// SeqBlockOnActiveChain reports whether a block is still connected to the
	// node's ACTIVE chain. getblockheader answers for ORPHANED blocks too, so
	// every anchor gate must establish this before trusting a block's committed
	// anchorheight (see the gate loops: the confirming block is re-derived from
	// the leg's txid every iteration, never cached).
	SeqBlockOnActiveChain(blockHash string) (bool, error)
	SeqBlockHashOfTx(txid string) (string, error)
	SeqFeeRate(assetHex string) (uint64, bool)
	LockSEQLeg(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*xchain.LegLock, string, error)
	VerifySEQLeg(hashH, claimPub, refundPub, providedScript []byte, seqLocktime uint32,
		txid string, vout uint32, amount uint64, assetID string, minConf int) (*xchain.VerifiedSEQLeg, error)
	VerifySeqLegSafe(seqBlockHash string, btcLegHeight int64) (*xchain.AnchorEvidence, error)
	ClaimSEQLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error)
	WatchSEQClaim(leg *xchain.LegLock) (claimTxid string, secret []byte, err error)
	InjectSecret(secret []byte) error
	RefundSEQLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error)
	SeqBroadcast(rawHex string) (string, error)
}

// LiveXcOps implements XcOps over a real swap and its chains.
type LiveXcOps struct {
	Swap *xchain.Swap
	BTC  *xchain.BitcoinChain
	SEQ  *xchain.Chain
}

func (o *LiveXcOps) BtcTip() (int64, error)                    { return o.BTC.BlockCount() }
func (o *LiveXcOps) BtcConfirmations(txid string) (int, error) { return o.BTC.Confirmations(txid) }
func (o *LiveXcOps) LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	return o.Swap.LockBTCLeg(claimPub, refundPub, amountCoins, locktime)
}
func (o *LiveXcOps) VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	// assetID "" = real BTC on the parent chain.
	return o.Swap.VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript, btcLocktime, txid, vout, amount, "", minConf)
}
func (o *LiveXcOps) ClaimBTCLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error) {
	return o.Swap.ClaimBTCLeg(leg, key, fee)
}
func (o *LiveXcOps) RefundBTCLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Swap.RefundBTCLeg(leg, key, nLockTime, fee)
}
func (o *LiveXcOps) SeqTip() (int64, error) { return o.SEQ.BlockCount() }
func (o *LiveXcOps) SeqAnchorHeightOf(blockHash string) (int64, error) {
	return o.SEQ.BlockAnchorHeight(blockHash)
}
func (o *LiveXcOps) SeqAnchorTip() (int64, string, error) {
	st, err := o.SEQ.GetAnchorStatus()
	if err != nil {
		return 0, "", err
	}
	return st.AnchorHeight, st.AnchorStatus, nil
}
func (o *LiveXcOps) SeqBlockOnActiveChain(blockHash string) (bool, error) {
	return o.SEQ.BlockOnActiveChain(blockHash)
}
func (o *LiveXcOps) SeqBlockHashOfTx(txid string) (string, error) {
	return o.SEQ.BlockHashOfTx(txid)
}
func (o *LiveXcOps) SeqFeeRate(assetHex string) (uint64, bool) {
	return o.SEQ.FeeExchangeRate(assetHex)
}
func (o *LiveXcOps) LockSEQLeg(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*xchain.LegLock, string, error) {
	return o.Swap.LockSEQLeg(claimPub, refundPub, amountCoins, assetLabel, locktime)
}
func (o *LiveXcOps) VerifySEQLeg(hashH, claimPub, refundPub, providedScript []byte, seqLocktime uint32,
	txid string, vout uint32, amount uint64, assetID string, minConf int) (*xchain.VerifiedSEQLeg, error) {
	return o.Swap.VerifySEQLeg(hashH, claimPub, refundPub, providedScript, seqLocktime, txid, vout, amount, assetID, minConf)
}
func (o *LiveXcOps) VerifySeqLegSafe(seqBlockHash string, btcLegHeight int64) (*xchain.AnchorEvidence, error) {
	return o.Swap.VerifySeqLegSafe(seqBlockHash, btcLegHeight)
}
func (o *LiveXcOps) ClaimSEQLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error) {
	return o.Swap.ClaimSEQLeg(leg, key, fee)
}
func (o *LiveXcOps) WatchSEQClaim(leg *xchain.LegLock) (string, []byte, error) {
	return o.Swap.WatchSEQClaim(leg)
}
func (o *LiveXcOps) InjectSecret(secret []byte) error { return o.Swap.InjectSecret(secret) }
func (o *LiveXcOps) RefundSEQLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Swap.RefundSEQLeg(leg, key, nLockTime, fee)
}
func (o *LiveXcOps) SeqBroadcast(rawHex string) (string, error) { return o.SEQ.Broadcast(rawHex) }

// XcTiming bundles the polling/wait knobs; zero values take the defaults.
//
// NOTE (owner ruling, Andreas 2026-07-25): none of these is allowed to cancel a
// HEALTHY wait for a contested chain to clear — "we should let users decide if
// the wait is intolerable and they want to cancel the trade, rather than cancel
// it automatically. We cannot really predict how long contested blocks will take
// to clear anyway." They are ordinary per-STEP courier/RPC budgets. The two waits
// that gate real value — the asset funder's anchor precondition, and the taker's
// wait for the funder to announce that leg — are bounded by PROTOCOL DEADLINES
// (the timelock claim windows, fundWindow below) and by the caller's Ctx, never
// by a flat number of minutes.
type XcTiming struct {
	Poll         time.Duration // chain/watch polling cadence (default 5s)
	TermsWait    time.Duration // taker: wait for XcTerms (default 2m)
	BtcConfWait  time.Duration // taker: wait for own BTC-leg confirmation (default 90m)
	SeqLockWait  time.Duration // per-ATTEMPT courier/RPC step budget (default 15m). For the taker's XcSeqLegLocked wait it is only the size of one recv window: an idle window is re-armed while the claim window is open, so a maker legitimately waiting out its anchor precondition is not cut off.
	AnchorWait   time.Duration // CLAIMANT ONLY: how long to wait out a transient anchorstatus/certification flap on an otherwise-good leg (default 20m). The asset FUNDER's precondition does NOT use this — it is bounded by the timelock (fundWindow).
	TermsReqWait time.Duration // maker: wait for XcTermsRequest (default 2m)
	BtcFundWait  time.Duration // maker: wait for XcBtcLegFunded (default 2h; covers the taker's conf wait)
}

func (t *XcTiming) setDefaults() {
	if t.Poll <= 0 {
		t.Poll = 5 * time.Second
	}
	if t.TermsWait <= 0 {
		t.TermsWait = 2 * time.Minute
	}
	if t.BtcConfWait <= 0 {
		t.BtcConfWait = 90 * time.Minute
	}
	if t.SeqLockWait <= 0 {
		// A per-ATTEMPT budget, not a swap deadline. The taker re-arms it for as
		// long as its own claim window (T_seq) is still open, because the maker may
		// legitimately be waiting out its anchor precondition — a wait nobody can
		// put a number on, and which only the user may cancel.
		t.SeqLockWait = 15 * time.Minute
	}
	if t.AnchorWait <= 0 {
		// CLAIMANT-side flap budget only. The ordering conjunct is already terminal
		// (ErrAnchorOrderingTerminal), so what remains here is a transient bad
		// anchorstatus / not-yet-certified block on a leg that is otherwise good.
		t.AnchorWait = 20 * time.Minute
	}
	if t.TermsReqWait <= 0 {
		t.TermsReqWait = 2 * time.Minute
	}
	if t.BtcFundWait <= 0 {
		t.BtcFundWait = 2 * time.Hour
	}
}

// Sentinel errors.
var (
	ErrXcPeerFailed = errors.New("peer failed the cross-chain lift") // wraps an XcFail from the counterparty
	ErrXcBadTerms   = errors.New("cross-chain terms rejected")
	ErrXcRefunded   = errors.New("cross-chain leg refunded")

	// ErrSeqAnchorBehindBTC means our OWN node's anchor view had not reached the
	// BTC-leg height by the time the PROTOCOL made funding unsafe (or our node
	// stopped answering), so we refused to fund the asset leg. Nothing of ours
	// moved; the counterparty refunds its BTC after T_btc.
	ErrSeqAnchorBehindBTC = errors.New("seq anchor behind the BTC leg; asset leg NOT funded")

	// ErrFundWindowClosed means the timelock claim window ran down while we were
	// waiting: funding now would leave the claimant (or us) too little room to
	// claim before the refund path opens, which is the one thing that turns a
	// stalled swap into a lost leg. This is the ONLY automatic stop the owner
	// ruling permits for the funder's wait — everything else is the user's call.
	ErrFundWindowClosed = errors.New("timelock claim window exhausted; asset leg NOT funded")

	// ErrXcCanceled means the caller cancelled the lift (context). The user asked
	// to stop waiting; nothing of ours has moved at that point.
	ErrXcCanceled = errors.New("cross-chain lift cancelled")

	// ErrBtcLegChanged means the counterparty's BTC leg no longer verifies the
	// way it did before we waited — it vanished (double-spent / reorged out) or
	// moved to a different height. Funding an asset leg against it would be the
	// classic one-sided loss, so we refuse.
	ErrBtcLegChanged = errors.New("btc leg changed during the anchor wait; asset leg NOT funded")
)

// maxFundWaitReadFailures bounds a wait against a DEAD node rather than against a
// slow chain: if we cannot read our own anchor at all, we could not fund anyway.
// At the 5s default poll that is ~5 minutes of an unreachable node.
const maxFundWaitReadFailures = 60

// maxCounterpartyHeightSlack caps how far a COUNTERPARTY-REPORTED BTC-leg height
// may raise our wait target above the height our own node verified. The gap is
// only ever the claimant's non-atomic tip/confirmations derivation plus a little
// courier indexing lag — a couple of parent blocks — so six is generous. See
// claimantBtcLegHeight: without a cap this field is a remote denial-of-service.
const maxCounterpartyHeightSlack = 6

// fundWindow is the PROTOCOL deadline an asset funder's wait is bounded by, in
// place of a wall-clock constant (owner ruling, Andreas 2026-07-25: "we should
// let users decide if the wait is intolerable and they want to cancel the trade
// [...] We cannot really predict how long contested blocks will take to clear").
//
// Waiting is free right up until the point where funding would no longer leave a
// real claim window — past that the swap can only end in a stranded leg, so the
// wait stops THERE and nowhere else. Both bounds are read off the chains, not a
// clock, so a slow chain moves the deadline with it.
type fundWindow struct {
	SeqLocktime       uint32 // T_seq: when the asset leg's refund path opens
	MinSeqClaimWindow uint32 // SEQ blocks that must remain after funding for the claim to land
	BtcLocktime       uint32 // T_btc: when the BTC leg's refund path opens (0 = not read here)
	MinBtcClaimWindow uint32 // parent blocks that must remain for the BTC-side claim (0 = does not bind)
}

// check reports why funding is no longer safe, or nil while it still is. A chain
// read that FAILS is not a verdict: it returns nil (keep waiting) and the caller's
// consecutive-failure counter handles a genuinely dead node.
func (w fundWindow) check(ops XcOps) error {
	if w.SeqLocktime > 0 && w.MinSeqClaimWindow > 0 {
		if seqTip, err := ops.SeqTip(); err == nil && uint32(seqTip)+w.MinSeqClaimWindow >= w.SeqLocktime {
			return fmt.Errorf("%w: SEQ tip %d is within %d blocks of T_seq %d, so a leg funded now could not be claimed in time",
				ErrFundWindowClosed, seqTip, w.MinSeqClaimWindow, w.SeqLocktime)
		}
	}
	if w.BtcLocktime > 0 && w.MinBtcClaimWindow > 0 {
		if btcTip, err := ops.BtcTip(); err == nil && uint32(btcTip)+w.MinBtcClaimWindow >= w.BtcLocktime {
			return fmt.Errorf("%w: BTC tip %d is within %d blocks of T_btc %d, so the BTC side could no longer be claimed",
				ErrFundWindowClosed, btcTip, w.MinBtcClaimWindow, w.BtcLocktime)
		}
	}
	return nil
}

// claimWindow is fundWindow's counterpart for the CLAIMANT side: it bounds the
// anchor gate by the protocol deadline instead of a wall clock.
//
// The same owner ruling applies here as to the funder's wait — no arbitrary
// wall-clock timeout may abort a HEALTHY wait, because nobody can predict how
// long a contested parent chain takes to clear. A flat budget is wrong in both
// directions at once: it gives up on a gate that is merely slow while hours of
// timelock remain, and it would happily keep waiting past the point where a
// claim could still land if the timelock were short.
//
// The real deadline is the one the protocol already enforces immediately after
// this gate: we must not still be here once the tip is within SeqClaimMargin of
// T_seq, because claiming then races the counterparty's refund. Waiting past
// that point cannot produce a usable result, so that is exactly where the gate
// stops — and the wait costs nothing before it, since the claimant has not
// revealed its secret and both legs remain refundable.
type claimWindow struct {
	SeqLocktime uint32 // T_seq: when the asset leg's refund path opens
	Margin      uint32 // SEQ blocks that must remain for our claim to land ahead of it
}

// closed reports why the claim can no longer land, or nil while it still can. As
// in fundWindow.check, a chain read that FAILS is not a verdict: it returns nil
// (keep waiting) and leaves node health to the caller's failure counter.
//
// A zero SeqLocktime or Margin means the caller did not supply the timelock. That
// is a programming error, not a licence to spin forever, so it is reported as
// closed rather than silently restoring the unbounded behaviour this type exists
// to remove.
func (w claimWindow) closed(ops XcOps) error {
	if w.SeqLocktime == 0 || w.Margin == 0 {
		return fmt.Errorf("%w: the anchor gate was given no T_seq to bound it (SeqLocktime=%d margin=%d); refusing to wait unbounded",
			ErrXcBadTerms, w.SeqLocktime, w.Margin)
	}
	seqTip, err := ops.SeqTip()
	if err != nil {
		return nil
	}
	if uint32(seqTip)+w.Margin >= w.SeqLocktime {
		return fmt.Errorf("%w: SEQ tip %d is within %d blocks of T_seq %d, so a claim made now would race the refund path",
			ErrFundWindowClosed, seqTip, w.Margin, w.SeqLocktime)
	}
	return nil
}

// waitSeqAnchorReachesBTCLeg is the ANCHOR PRECONDITION every asset FUNDER must
// clear BEFORE it commits the asset (forward maker, reverse taker). It blocks
// until our own node's live anchor view (getanchorstatus) has reached the height
// at which the BTC leg confirmed, with a healthy anchor status.
//
// Why here, and not later. The claimant's gate (xchain.VerifySeqLegSafe) requires
// the block that CONFIRMS this funding to commit anchorheight >= the BTC-leg
// height. That value is a committed header field: once the funding confirms, its
// fate is decided and no amount of waiting can change it. Anchors are monotone
// non-decreasing along a chain (consensus rule R2, "bad-anchor-not-monotonic"),
// so by waiting for the TIP's anchor to reach the BTC-leg height first, every
// block extending that tip — including the one that will confirm our funding —
// carries anchorheight >= the BTC-leg height, and the claimant's gate passes on
// its FIRST read. That is Andreas's invariant ("BTC leg first implies the
// Sequentia leg anchors at or above the BTC-leg height") turned from a hope into
// a precondition we enforce.
//
// Waiting HERE is free: nothing is committed, so aborting costs nothing but the
// swap. Waiting AFTER funding is futile AND expensive: the value is already at
// risk and the only exit burns both timelock windows.
//
// This does NOT relax the claimant's gate by a single character, and it must
// never be replaced by "consult a burying block / the tip at claim time": a
// burying block's anchor says nothing about whether the funding OUTPUT survives a
// Bitcoin reorg (invalidation propagates to descendants, never to ancestors).
//
// HOW LONG IT WAITS. Not a wall-clock constant — the owner ruled that out
// (Andreas 2026-07-25): "we should let users decide if the wait is intolerable
// and they want to cancel the trade (putting the makers order back to rest),
// rather than cancel it automatically. We cannot really predict how long
// contested blocks will take to clear anyway." So the only automatic stops are
//  1. the PROTOCOL deadline (fundWindow): past it, funding could not be claimed;
//  2. ctx cancellation — the user's own decision, wired here so the wallet's
//     cancel button needs no further refactor;
//  3. our own node being unreadable for maxFundWaitReadFailures polls, which is
//     an RPC-health bound, not a judgement about the chain.
//
// `target` is the height our anchor must REACH, already carrying the +1 slack the
// caller computes (see the call sites): waiting longer is never unsafe, so the
// funder absorbs the claimant's height-derivation race rather than risking a
// refusal that would strand the leg.
func waitSeqAnchorReachesBTCLeg(ctx context.Context, ops XcOps, target int64, window fundWindow,
	timing XcTiming, logf func(string, ...interface{})) error {
	announced := false
	readFails := 0
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("%w while waiting for our anchor to reach %d (asset leg NOT funded; nothing of ours was spent)", ErrXcCanceled, target)
		}
		anchorH, status, err := ops.SeqAnchorTip()
		if err != nil {
			readFails++
			if readFails >= maxFundWaitReadFailures {
				return fmt.Errorf("%w: our node's anchor view was unreadable for %d consecutive polls (target height %d): %v",
					ErrSeqAnchorBehindBTC, readFails, target, err)
			}
		} else {
			readFails = 0
			if status == "ok" && anchorH >= target {
				if announced && logf != nil {
					logf("anchor precondition met: our anchor is %d >= %d; funding the asset leg", anchorH, target)
				}
				return nil
			}
		}
		// The ONE automatic stop: the timelock, read off the chains.
		if werr := window.check(ops); werr != nil {
			return fmt.Errorf("%w (our anchor was %d, status %q, needed %d)", werr, anchorH, status, target)
		}
		if !announced && logf != nil {
			logf("anchor precondition: our anchor is %d (status %q); waiting for >= %d before funding the asset leg. "+
				"This waits as long as the timelock allows (T_seq %d, keeping %d blocks for the claim) — cancel the trade if that is too long.",
				anchorH, status, target, window.SeqLocktime, window.MinSeqClaimWindow)
			announced = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w while waiting for our anchor to reach %d (asset leg NOT funded; nothing of ours was spent)", ErrXcCanceled, target)
		case <-time.After(timing.Poll):
		}
	}
}

// claimantBtcLegHeight is the height a claimant will measure this BTC leg at, and
// the funder's wait target is one ABOVE it. Two independent reasons for the slack:
//
//   - the claimant derives its height from two non-atomic RPCs (tip, confirmations);
//     a parent block landing between them yields a height one too HIGH, and the gate
//     then fails on a leg that was actually fine;
//   - the counterparty's own reported height (courier data) may be one ahead of what
//     our node has indexed.
//
// Waiting is safety-monotone — a HIGHER anchor never weakens the ordering guarantee —
// so the funder absorbs the race instead of the claimant refusing mid-swap. `ours` is
// our own node's verification (authoritative); `theirs` is the courier's claim, used
// only to raise the bar, never to lower it.
//
// ⚠ `theirs` IS COUNTERPARTY-CONTROLLED and is therefore CLAMPED. It arrives as
// Leg.Height in a courier message, so a hostile peer can put any number in it.
// Left unbounded it is a denial-of-service on the funder: reporting a height of,
// say, 2^62 sets a wait target no anchor will ever reach, and the funder then
// waits out its entire timelock for nothing. Since `ours` comes from our own
// node's verification of the very same leg, the true height IS `ours`; `theirs`
// exists only to absorb the claimant's non-atomic derivation and a little courier
// indexing lag, both of which are a few blocks at most. Anything beyond
// maxCounterpartyHeightSlack is not lag, it is a bogus report, so the bar is
// raised to the slack limit and no further. Clamping rather than rejecting keeps
// a merely-confused peer workable while denying a malicious one the DoS.
func claimantBtcLegHeight(ours, theirs int64) int64 {
	limit := ours + maxCounterpartyHeightSlack
	if theirs > limit {
		return limit
	}
	if theirs > ours {
		return theirs
	}
	return ours
}

// btcLegConfirmedHeight derives a BTC leg's confirmation height ATOMICALLY enough
// to be trusted: tip - confirmations + 1 is only meaningful if no block lands
// between the two reads, so the confirmation count is read on BOTH sides of the
// tip read and the result is used only when it did not move. Without this a
// parent block arriving mid-derivation silently inflates the height by one, and
// the claimant then demands an anchor the funder was never told to reach.
func btcLegConfirmedHeight(ops XcOps, txid string, minConf int) (height int64, confs int, err error) {
	for attempt := 0; attempt < 5; attempt++ {
		before, cerr := ops.BtcConfirmations(txid)
		if cerr != nil {
			return 0, 0, cerr
		}
		if before < minConf {
			return 0, before, fmt.Errorf("btc leg %s has %d confirmations, need %d", txid, before, minConf)
		}
		tip, terr := ops.BtcTip()
		if terr != nil {
			return 0, before, terr
		}
		after, cerr := ops.BtcConfirmations(txid)
		if cerr != nil {
			return 0, before, cerr
		}
		if after == before {
			return tip - int64(before) + 1, before, nil
		}
		// A block landed mid-read; retry so the two halves agree.
	}
	return 0, 0, fmt.Errorf("btc leg %s: confirmation count kept moving; could not derive its height consistently", txid)
}

// atomsToCoins renders atoms as an 8-decimal coin string for wallet RPCs.
func atomsToCoins(a uint64) string { return fmt.Sprintf("%d.%08d", a/100_000_000, a%100_000_000) }

// xcSafeFee clamps a flat sat fee to half the leg so the output stays positive.
func xcSafeFee(spendFee, legAmount uint64) uint64 {
	if max := legAmount / 2; spendFee > max {
		return max
	}
	return spendFee
}

// xcSeqLegFee sizes the fee for spending a SEQ-side leg IN THE LEG'S OWN asset:
// the flat native-sats target converted through the open-fee-market exchange rate
// (asset_atoms = ceil(spendFee*1e8/rate)), exactly like the RFQ watcher and the
// wallet do — a valuable asset correctly pays FEWER atoms. Falls back to the flat
// target when no rate is published; clamped to half the leg.
func xcSeqLegFee(ops XcOps, assetHex string, spendFee, legAmount uint64) uint64 {
	fee := spendFee
	if rate, ok := ops.SeqFeeRate(assetHex); ok && rate > 0 {
		const scale = 100_000_000
		fee = (spendFee*scale + rate - 1) / rate
		if fee == 0 {
			fee = 1
		}
	}
	return xcSafeFee(fee, legAmount)
}

// sendXc seals and sends one message; sendXcFail is its best-effort error twin.
func sendXc(m *XcMsg, c *Crypter, send XcSend) error {
	sealed, err := m.Seal(c)
	if err != nil {
		return err
	}
	return send(sealed)
}

func sendXcFail(c *Crypter, send XcSend, code, msg string) {
	if sealed, err := failMsg(code, msg).Seal(c); err == nil {
		_ = send(sealed)
	}
}

// recvXcType receives, opens, and filters courier frames until one of the wanted
// type arrives, the peer fails the session, or the deadline passes. Unknown or
// out-of-order message types are skipped (the courier is at-least-once-ish and a
// future peer may add courtesy messages).
// errXcRecvWindowElapsed marks "this recv window went by with nothing in it",
// as opposed to a broken courier. Only the former may be re-armed by a caller
// whose protocol deadline is still open (see recvXcTypeWhile).
var errXcRecvWindowElapsed = errors.New("courier window elapsed with no message")

func recvXcType(recv XcRecv, c *Crypter, want XcMsgType, wait time.Duration) (*XcMsg, error) {
	deadline := time.Now().Add(wait)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil, fmt.Errorf("%w: %q", errXcRecvWindowElapsed, want)
		}
		sealed, err := recv(remain)
		if err != nil {
			return nil, fmt.Errorf("courier recv while waiting for %q: %w", want, err)
		}
		m, err := OpenXcMsg(sealed, c)
		if err != nil {
			// An undecryptable frame is at most relay noise/injection (the relay
			// can always drop or garble; killing the session here would hand it
			// MORE power than dropping). Skip it; the deadline bounds the loop.
			continue
		}
		switch m.Type {
		case want:
			return m, nil
		case XcFail:
			return nil, fmt.Errorf("%w: %s: %s", ErrXcPeerFailed, m.Code, m.Message)
		default:
			continue
		}
	}
}

// recvXcTypeWhile waits for `want` for as long as `stillWorthWaiting` says the
// PROTOCOL allows, using `step` as the size of one courier window rather than as
// the deadline of the whole wait.
//
// Why not just a bigger timeout: the peer on the other side may be legitimately
// parked in its own anchor precondition, a wait nobody can put a number on
// (owner ruling 2026-07-25). Cutting it off with a flat constant refunds a swap
// that was about to succeed and leaves the funder's asset locked until T_seq. So
// an EMPTY window is re-armed; only a broken courier or a closed claim window
// ends the wait.
//
// Classifying a recv failure: the caller's recv reports its own idle timeout as
// an error too, so "the window went by quietly" and "the socket died" arrive the
// same way. They are told apart by WHEN: an idle window returns at the end of its
// budget, a dead transport returns straight away. Anything that comes back in
// well under the budget is treated as a real transport failure and aborts.
func recvXcTypeWhile(recv XcRecv, c *Crypter, want XcMsgType, step time.Duration,
	stillWorthWaiting func() error, logf func(string, ...interface{})) (*XcMsg, error) {
	if step <= 0 {
		step = time.Minute
	}
	for {
		if err := stillWorthWaiting(); err != nil {
			return nil, err
		}
		started := time.Now()
		m, err := recvXcType(recv, c, want, step)
		if err == nil {
			return m, nil
		}
		if errors.Is(err, ErrXcPeerFailed) {
			return nil, err
		}
		idled := errors.Is(err, errXcRecvWindowElapsed) || time.Since(started) >= step*3/4
		if !idled {
			return nil, err // the courier itself is broken; waiting cannot help
		}
		if logf != nil {
			logf("still waiting for %q after %s (the peer may be waiting for its own anchor to catch up); "+
				"this continues while the timelock leaves a claim window — cancel the trade to stop it", want, step)
		}
	}
}

// gateSeqLeg is THE claimant-side anchor gate, shared by every side that is about
// to treat an asset leg as claimable. It re-derives the leg's confirming block on
// EVERY iteration instead of trusting one captured at verification time.
//
// Why re-derive. getblockheader answers for ORPHANED blocks too — it reads the
// header index, not the chain — so a cached hash keeps returning a verdict about
// a block that may no longer be part of anything. Two failure modes follow from
// caching: a Sequentia reorg that re-mines the leg into a BETTER-anchored block
// stays invisible (we refuse a leg that is now fine), and one that re-mines it
// into a worse-anchored block leaves us reading the stale, rosier header. So each
// pass asks the node which block confirms this txid NOW, insists that block is on
// the ACTIVE chain, and only then reads its committed anchorheight.
//
// The gate itself is untouched: anchor >= btcLegHeight AND anchorstatus ok AND
// (certification absent OR certified), read off the block that CONFIRMED the
// funding — never a burying block, whose anchor says nothing about whether the
// funding output survives a Bitcoin reorg.
//
// HOW LONG IT WAITS. By the TIMELOCK, never by a clock — see claimWindow. The
// only stops are the protocol deadline (the tip reaching T_seq minus the claim
// margin), ctx cancellation, and a terminal ordering verdict. Nothing of ours is
// committed while this runs: the secret is unrevealed and both legs stay
// refundable, so a long wait costs only the trade, and the decision to abandon a
// slow one belongs to the user.
func gateSeqLeg(ctx context.Context, ops XcOps, legTxid string, btcLegHeight int64,
	window claimWindow, timing XcTiming, logf func(string, ...interface{})) (*xchain.AnchorEvidence, error) {
	var lastErr error
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w while running the anchor gate", ErrXcCanceled)
		}
		blockHash, berr := ops.SeqBlockHashOfTx(legTxid)
		if berr == nil && blockHash != "" {
			onChain, cerr := ops.SeqBlockOnActiveChain(blockHash)
			switch {
			case cerr != nil:
				lastErr = fmt.Errorf("could not confirm block %s is on the active chain: %w", blockHash, cerr)
			case !onChain:
				// The leg is being re-mined: its old block is orphaned and the header
				// it committed is meaningless now. Wait for the new one.
				lastErr = fmt.Errorf("the block %s holding the asset leg is no longer on the active chain (the leg is being re-mined)", blockHash)
			default:
				ev, gerr := ops.VerifySeqLegSafe(blockHash, btcLegHeight)
				if gerr == nil {
					return ev, nil
				}
				// The ordering conjunct reads a COMMITTED header field of the block we
				// just re-derived, so for that block it can never clear: fail fast and
				// keep the whole refund window instead of re-reading one number.
				if errors.Is(gerr, xchain.ErrAnchorOrderingTerminal) {
					return ev, gerr
				}
				lastErr = gerr
			}
		} else if berr != nil {
			lastErr = fmt.Errorf("asset leg %s not confirmed yet: %w", legTxid, berr)
		} else {
			lastErr = fmt.Errorf("asset leg %s has no confirming block yet", legTxid)
		}
		if werr := window.closed(ops); werr != nil {
			return nil, fmt.Errorf("%w (last anchor-gate reason: %v)", werr, lastErr)
		}
		if logf != nil {
			logf("anchor gate not satisfied yet (%v); re-reading the leg's confirming block. "+
				"This continues while T_seq %d leaves a claim window — cancel the trade to stop it",
				lastErr, window.SeqLocktime)
		}
		time.Sleep(timing.Poll)
	}
}

// --- FORWARD taker -----------------------------------------------------------

// TakerForwardParams configures RunTakerForward. Expectations come from the
// SIGNED offer the taker verified before lifting; the courier peer and relay are
// untrusted beyond it.
type TakerForwardParams struct {
	Ops     XcOps
	Crypter *Crypter

	// Ctx cancels the lift. The only waits it interrupts are ones where nothing
	// of ours is committed yet (or is already recoverable), so cancelling is the
	// user's call to make and never loses funds — the wallet's "cancel this trade"
	// button wires straight to it. nil == context.Background().
	Ctx context.Context

	Secret       []byte      // 32-byte preimage; Ops' hashlock must be built from it
	BtcRefundKey *xchain.Key // refunds our BTC leg after T_btc
	SeqClaimKey  *xchain.Key // claims the SEQ leg (the claim reveals Secret on-chain)

	ExpectAsset     string // SEQ asset hex the offer sells (required)
	ExpectSeqAmount uint64 // atoms the WHOLE signed offer promises (required)
	ExpectBtcAmount uint64 // sats the WHOLE signed offer wants (required)
	MaxFeeBtc       uint64 // refuse terms whose fee_btc exceeds this

	// TakeSeqAmount is the slice of the offer to take (asset atoms). 0 (or a value
	// >= ExpectSeqAmount) takes the WHOLE offer, reducing to the classic whole-HTLC
	// lift. For a partial (0 < TakeSeqAmount < ExpectSeqAmount) the BTC leg is sized
	// to ProportionalBtc(ExpectBtcAmount, TakeSeqAmount, ExpectSeqAmount) — ceil, in
	// the maker's favour, bound to the SIGNED offer's ratio so the maker cannot quote
	// a worse price — and the maker's SEQ leg must be exactly TakeSeqAmount.
	TakeSeqAmount uint64

	// Locktime sanity. T_btc bounds how long our BTC can be locked before the
	// refund path opens; T_seq must leave us a real window to claim after the
	// anchor gate, and we must never reveal the secret when the maker's refund
	// path is about to open (it could race our claim with its refund). The
	// window must cover a REAL parent confirmation (median ~10 min, long tail)
	// plus the maker's SEQ lock (~3 min worst) plus the anchor gate, in ~30 s
	// SEQ slots — hence the 120-slot (~1 h) default; a maker quoting less is
	// refused BEFORE any BTC moves.
	MaxBtcLockDelta   uint32 // default 400 parent blocks
	MinSeqClaimWindow uint32 // default 120 SEQ blocks at terms time
	SeqClaimMargin    uint32 // default 10 SEQ blocks: refuse to claim closer than this to T_seq

	MinBTCConf   int    // confirmations before announcing our BTC leg (default 1)
	SpendFeeSats uint64 // fee target in native sats (default 1000)
	Timing       XcTiming
	Log          func(format string, args ...interface{})

	// OnBtcLegFunded is invoked the moment the BTC leg is funded, BEFORE the
	// confirmation wait: the caller must persist the result's leg, locktime,
	// and its own keys/secret there, so a crash during the (potentially long)
	// wait never strands coins behind an unknowable redeem script.
	OnBtcLegFunded func(*TakerForwardResult)
}

// TakerForwardResult is returned even alongside an error once the BTC leg is
// funded, so the caller can persist it and refund after T_btc.
type TakerForwardResult struct {
	Terms        *XcMsg
	BtcLeg       *xchain.LegLock
	BtcLegHeight int64
	BtcLocktime  uint32
	SeqLeg       *xchain.LegLock
	SeqClaimTxid string
	Evidence     *xchain.AnchorEvidence
	// FilledSeq / FilledBtc are the asset atoms taken and the BTC sats locked for
	// this lift (== the whole offer for a whole-HTLC lift; a smaller slice + its
	// proportional BTC for a partial). The caller persists/reports these.
	FilledSeq uint64
	FilledBtc uint64
}

func (p *TakerForwardParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunTakerForward executes the forward handshake as the taker: request terms,
// fund + confirm the BTC leg, announce it, verify the maker's SEQ leg through
// the anchor gate, and claim it (revealing the secret). The maker then extracts
// the secret from our claim on-chain; no further courier messages are needed.
func RunTakerForward(p TakerForwardParams, send XcSend, recv XcRecv) (*TakerForwardResult, error) {
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil || len(p.Secret) != 32 || p.BtcRefundKey == nil || p.SeqClaimKey == nil {
		return nil, errors.New("taker forward: incomplete params")
	}
	if p.ExpectAsset == "" || p.ExpectSeqAmount == 0 || p.ExpectBtcAmount == 0 {
		return nil, errors.New("taker forward: offer expectations required")
	}
	if p.MaxBtcLockDelta == 0 {
		p.MaxBtcLockDelta = 400
	}
	if p.MinSeqClaimWindow == 0 {
		p.MinSeqClaimWindow = 120
	}
	if p.SeqClaimMargin == 0 {
		p.SeqClaimMargin = 10
	}
	if p.MinBTCConf <= 0 {
		p.MinBTCConf = 1
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	if p.Ctx == nil {
		p.Ctx = context.Background()
	}
	res := &TakerForwardResult{}
	hashH := sha256.Sum256(p.Secret)

	// 1. Ask for per-lift terms.
	if err := sendXc(&XcMsg{Type: XcTermsRequest}, p.Crypter, send); err != nil {
		return res, err
	}
	terms, err := recvXcType(recv, p.Crypter, XcTerms, p.Timing.TermsWait)
	if err != nil {
		return res, err
	}
	res.Terms = terms
	res.BtcLocktime = terms.BtcLocktime

	// 2. Bind the terms to the signed offer and sanity-check the locktimes.
	if terms.BtcAmount != p.ExpectBtcAmount {
		sendXcFail(p.Crypter, send, "terms_mismatch", "btc_amount differs from the signed offer")
		return res, fmt.Errorf("%w: btc_amount %d != offer %d", ErrXcBadTerms, terms.BtcAmount, p.ExpectBtcAmount)
	}
	if terms.SeqAmount != p.ExpectSeqAmount {
		sendXcFail(p.Crypter, send, "terms_mismatch", "seq_amount differs from the signed offer")
		return res, fmt.Errorf("%w: seq_amount %d != offer %d", ErrXcBadTerms, terms.SeqAmount, p.ExpectSeqAmount)
	}
	// Partial fills: the maker quotes the WHOLE offer in terms (verified above); we
	// take a slice of it. The BTC we lock is the PROPORTIONAL price of that slice at
	// the SIGNED offer's own ratio (ceil, in the maker's favour), computed from OUR
	// verified offer values so a maker can never quote a worse ratio. takeSeq==0 (or
	// >= the whole) is the classic whole-HTLC lift; fundBtc then == ExpectBtcAmount.
	takeSeq := p.TakeSeqAmount
	if takeSeq == 0 {
		takeSeq = p.ExpectSeqAmount
	}
	if takeSeq > p.ExpectSeqAmount {
		sendXcFail(p.Crypter, send, "terms_mismatch", "take exceeds the offer")
		return res, fmt.Errorf("%w: take %d exceeds the offer's %d", ErrXcBadTerms, takeSeq, p.ExpectSeqAmount)
	}
	fundBtc := ProportionalBtc(p.ExpectBtcAmount, takeSeq, p.ExpectSeqAmount)
	if fundBtc == 0 {
		return res, fmt.Errorf("%w: take %d of %d prices to 0 sats (dust)", ErrXcBadTerms, takeSeq, p.ExpectSeqAmount)
	}
	// Minimum-slice guard: a partial must never fund a BTC leg (or accept an asset
	// leg) so small that, after the HTLC spend fee, its claim/refund output is sub-
	// dust and unclaimable. Fail CLOSED here, BEFORE LockBTCLeg moves any coins; a
	// whole take is exempt. The BTC leg is ours to fund; the asset leg is the one we
	// will have to CLAIM, so guard both.
	if err := minSafeBtcErr(takeSeq, p.ExpectSeqAmount, fundBtc, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}
	if err := minSafeAssetErr(p.Ops, p.ExpectAsset, takeSeq, p.ExpectSeqAmount, takeSeq, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}
	res.FilledSeq, res.FilledBtc = takeSeq, fundBtc
	if terms.FeeBtc > p.MaxFeeBtc {
		sendXcFail(p.Crypter, send, "terms_mismatch", "fee_btc exceeds the taker bound")
		return res, fmt.Errorf("%w: fee_btc %d > max %d", ErrXcBadTerms, terms.FeeBtc, p.MaxFeeBtc)
	}
	makerBtcClaimPub, err := hex.DecodeString(terms.MakerBtcClaimPub)
	if err != nil || len(makerBtcClaimPub) != 33 {
		return res, fmt.Errorf("%w: bad maker_btc_claim_pub", ErrXcBadTerms)
	}
	makerSeqRefundPub, err := hex.DecodeString(terms.MakerRefundPub)
	if err != nil || len(makerSeqRefundPub) != 33 {
		return res, fmt.Errorf("%w: bad maker_refund_pub", ErrXcBadTerms)
	}
	btcTip, err := p.Ops.BtcTip()
	if err != nil {
		return res, err
	}
	seqTip, err := p.Ops.SeqTip()
	if err != nil {
		return res, err
	}
	if terms.BtcLocktime <= uint32(btcTip) || terms.BtcLocktime > uint32(btcTip)+p.MaxBtcLockDelta {
		sendXcFail(p.Crypter, send, "terms_mismatch", "btc_locktime out of bounds")
		return res, fmt.Errorf("%w: btc_locktime %d vs tip %d (max delta %d)", ErrXcBadTerms, terms.BtcLocktime, btcTip, p.MaxBtcLockDelta)
	}
	if terms.SeqLocktime < uint32(seqTip)+p.MinSeqClaimWindow {
		sendXcFail(p.Crypter, send, "terms_mismatch", "seq_locktime leaves no claim window")
		return res, fmt.Errorf("%w: seq_locktime %d vs tip %d (min window %d)", ErrXcBadTerms, terms.SeqLocktime, seqTip, p.MinSeqClaimWindow)
	}

	// 3. Fund the BTC leg and wait out our own confirmation: on a live parent
	// LockBTCLeg is broadcast-only (Hp=0) and the maker will reject an
	// unconfirmed leg, so we only announce once MinBTCConf is reached, with the
	// confirmation height the anchor gate will be measured against.
	p.logf("locking BTC leg: %d sats (take %d of %d %s), T_btc=%d", fundBtc, takeSeq, p.ExpectSeqAmount, p.ExpectAsset, terms.BtcLocktime)
	btcLeg, hp, err := p.Ops.LockBTCLeg(makerBtcClaimPub, p.BtcRefundKey.PubKey(), atomsToCoins(fundBtc), terms.BtcLocktime)
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_lock_failed", err.Error())
		return res, err
	}
	res.BtcLeg = btcLeg
	if p.OnBtcLegFunded != nil {
		p.OnBtcLegFunded(res)
	}
	if hp <= 0 {
		confDeadline := time.Now().Add(p.Timing.BtcConfWait)
		for {
			// Derived ATOMICALLY (confirmations either side of the tip read): a
			// parent block landing between the two RPCs used to inflate hp by one,
			// and the maker was then never told to anchor that high — the gate below
			// failed on a leg that was perfectly fine.
			h, _, herr := btcLegConfirmedHeight(p.Ops, btcLeg.Funded.TxID, p.MinBTCConf)
			if herr == nil {
				hp = h
				break
			}
			if time.Now().After(confDeadline) {
				sendXcFail(p.Crypter, send, "btc_conf_timeout", "btc leg did not confirm in time")
				return res, fmt.Errorf("btc leg %s: no %d-conf within %s (refund after T_btc %d)",
					btcLeg.Funded.TxID, p.MinBTCConf, p.Timing.BtcConfWait, terms.BtcLocktime)
			}
			time.Sleep(p.Timing.Poll)
		}
	}
	res.BtcLegHeight = hp
	p.logf("BTC leg %s confirmed at height %d", btcLeg.Funded.TxID, hp)

	// 4. Announce the leg.
	announce := &XcMsg{
		Type:              XcBtcLegFunded,
		HashH:             hex.EncodeToString(hashH[:]),
		TakerSeqClaimPub:  hex.EncodeToString(p.SeqClaimKey.PubKey()),
		TakerBtcRefundPub: hex.EncodeToString(p.BtcRefundKey.PubKey()),
		SeqAmount:         takeSeq, // the slice we're taking (maker locks exactly this)
		BtcAmount:         fundBtc, // the proportional BTC we locked (== the funded leg amount)
		Leg: &XcLeg{
			Txid:         btcLeg.Funded.TxID,
			Vout:         btcLeg.Funded.Vout,
			Amount:       btcLeg.Funded.Amount,
			RedeemScript: hex.EncodeToString(btcLeg.Script),
			Locktime:     terms.BtcLocktime,
			Height:       hp,
		},
	}
	if err := sendXc(announce, p.Crypter, send); err != nil {
		return res, err
	}

	// 5. Await + verify the maker's SEQ leg. The redeem script is re-derived
	// byte-for-byte by VerifySEQLeg (claim = our key, refund = the maker's from
	// terms), and the amount/asset are bound to the signed offer.
	//
	// HOW LONG WE WAIT for it: not a flat number of minutes. The maker is very
	// likely parked in its own anchor precondition — waiting for its node's anchor
	// to reach the height our BTC leg confirmed at — and that wait has no
	// predictable length (owner ruling 2026-07-25). Cutting it off would abandon a
	// swap that is about to succeed and leave the maker's asset locked to T_seq for
	// nothing. So we keep listening while our own claim window is still open, and
	// stop at the PROTOCOL deadline: once the SEQ tip is within SeqClaimMargin of
	// T_seq we could not safely claim anyway, and the BTC refund at T_btc is the
	// exit. Ctx cancellation (the user's decision) stops it at any time.
	locked, err := recvXcTypeWhile(recv, p.Crypter, XcSeqLegLocked, p.Timing.SeqLockWait, func() error {
		if p.Ctx.Err() != nil {
			return fmt.Errorf("%w while waiting for the maker's asset leg (BTC refundable after T_btc %d)", ErrXcCanceled, terms.BtcLocktime)
		}
		seqNow, terr := p.Ops.SeqTip()
		if terr != nil {
			return nil // a read hiccup is not a verdict; keep listening
		}
		if uint32(seqNow)+p.SeqClaimMargin >= terms.SeqLocktime {
			return fmt.Errorf("no asset leg by the time the claim window closed: SEQ tip %d is within %d of T_seq %d (BTC refundable after T_btc %d)",
				seqNow, p.SeqClaimMargin, terms.SeqLocktime, terms.BtcLocktime)
		}
		return nil
	}, p.logf)
	if err != nil {
		return res, err
	}
	if locked.Leg == nil {
		return res, errors.New("seq_leg_locked without a leg")
	}
	// The maker must lock exactly the slice we asked for (takeSeq), at the asset the
	// signed offer names. Binding to takeSeq (not the whole offer) is what makes the
	// partial fund-safe: we paid the proportional BTC for takeSeq, and we accept the
	// SEQ leg only if it delivers exactly takeSeq of the right asset.
	if locked.Leg.Amount != takeSeq || locked.Leg.Asset != p.ExpectAsset {
		sendXcFail(p.Crypter, send, "seq_leg_mismatch", "amount/asset differ from the taken slice")
		return res, fmt.Errorf("seq leg %d %s != taken %d %s", locked.Leg.Amount, locked.Leg.Asset, takeSeq, p.ExpectAsset)
	}
	if locked.Leg.Locktime != terms.SeqLocktime {
		sendXcFail(p.Crypter, send, "seq_leg_mismatch", "locktime differs from terms")
		return res, fmt.Errorf("seq leg locktime %d != terms %d", locked.Leg.Locktime, terms.SeqLocktime)
	}
	script, err := hex.DecodeString(locked.Leg.RedeemScript)
	if err != nil {
		return res, fmt.Errorf("bad seq redeem_script hex: %w", err)
	}
	verified, err := p.Ops.VerifySEQLeg(hashH[:], p.SeqClaimKey.PubKey(), makerSeqRefundPub, script,
		terms.SeqLocktime, locked.Leg.Txid, locked.Leg.Vout, locked.Leg.Amount, locked.Leg.Asset, 1)
	if err != nil {
		sendXcFail(p.Crypter, send, "seq_leg_invalid", err.Error())
		return res, err
	}
	res.SeqLeg = verified.Leg

	// 6. Anchor gate: the SEQ block holding the leg must anchor at/above our BTC
	// confirmation height and the node's anchor must be healthy. The block is
	// SELF-DERIVED from our own node's view of the leg (and re-derived every pass,
	// see gateSeqLeg), NEVER the courier-supplied value: a malicious maker could
	// otherwise quote any well-anchored block for a leg that actually confirmed in
	// a badly anchored one, voiding the exact ordering guarantee this gate exists
	// for. The transient conjuncts (anchorstatus flap, not-yet-certified) are
	// retried; the ORDERING one is terminal for the block it was read from.
	ev, err := gateSeqLeg(p.Ctx, p.Ops, verified.Leg.Funded.TxID, hp,
		claimWindow{SeqLocktime: terms.SeqLocktime, Margin: p.SeqClaimMargin}, p.Timing, nil)
	if err != nil {
		if errors.Is(err, xchain.ErrAnchorOrderingTerminal) {
			return res, fmt.Errorf("anchor gate terminally failed (waiting cannot help): %w (BTC refundable after T_btc %d)", err, terms.BtcLocktime)
		}
		return res, fmt.Errorf("anchor gate did not pass while T_seq %d left a claim window: %w (BTC refundable after T_btc %d)",
			terms.SeqLocktime, err, terms.BtcLocktime)
	}
	res.Evidence = ev

	// 7. Claim window re-check: never reveal the secret when the maker's refund
	// path is about to open (it could race our claim). Abort WITHOUT revealing;
	// our BTC refund after T_btc is then the exit.
	seqTip2, err := p.Ops.SeqTip()
	if err != nil {
		return res, err
	}
	if uint32(seqTip2)+p.SeqClaimMargin >= terms.SeqLocktime {
		return res, fmt.Errorf("%w: seq tip %d within %d of T_seq %d; not revealing the secret",
			ErrXcBadTerms, seqTip2, p.SeqClaimMargin, terms.SeqLocktime)
	}

	// 8. Claim the SEQ leg (reveals the secret on-chain; the maker's watcher
	// extracts it and claims the BTC leg — the handshake is complete for us).
	fee := xcSeqLegFee(p.Ops, p.ExpectAsset, p.SpendFeeSats, locked.Leg.Amount)
	claimTxid, err := p.Ops.ClaimSEQLeg(verified.Leg, p.SeqClaimKey, fee)
	if err != nil {
		return res, fmt.Errorf("seq claim failed: %w (BTC refundable after T_btc %d)", err, terms.BtcLocktime)
	}
	res.SeqClaimTxid = claimTxid
	p.logf("claimed SEQ leg: %s (secret revealed)", claimTxid)
	return res, nil
}

// RefundTakerBTC spends the taker's BTC leg through the CLTV refund path once
// T_btc has passed. With wait=false it returns ErrXcRefundNotDue when early.
var ErrXcRefundNotDue = errors.New("btc refund locktime not yet reached")

func RefundTakerBTC(ops XcOps, leg *xchain.LegLock, key *xchain.Key, locktime uint32, spendFeeSats uint64, wait bool, poll time.Duration) (string, error) {
	if poll <= 0 {
		poll = 15 * time.Second
	}
	if spendFeeSats == 0 {
		spendFeeSats = 1000
	}
	for {
		tip, err := ops.BtcTip()
		if err != nil {
			return "", err
		}
		if uint32(tip) >= locktime {
			break
		}
		if !wait {
			return "", fmt.Errorf("%w: tip %d < T_btc %d", ErrXcRefundNotDue, tip, locktime)
		}
		time.Sleep(poll)
	}
	return ops.RefundBTCLeg(leg, key, locktime, xcSafeFee(spendFeeSats, leg.Funded.Amount))
}

// --- FORWARD maker -----------------------------------------------------------

// MakerForwardParams configures RunMakerForward. Amounts and the asset come from
// the maker's own SIGNED offer (whole-HTLC: the lift's take_amount must equal the
// offer's base amount; the caller enforces that before starting the driver).
type MakerForwardParams struct {
	// NewOps binds the settlement engine to the taker's hashlock once it arrives
	// (the maker never knows the secret; it builds from the hash).
	NewOps  func(hashH []byte) (XcOps, error)
	Crypter *Crypter

	// Ctx cancels the lift while nothing of ours is committed — in particular
	// during the anchor precondition below, which is deliberately not bounded by
	// any wall clock. The wallet's "cancel this trade and re-rest my order" button
	// wires here. nil == context.Background().
	Ctx context.Context

	// Tip queries usable BEFORE the hashlock exists (terms are minted first).
	BtcTip func() (int64, error)
	SeqTip func() (int64, error)

	AssetHex  string // SEQ asset we deliver (offer pair base)
	SeqAmount uint64 // atoms we lock
	BtcAmount uint64 // sats we require
	FeeBtc    uint64 // advisory fee surfaced in terms

	// Locktime deltas above the respective tips. T_btc must be LONGER IN TIME
	// than T_seq, and T_seq must leave the taker room for a REAL parent
	// confirmation before its claim: 180 parent blocks ~30 h vs 240 SEQ slots
	// ~2 h. (The RFQ's 50-slot delta was tuned for regtest; on a live parent a
	// taker with the default 120-slot minimum window rightly refuses it.)
	//
	// BtcLocktimeDelta is ALSO consumed by the LSP PAYER leg-bridge: it becomes
	// T_btc on the on-chain BTC HTLC the LSP funds, which checkPayerFundGate
	// (sequentia-web-wallet leg-bridge.mjs) gates against the taker's BTC-LN hold
	// under CONSERVATIVE divisors (slow-SEQ 90 s/blk, fast-BTC 150 s/blk):
	//   B1  T_btc >= tip + ceil(T_seq_blocks*90/150) + makerClaimRunway(6)
	//       => for T_seq=240 the floor is ~150 blocks (the old 100 was a wall-clock
	//       inversion under fast BTC and FAILED B1);
	//   B3  T_btc <= holdCLTV - holdBuffer, holdBuffer=18 (finality 6 + confirm 12);
	//       the taker's hold is ~requiredTakerHold(T_seq) ~210 blocks => ceiling ~192.
	// 180 sits comfortably in the [~150,~192] window (B1 margin ~30, B3 ~12),
	// clearing the payer fund gate for the honest fleet. Keep it in that window if
	// you change SeqLocktimeDelta or the LSP's holdBuffer.
	BtcLocktimeDelta uint32 // default 180 (payer-bridge safe window [~150,~192] for T_seq=240)
	SeqLocktimeDelta uint32 // default 240

	MinBTCConf   int    // confirmations required on the taker's BTC leg (default 1; testnet-grade — depth, not anchoring, protects the maker's BTC side)
	SpendFeeSats uint64 // fee target in native sats (default 1000)

	// MinSeqClaimWindow is the PROTOCOL bound on the anchor precondition below:
	// we will wait for our anchor to catch up for as long as T_seq still leaves
	// this many SEQ blocks for the taker to claim in, and not one block longer.
	// Default 120 — the same window the taker itself demanded of our terms, so
	// the two sides agree on when funding stops being safe. This replaces the old
	// flat wall-clock timeout, which the owner ruled out (2026-07-25).
	MinSeqClaimWindow uint32
	// MinBtcClaimWindow is the same idea on the parent chain: below this many
	// blocks before T_btc we could not claim the taker's BTC after the reveal, so
	// funding the asset would be one-sided. Default 12 (settleMakerForward gives
	// up at T_btc-6, so this leaves real room above it).
	MinBtcClaimWindow uint32

	Timing XcTiming
	Log    func(format string, args ...interface{})

	// OnUpdate is invoked after every state transition that creates or changes
	// recoverable value (keys minted, legs funded/verified, secret learned,
	// settled/refunded). The caller must persist the result snapshot there: the
	// per-lift keys exist nowhere else, and losing them mid-swap burns a leg.
	OnUpdate func(*MakerForwardResult)
}

// MakerForwardResult reports the lift's evolving state; with OnUpdate it is the
// maker's persistence record (the keys are the recovery material).
type MakerForwardResult struct {
	HashH        []byte
	BtcClaimKey  *xchain.Key // claims the taker's BTC leg once the secret is known
	SeqRefundKey *xchain.Key // refunds our SEQ leg after T_seq
	BtcLocktime  uint32
	SeqLocktime  uint32
	BtcLeg       *xchain.LegLock // the taker's verified BTC leg
	SeqLeg       *xchain.LegLock
	SeqBlockHash string
	Secret       []byte
	BtcClaimTxid string
	SeqRefundTx  string
	Settled      bool
	// FilledSeq / FilledBtc are the asset atoms and BTC sats this lift ACTUALLY took
	// (== the offer for a whole lift; a smaller slice + its proportional BTC for a
	// partial). The serve loop re-rests offer-minus-FilledSeq when they are smaller.
	FilledSeq uint64
	FilledBtc uint64
}

func (p *MakerForwardParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunMakerForward executes the forward handshake as the maker: mint per-lift
// terms, verify the taker's confirmed BTC leg, lock the SEQ leg, then watch for
// the taker's claim and redeem the BTC leg with the revealed secret — or refund
// the SEQ leg once T_seq passes without a claim. `in` delivers sealed courier
// frames for this session; the driver owns Open (first act) and Seal.
func RunMakerForward(p MakerForwardParams, in <-chan []byte, send XcSend) (*MakerForwardResult, error) {
	p.Timing.setDefaults()
	if p.NewOps == nil || p.Crypter == nil || p.BtcTip == nil || p.SeqTip == nil {
		return nil, errors.New("maker forward: incomplete params")
	}
	if p.AssetHex == "" || p.SeqAmount == 0 || p.BtcAmount == 0 {
		return nil, errors.New("maker forward: offer amounts required")
	}
	if p.BtcLocktimeDelta == 0 {
		p.BtcLocktimeDelta = 180 // payer-bridge safe window [~150,~192] for T_seq=240; see the field comment
	}
	if p.SeqLocktimeDelta == 0 {
		p.SeqLocktimeDelta = 240
	}
	if p.MinBTCConf <= 0 {
		p.MinBTCConf = 1
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	if p.MinSeqClaimWindow == 0 {
		p.MinSeqClaimWindow = 120
	}
	if p.MinBtcClaimWindow == 0 {
		p.MinBtcClaimWindow = 12
	}
	if p.Ctx == nil {
		p.Ctx = context.Background()
	}
	recv := func(timeout time.Duration) ([]byte, error) {
		select {
		case sealed, ok := <-in:
			if !ok {
				return nil, errors.New("session closed")
			}
			return sealed, nil
		case <-time.After(timeout):
			return nil, errors.New("courier timeout")
		}
	}
	res := &MakerForwardResult{}

	// 1. Terms request, then mint per-lift terms with FRESH keys (the offer's
	// resting keys are advisory/discovery only).
	if _, err := recvXcType(recv, p.Crypter, XcTermsRequest, p.Timing.TermsReqWait); err != nil {
		return res, err
	}
	makerBtcClaim, err := xchain.NewKey()
	if err != nil {
		return res, err
	}
	makerSeqRefund, err := xchain.NewKey()
	if err != nil {
		return res, err
	}
	btcTip, err := p.BtcTip()
	if err != nil {
		return res, err
	}
	seqTip, err := p.SeqTip()
	if err != nil {
		return res, err
	}
	btcLocktime := uint32(btcTip) + p.BtcLocktimeDelta
	seqLocktime := uint32(seqTip) + p.SeqLocktimeDelta
	res.BtcClaimKey, res.SeqRefundKey = makerBtcClaim, makerSeqRefund
	res.BtcLocktime, res.SeqLocktime = btcLocktime, seqLocktime
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	terms := &XcMsg{
		Type:             XcTerms,
		MakerBtcClaimPub: hex.EncodeToString(makerBtcClaim.PubKey()),
		MakerRefundPub:   hex.EncodeToString(makerSeqRefund.PubKey()),
		BtcLocktime:      btcLocktime,
		SeqLocktime:      seqLocktime,
		BtcAmount:        p.BtcAmount,
		SeqAmount:        p.SeqAmount,
		FeeBtc:           p.FeeBtc,
	}
	if err := sendXc(terms, p.Crypter, send); err != nil {
		return res, err
	}
	p.logf("terms sent: %d sats <- %d atoms %s, T_btc=%d T_seq=%d", p.BtcAmount, p.SeqAmount, p.AssetHex, btcLocktime, seqLocktime)

	// 2. The taker's confirmed BTC leg.
	funded, err := recvXcType(recv, p.Crypter, XcBtcLegFunded, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	if funded.Leg == nil {
		return res, errors.New("btc_leg_funded without a leg")
	}
	hashH, err := hex.DecodeString(funded.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "bad_hash", "hash_h must be 32 bytes hex")
		return res, errors.New("bad hash_h")
	}
	takerSeqClaimPub, err := hex.DecodeString(funded.TakerSeqClaimPub)
	if err != nil || len(takerSeqClaimPub) != 33 {
		sendXcFail(p.Crypter, send, "bad_pubkey", "taker_seq_claim_pub invalid")
		return res, errors.New("bad taker_seq_claim_pub")
	}
	takerBtcRefundPub, err := hex.DecodeString(funded.TakerBtcRefundPub)
	if err != nil || len(takerBtcRefundPub) != 33 {
		sendXcFail(p.Crypter, send, "bad_pubkey", "taker_btc_refund_pub invalid")
		return res, errors.New("bad taker_btc_refund_pub")
	}
	// Partial fills: the taker MAY take a slice. funded.SeqAmount is how much asset it
	// wants (0 = the whole offer, back-compat with older takers). It must fund the
	// PROPORTIONAL BTC at the offer's own ratio (ceil, in the maker's favour); we then
	// lock exactly that slice of SEQ and the serve loop re-rests the remainder. Never
	// over-deliver: reject a take larger than the offer before anything is locked.
	takeSeq := funded.SeqAmount
	if takeSeq == 0 {
		takeSeq = p.SeqAmount
	}
	if takeSeq > p.SeqAmount {
		sendXcFail(p.Crypter, send, "amount_too_large", "requested more asset than the offer")
		return res, fmt.Errorf("maker forward: take %d > offer %d", takeSeq, p.SeqAmount)
	}
	wantBtc := ProportionalBtc(p.BtcAmount, takeSeq, p.SeqAmount)
	if funded.Leg.Amount != wantBtc {
		sendXcFail(p.Crypter, send, "btc_leg_mismatch", "amount != proportional quote")
		return res, fmt.Errorf("btc leg amount %d != required %d (take %d of %d)", funded.Leg.Amount, wantBtc, takeSeq, p.SeqAmount)
	}
	ops, err := p.NewOps(hashH)
	if err != nil {
		return res, err
	}
	res.HashH = hashH
	// Minimum-slice guard: refuse to lock our asset leg (LockSEQLeg below) against a
	// partial whose BTC leg (the one we CLAIM) or asset leg (the one we FUND, then
	// refund after T_seq) would be sub-dust-after-fee and stranded. Fails CLOSED
	// before any coin moves; a whole take is exempt.
	if err := minSafeBtcErr(takeSeq, p.SeqAmount, wantBtc, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}
	if err := minSafeAssetErr(ops, p.AssetHex, takeSeq, p.SeqAmount, takeSeq, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}
	script, err := hex.DecodeString(funded.Leg.RedeemScript)
	if err != nil {
		return res, fmt.Errorf("bad btc redeem_script hex: %w", err)
	}
	// The taker announces the instant ITS node reports MinBTCConf; ours can lag
	// by propagation/indexing or run a stricter -min-btc-conf. Only a proven
	// INVALID leg is terminal; unconfirmed / not-yet-visible is polled out (the
	// RFQ path got the same tolerance from the taker retrying the RPC).
	var verifiedBtc *xchain.VerifiedBTCLeg
	verifyDeadline := time.Now().Add(p.Timing.SeqLockWait)
	for {
		verifiedBtc, err = ops.VerifyBTCLeg(hashH, makerBtcClaim.PubKey(), takerBtcRefundPub, script,
			btcLocktime, funded.Leg.Txid, funded.Leg.Vout, funded.Leg.Amount, p.MinBTCConf)
		if err == nil {
			break
		}
		if errors.Is(err, xchain.ErrBTCLegInvalid) || time.Now().After(verifyDeadline) {
			sendXcFail(p.Crypter, send, "btc_leg_invalid", err.Error())
			return res, err
		}
		time.Sleep(p.Timing.Poll)
	}
	res.BtcLeg = verifiedBtc.Leg
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	p.logf("taker BTC leg verified: %s (height %d)", funded.Leg.Txid, funded.Leg.Height)

	// 2b. ANCHOR PRECONDITION — the taker's gate, honoured BEFORE our asset moves.
	// The taker will refuse to claim unless the block confirming our funding
	// commits anchorheight >= the BTC-leg height, and that value is immutable once
	// the funding confirms. So wait for OUR OWN anchor to reach that height first:
	// R2 monotonicity then makes every block extending this tip satisfy the gate by
	// construction. Measured primarily against our own node's view of the leg
	// (verifiedBtc.Height); the courier-supplied funded.Leg.Height is allowed only
	// to RAISE the bar, never to lower it, and one block of slack absorbs the
	// taker's non-atomic height derivation. Waiting longer is never unsafe.
	//
	// How long: until the timelock says funding could no longer be claimed, or the
	// user cancels — never a flat number of minutes (owner ruling 2026-07-25).
	// Aborting here is clean: no asset has moved and the taker refunds at T_btc.
	btcLegH := claimantBtcLegHeight(verifiedBtc.Height, funded.Leg.Height)
	window := fundWindow{
		SeqLocktime:       seqLocktime,
		MinSeqClaimWindow: p.MinSeqClaimWindow,
		BtcLocktime:       btcLocktime,
		MinBtcClaimWindow: p.MinBtcClaimWindow,
	}
	if err := waitSeqAnchorReachesBTCLeg(p.Ctx, ops, btcLegH+1, window, p.Timing, p.logf); err != nil {
		sendXcFail(p.Crypter, send, "anchor_not_caught_up", "our anchor has not reached your BTC leg; asset leg not funded")
		return res, fmt.Errorf("%w (BTC-leg height %d; nothing of ours was spent)", err, btcLegH)
	}

	// 2c. RE-VERIFY THE BTC LEG. The wait above can be long, and a BTC HTLC that
	// was confirmed when we checked can be GONE by the time it ends: a one-block
	// parent reorg lets the taker double-spend the input it funded with. Funding
	// our asset leg against a dead BTC leg is the whole fund-loss case this file
	// exists to prevent, so the full verification runs again — same txid, vout,
	// amount, script, locktime, confirmations — immediately before LockSEQLeg. A
	// changed height means the leg was re-mined, which also invalidates the anchor
	// precondition we just satisfied, so that is a refusal too (fail closed; the
	// taker refunds at T_btc and nothing of ours moved).
	recheckBtc, err := ops.VerifyBTCLeg(hashH, makerBtcClaim.PubKey(), takerBtcRefundPub, script,
		btcLocktime, funded.Leg.Txid, funded.Leg.Vout, funded.Leg.Amount, p.MinBTCConf)
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_leg_gone", "your BTC leg no longer verifies; asset leg not funded")
		return res, fmt.Errorf("%w: re-verifying %s after the anchor wait: %v (nothing of ours was spent)",
			ErrBtcLegChanged, funded.Leg.Txid, err)
	}
	if recheckBtc.Height != verifiedBtc.Height {
		sendXcFail(p.Crypter, send, "btc_leg_gone", "your BTC leg moved to a different block; asset leg not funded")
		return res, fmt.Errorf("%w: %s was at height %d, now %d (nothing of ours was spent)",
			ErrBtcLegChanged, funded.Leg.Txid, verifiedBtc.Height, recheckBtc.Height)
	}
	// 2d. And the timelock re-check, read at the last possible moment: the wait
	// may have consumed the claim window right at its boundary.
	if werr := window.check(ops); werr != nil {
		sendXcFail(p.Crypter, send, "seq_window_closed", "the asset leg's claim window has run down; asset leg not funded")
		return res, fmt.Errorf("%w (nothing of ours was spent)", werr)
	}

	// 3. Lock the SEQ leg and announce it with its anchor evidence. A funded-
	// but-unconfirmed lock (LockSEQLeg's ~3 min budget on a stalled chain) is
	// NOT abandoned: the leg is persisted and its confirming block polled out,
	// so the coins are never stranded behind an unknowable script.
	seqLeg, seqBlockHash, err := ops.LockSEQLeg(takerSeqClaimPub, makerSeqRefund.PubKey(),
		atomsToCoins(takeSeq), p.AssetHex, seqLocktime)
	if seqLeg != nil {
		res.SeqLeg = seqLeg
		if p.OnUpdate != nil {
			p.OnUpdate(res)
		}
	}
	if err != nil {
		if seqLeg == nil {
			sendXcFail(p.Crypter, send, "seq_lock_failed", err.Error())
			return res, err
		}
		p.logf("SEQ leg %s funded but slow to confirm; polling: %v", seqLeg.Funded.TxID, err)
		confirmDeadline := time.Now().Add(p.Timing.SeqLockWait)
		for seqBlockHash == "" {
			if bh, berr := ops.SeqBlockHashOfTx(seqLeg.Funded.TxID); berr == nil && bh != "" {
				seqBlockHash = bh
				break
			}
			if time.Now().After(confirmDeadline) {
				sendXcFail(p.Crypter, send, "seq_lock_failed", "seq leg funded but unconfirmed; will refund after T_seq")
				return res, fmt.Errorf("seq leg %s funded but unconfirmed within %s (refund after T_seq %d)",
					seqLeg.Funded.TxID, p.Timing.SeqLockWait, seqLocktime)
			}
			time.Sleep(p.Timing.Poll)
		}
	}
	res.SeqBlockHash = seqBlockHash
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	// POST-FUNDING CHECK: does the block that actually confirmed our funding
	// satisfy the taker's gate? The precondition above makes it true by
	// construction on the honest path, but a Sequentia reorg can still land the
	// funding on a lower-anchored branch. If it did we withhold the announcement
	// and let the T_seq refund recover the leg.
	//
	// WHAT WITHHOLDING BUYS US — and what it does NOT. It is a COURTESY, not a
	// guarantee: the taker minted the secret and holds MakerRefundPub, so it can
	// rebuild this exact redeem script and find its P2SH on chain without any
	// message from us. Withholding only spares an HONEST taker a doomed claim (and
	// tells it plainly, via the XcFail, not to try). The ACTUAL defence against an
	// under-anchored asset leg is the PRE-FUNDING precondition at 2b — we do not
	// commit the asset until our anchor has passed the BTC-leg height — plus the
	// taker's own claimant gate, which refuses the leg on its own node's reading.
	// Never treat this branch as if it stopped anybody from claiming.
	//
	// The confirming block is RE-DERIVED here rather than reusing the one from the
	// lock: a reorg between funding and now would leave the cached hash pointing at
	// an orphan, whose header commits nothing. A transient RPC hiccup is polled out
	// so we do not abandon a good leg.
	anchorH := int64(0)
	var aerr error
	for i := 0; i < 6; i++ {
		if bh, berr := ops.SeqBlockHashOfTx(seqLeg.Funded.TxID); berr == nil && bh != "" {
			seqBlockHash = bh
		}
		if onChain, cerr := ops.SeqBlockOnActiveChain(seqBlockHash); cerr != nil || !onChain {
			aerr = fmt.Errorf("block %s is not on the active chain (cerr %v)", seqBlockHash, cerr)
			time.Sleep(p.Timing.Poll)
			continue
		}
		if anchorH, aerr = ops.SeqAnchorHeightOf(seqBlockHash); aerr == nil {
			break
		}
		time.Sleep(p.Timing.Poll)
	}
	res.SeqBlockHash = seqBlockHash
	if aerr != nil || anchorH < btcLegH {
		p.logf("NOT announcing the SEQ leg: its block %s anchors at %d (read err %v), below the BTC-leg height %d; "+
			"a Sequentia reorg landed the funding on a lower-anchored branch. An honest taker will not claim it; refunding after T_seq %d.",
			seqBlockHash, anchorH, aerr, btcLegH, seqLocktime)
		sendXcFail(p.Crypter, send, "seq_leg_underanchored",
			"our asset leg confirmed under-anchored; do NOT claim it (we refund it after T_seq); refund your BTC after T_btc")
	} else {
		lockedMsg := &XcMsg{
			Type: XcSeqLegLocked,
			Leg: &XcLeg{
				Txid:         seqLeg.Funded.TxID,
				Vout:         seqLeg.Funded.Vout,
				Amount:       seqLeg.Funded.Amount,
				Asset:        seqLeg.Funded.AssetID,
				RedeemScript: hex.EncodeToString(seqLeg.Script),
				Locktime:     seqLocktime,
				BlockHash:    seqBlockHash,
				AnchorHeight: anchorH,
			},
		}
		if err := sendXc(lockedMsg, p.Crypter, send); err != nil {
			// The SEQ leg is on-chain: from here the session must end in claim or
			// refund regardless of courier health, so a failed announce is logged,
			// not fatal. The taker most likely never learns the leg and never
			// claims; the T_seq refund below then recovers it automatically.
			p.logf("seq_leg_locked announce failed (%v); continuing to watch on-chain", err)
		}
		p.logf("SEQ leg locked: %s in block %s (anchor %d >= BTC-leg height %d)", seqLeg.Funded.TxID, seqBlockHash, anchorH, btcLegH)
	}

	// 4. Watch for the taker's claim (which reveals the secret) and redeem the
	// BTC leg — or refund the SEQ leg once T_seq passes without a claim. This
	// is a pure ON-CHAIN loop with no further courier dependency, so it is also
	// the resume entrypoint (settleMakerForward): a maker that restarts mid-swap
	// reconstructs the legs/keys from persisted state and re-enters it. Fee sizing +
	// the near-T_btc guard use the ACTUAL leg sizes (wantBtc/takeSeq), not the whole
	// offer, so a partial's fees are sized to the partial legs.
	res.FilledSeq, res.FilledBtc = takeSeq, wantBtc
	return settleMakerForward(ops, res, verifiedBtc.Leg, seqLeg, makerBtcClaim, makerSeqRefund,
		btcLocktime, seqLocktime, p.AssetHex, wantBtc, takeSeq, p.SpendFeeSats, p.Timing,
		func(code, msg string) { sendXcFail(p.Crypter, send, code, msg) }, p.OnUpdate, p.logf)
}

// settleMakerForward is the maker's on-chain settle loop, shared by
// RunMakerForward (live) and ResumeMakerForward (post-restart). It watches the
// maker's SEQ leg for the taker's claim (which reveals the secret) and redeems
// the taker's BTC leg, or refunds the SEQ leg once T_seq passes. res is mutated
// and returned; onUpdate persists each transition. onRefundNote couriers an
// advisory XcFail if a live session is still attached (nil-safe).
func settleMakerForward(ops XcOps, res *MakerForwardResult, btcLeg, seqLeg *xchain.LegLock,
	makerBtcClaim, makerSeqRefund *xchain.Key, btcLocktime, seqLocktime uint32,
	assetHex string, btcAmount, seqAmount, spendFeeSats uint64, timing XcTiming,
	onRefundNote func(code, msg string), onUpdate func(*MakerForwardResult), logf func(string, ...interface{})) (*MakerForwardResult, error) {
	timing.setDefaults()
	for {
		claimTxid, secret, werr := ops.WatchSEQClaim(seqLeg)
		if werr == nil && len(secret) > 0 {
			if err := ops.InjectSecret(secret); err != nil {
				return res, err
			}
			res.Secret = secret
			if onUpdate != nil {
				onUpdate(res)
			}
			btcClaimTxid, cerr := ops.ClaimBTCLeg(btcLeg, makerBtcClaim, xcSafeFee(spendFeeSats, btcAmount))
			if cerr != nil {
				if tip, terr := ops.BtcTip(); terr == nil && uint32(tip) >= btcLocktime-6 {
					return res, fmt.Errorf("btc claim still failing near T_btc %d (secret persisted): %w", btcLocktime, cerr)
				}
				logf("btc claim retrying: %v", cerr)
				time.Sleep(timing.Poll)
				continue
			}
			res.BtcClaimTxid = btcClaimTxid
			res.Settled = true
			if onUpdate != nil {
				onUpdate(res)
			}
			logf("settled: taker claimed SEQ (%s), we claimed BTC (%s)", claimTxid, btcClaimTxid)
			return res, nil
		}
		tip, terr := ops.SeqTip()
		if terr == nil && uint32(tip) >= seqLocktime {
			raw, rerr := ops.RefundSEQLeg(seqLeg, makerSeqRefund, seqLocktime, xcSeqLegFee(ops, assetHex, spendFeeSats, seqAmount))
			if rerr == nil {
				if txid, berr := ops.SeqBroadcast(raw); berr == nil {
					res.SeqRefundTx = txid
					if onUpdate != nil {
						onUpdate(res)
					}
					if onRefundNote != nil {
						onRefundNote("refunded", "seq leg refunded after T_seq")
					}
					logf("refunded SEQ leg after T_seq: %s", txid)
					return res, fmt.Errorf("%w: no claim by T_seq %d", ErrXcRefunded, seqLocktime)
				}
			}
			// Build/broadcast hiccup: retry next tick until it lands.
		}
		time.Sleep(timing.Poll)
	}
}

// MakerForwardResumeParams reconstructs a maker forward session from persisted
// state after a restart. All legs/keys/locktimes come from the on-disk record;
// the driver re-enters the on-chain settle loop with no courier.
type MakerForwardResumeParams struct {
	Ops          XcOps
	BtcLeg       *xchain.LegLock // the taker's BTC leg (we claim it with the secret)
	SeqLeg       *xchain.LegLock // our asset leg (we watch it / refund it)
	BtcClaimKey  *xchain.Key
	SeqRefundKey *xchain.Key
	HashH        []byte
	BtcLocktime  uint32
	SeqLocktime  uint32
	AssetHex     string
	BtcAmount    uint64
	SeqAmount    uint64
	SpendFeeSats uint64
	Timing       XcTiming
	OnUpdate     func(*MakerForwardResult)
	Log          func(string, ...interface{})
}

// ResumeMakerForward finishes a maker forward session after a restart: it drives
// the same on-chain settle loop RunMakerForward ends with, so a mid-swap crash
// or courier timeout completes (claim on the taker's reveal) or refunds (after
// T_seq) instead of stranding the maker's asset leg.
func ResumeMakerForward(p MakerForwardResumeParams) (*MakerForwardResult, error) {
	if p.Ops == nil || p.BtcLeg == nil || p.SeqLeg == nil || p.BtcClaimKey == nil || p.SeqRefundKey == nil {
		return nil, errors.New("maker forward resume: incomplete state")
	}
	logf := func(string, ...interface{}) {}
	if p.Log != nil {
		logf = p.Log
	}
	res := &MakerForwardResult{
		HashH: p.HashH, BtcClaimKey: p.BtcClaimKey, SeqRefundKey: p.SeqRefundKey,
		BtcLocktime: p.BtcLocktime, SeqLocktime: p.SeqLocktime,
		BtcLeg: p.BtcLeg, SeqLeg: p.SeqLeg,
	}
	return settleMakerForward(p.Ops, res, p.BtcLeg, p.SeqLeg, p.BtcClaimKey, p.SeqRefundKey,
		p.BtcLocktime, p.SeqLocktime, p.AssetHex, p.BtcAmount, p.SeqAmount, p.SpendFeeSats,
		p.Timing, nil, p.OnUpdate, logf)
}
