package client

// xdriver_reverse.go runs the REVERSE cross-chain lift (offer.direction =
// ASSET_TO_BTC: the taker sells a Sequentia asset for real BTC; the MAKER holds
// the secret and funds the BTC leg FIRST, mirroring the deployed RFQ reverse
// design). Same construction rules as the forward driver in xdriver.go: the
// drivers own Seal/Open, everything from the peer binds to the SIGNED offer,
// redeem scripts are re-derived byte-for-byte, anchor gating uses only
// self-derived chain data, and every value-bearing transition is surfaced to a
// persistence hook BEFORE the next wait.
//
// Secret transfer: the maker's ClaimSEQLeg reveals s on-chain in the claim's
// scriptSig; the taker learns it by watching ITS OWN funded SEQ leg
// (WatchSEQClaim), never by trusting the courier. The courtesy
// XcSecretRevealed message is still sent for other implementations, but this
// taker deliberately ignores it: the on-chain path cannot be withheld or
// spoofed by the peer or relay.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// --- REVERSE taker (sells the asset, receives BTC) ----------------------------

// TakerReverseParams configures RunTakerReverse. Expectations come from the
// SIGNED offer (a maker BUY: it gives BTC, wants the asset).
type TakerReverseParams struct {
	// NewOps binds the settlement engine to the hashlock H once the maker's
	// terms arrive. The taker does not know the secret, but its SEQ leg script
	// and its BTC claim both embed H, so the swap must be built from the H in
	// the terms (mirrors the forward maker's NewOps(hashH)).
	NewOps  func(hashH []byte) (XcOps, error)
	Crypter *Crypter

	// Ctx cancels the lift. It interrupts the anchor precondition below, where
	// nothing of ours is committed yet, so cancelling never loses funds — this is
	// the seam the wallet's "cancel this trade" button uses. nil == Background.
	Ctx context.Context

	BtcClaimKey  *xchain.Key // claims the maker's BTC leg once the secret is revealed
	SeqRefundKey *xchain.Key // refunds our SEQ leg after T_seq if the maker never claims

	ExpectAsset     string // SEQ asset hex we sell (required)
	ExpectSeqAmount uint64 // atoms the WHOLE signed offer buys (required)
	ExpectBtcAmount uint64 // sats the WHOLE signed offer pays (required)
	MaxFeeBtc       uint64 // refuse terms whose fee_btc exceeds this

	// TakeSeqAmount is the slice of the offer to sell (asset atoms). 0 (or >= the
	// whole) sells into the WHOLE offer, reducing to the classic whole-HTLC lift.
	// For a partial (0 < TakeSeqAmount < ExpectSeqAmount) the maker pays
	// ProportionalBtcFloor(ExpectBtcAmount, TakeSeqAmount, ExpectSeqAmount) — floor,
	// in the maker's favour since the MAKER GIVES the BTC here, bound to the SIGNED
	// offer's ratio — and we deliver exactly TakeSeqAmount of the asset.
	TakeSeqAmount uint64

	// Locktime sanity. T_btc is when the MAKER can take its BTC back, so it
	// must leave us real runway to claim after the secret appears; T_seq is
	// when WE can refund our asset leg, so it must fit the funding +
	// confirmation + the maker's gate-and-claim.
	MinBtcClaimDelta uint32 // T_btc >= btcTip + this (default 30 parent blocks)
	MinSeqFundWindow uint32 // T_seq >= seqTip + this (default 120 SEQ blocks)
	BtcClaimMargin   uint32 // refuse to claim closer than this to T_btc (default 6)

	MinBTCConf   int    // confirmations required on the MAKER's BTC leg before we fund (default 1)
	SpendFeeSats uint64 // fee target in native sats (default 1000)
	Timing       XcTiming
	Log          func(format string, args ...interface{})

	// OnSeqLegFunded is invoked the moment our SEQ leg is funded, BEFORE any
	// further wait: the caller must persist the result there so a crash never
	// strands the asset behind an unknowable redeem script.
	OnSeqLegFunded func(*TakerReverseResult)
}

// TakerReverseResult is returned even alongside an error once the SEQ leg is
// funded, so the caller can persist it and refund after T_seq.
type TakerReverseResult struct {
	Terms        *XcMsg // the maker's XcBtcLegLocked (terms ride in it)
	BtcLeg       *xchain.LegLock
	BtcLocktime  uint32
	SeqLeg       *xchain.LegLock
	SeqBlockHash string
	SeqLocktime  uint32
	Secret       []byte
	BtcClaimTxid string
	SeqRefundTx  string
	// FilledSeq / FilledBtc are the asset atoms sold and BTC sats received for this
	// lift (== the whole offer for a whole-HTLC lift; a smaller slice + its
	// proportional BTC for a partial). The caller persists/reports these.
	FilledSeq uint64
	FilledBtc uint64
}

func (p *TakerReverseParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunTakerReverse executes the reverse handshake as the taker: request terms
// (shipping both taker keys), verify the maker's BTC leg through our own
// node's confirmation, fund the SEQ asset leg, then watch our leg for the
// maker's claim (which reveals the secret on-chain) and claim the BTC leg —
// or refund the SEQ leg once T_seq passes without a claim.
func RunTakerReverse(p TakerReverseParams, send XcSend, recv XcRecv) (*TakerReverseResult, error) {
	p.Timing.setDefaults()
	if p.NewOps == nil || p.Crypter == nil || p.BtcClaimKey == nil || p.SeqRefundKey == nil {
		return nil, errors.New("taker reverse: incomplete params")
	}
	if p.ExpectAsset == "" || p.ExpectSeqAmount == 0 || p.ExpectBtcAmount == 0 {
		return nil, errors.New("taker reverse: offer expectations required")
	}
	if p.MinBtcClaimDelta == 0 {
		p.MinBtcClaimDelta = 30
	}
	if p.MinSeqFundWindow == 0 {
		p.MinSeqFundWindow = 120
	}
	if p.BtcClaimMargin == 0 {
		p.BtcClaimMargin = 6
	}
	// 0 IS A REAL CHOICE — accept the counterparty's BTC HTLC straight from the
	// mempool. It used to be coerced to 1, which made "instant" inexpressible and
	// forced a Bitcoin confirmation into every rail-crossing take: a taker paying
	// over Lightning against a best-priced on-chain maker waited ~10 minutes for a
	// block, even though its own Lightning payment was already held.
	//
	// The user's price should never cost them latency. Rail-blind matching means the
	// best-priced offer wins whatever rail it rests on, and the bridge is supposed to
	// absorb the difference — not pass a block time back to the taker.
	//
	// The trade-off is explicit: at 0 the maker takes double-spend risk on the
	// counterparty's funding tx. That is bounded here because the bridge counterparty
	// is the LSP, which is already holding the taker's Lightning payment when it
	// funds, so it gains nothing by double-spending itself. Raise this for a maker
	// that faces anonymous takers or real value.
	if p.MinBTCConf < 0 {
		p.MinBTCConf = 0
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	if p.Ctx == nil {
		p.Ctx = context.Background()
	}
	res := &TakerReverseResult{}

	// Partial fills: decide the slice up front — the maker funds its BTC leg FIRST,
	// so it must learn the slice BEFORE funding (via this request). takeSeq==0 (or
	// >= the whole) is the classic whole-HTLC lift. The maker pays the PROPORTIONAL
	// BTC for the slice (floor, in the maker's favour since the maker GIVES the BTC),
	// which we recompute from OUR verified offer values and require exactly.
	takeSeq := p.TakeSeqAmount
	if takeSeq == 0 {
		takeSeq = p.ExpectSeqAmount
	}
	if takeSeq > p.ExpectSeqAmount {
		return res, fmt.Errorf("%w: take %d exceeds the offer's %d", ErrXcBadTerms, takeSeq, p.ExpectSeqAmount)
	}
	wantBtc := ProportionalBtcFloor(p.ExpectBtcAmount, takeSeq, p.ExpectSeqAmount)
	if wantBtc == 0 {
		return res, fmt.Errorf("%w: take %d of %d prices to 0 sats (dust)", ErrXcBadTerms, takeSeq, p.ExpectSeqAmount)
	}
	// Minimum-slice guard (BTC side): the BTC leg we will CLAIM must clear the safe
	// minimum, else it is sub-dust-after-fee and stranded. Fail CLOSED here, BEFORE
	// we even request terms or fund the asset leg; a whole take is exempt. (The asset
	// leg we FUND is guarded once the engine exists, below.)
	if err := minSafeBtcErr(takeSeq, p.ExpectSeqAmount, wantBtc, p.SpendFeeSats); err != nil {
		return res, err
	}
	res.FilledSeq, res.FilledBtc = takeSeq, wantBtc

	// 1. Request terms, shipping the keys the maker's BTC HTLC must pay + the slice.
	req := &XcMsg{
		Type:              XcTermsRequest,
		TakerSeqRefundPub: hex.EncodeToString(p.SeqRefundKey.PubKey()),
		TakerBtcClaimPub:  hex.EncodeToString(p.BtcClaimKey.PubKey()),
		SeqAmount:         takeSeq, // the slice we sell (maker sizes its BTC leg to it)
	}
	if err := sendXc(req, p.Crypter, send); err != nil {
		return res, err
	}

	// 2. The maker's BTC leg + terms (one message; the lock is broadcast-only
	// so it arrives fast, but give it the leg-wait budget, not the terms one).
	locked, err := recvXcType(recv, p.Crypter, XcBtcLegLocked, p.Timing.SeqLockWait)
	if err != nil {
		return res, err
	}
	res.Terms = locked
	if locked.Leg == nil {
		return res, errors.New("btc_leg_locked without a leg")
	}
	// The maker must pay exactly the proportional BTC (floor) for our slice, in both
	// the terms field and the funded leg, and size the trade to takeSeq. Binding to
	// wantBtc/takeSeq (not the whole offer) is what makes the partial fund-safe: we
	// deliver takeSeq asset only against a BTC leg of exactly the proportional value.
	if locked.BtcAmount != wantBtc || locked.Leg.Amount != wantBtc {
		sendXcFail(p.Crypter, send, "terms_mismatch", "btc amount != proportional for the slice")
		return res, fmt.Errorf("%w: btc %d/%d != proportional %d (take %d of %d)", ErrXcBadTerms, locked.BtcAmount, locked.Leg.Amount, wantBtc, takeSeq, p.ExpectSeqAmount)
	}
	if locked.SeqAmount != takeSeq {
		sendXcFail(p.Crypter, send, "terms_mismatch", "seq_amount differs from the taken slice")
		return res, fmt.Errorf("%w: seq_amount %d != taken %d", ErrXcBadTerms, locked.SeqAmount, takeSeq)
	}
	if locked.FeeBtc > p.MaxFeeBtc {
		sendXcFail(p.Crypter, send, "terms_mismatch", "fee_btc exceeds the taker bound")
		return res, fmt.Errorf("%w: fee_btc %d > max %d", ErrXcBadTerms, locked.FeeBtc, p.MaxFeeBtc)
	}
	hashH, err := hex.DecodeString(locked.HashH)
	if err != nil || len(hashH) != 32 {
		return res, fmt.Errorf("%w: bad hash_h", ErrXcBadTerms)
	}
	makerSeqClaimPub, err := hex.DecodeString(locked.MakerSeqClaimPub)
	if err != nil || len(makerSeqClaimPub) != 33 {
		return res, fmt.Errorf("%w: bad maker_seq_claim_pub", ErrXcBadTerms)
	}
	makerBtcRefundPub, err := hex.DecodeString(locked.MakerRefundPub)
	if err != nil || len(makerBtcRefundPub) != 33 {
		return res, fmt.Errorf("%w: bad maker_refund_pub", ErrXcBadTerms)
	}
	// Bind the settlement engine to H now that we know it (our SEQ leg script
	// and BTC claim both embed it).
	ops, err := p.NewOps(hashH)
	if err != nil {
		return res, err
	}
	// Minimum-slice guard (asset side): the asset leg we are about to FUND (and would
	// refund after T_seq) must clear the safe minimum in the asset's own atoms, else
	// it is sub-dust-after-fee and stranded. Fail CLOSED before LockSEQLeg; a whole
	// take is exempt.
	if err := minSafeAssetErr(ops, p.ExpectAsset, takeSeq, p.ExpectSeqAmount, takeSeq, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}
	btcTip, err := ops.BtcTip()
	if err != nil {
		return res, err
	}
	seqTip, err := ops.SeqTip()
	if err != nil {
		return res, err
	}
	tBtc := locked.Leg.Locktime
	tSeq := locked.SeqLocktime
	res.BtcLocktime, res.SeqLocktime = tBtc, tSeq
	if tBtc < uint32(btcTip)+p.MinBtcClaimDelta {
		sendXcFail(p.Crypter, send, "terms_mismatch", "btc_locktime leaves no claim runway")
		return res, fmt.Errorf("%w: T_btc %d vs tip %d (min delta %d)", ErrXcBadTerms, tBtc, btcTip, p.MinBtcClaimDelta)
	}
	if tSeq < uint32(seqTip)+p.MinSeqFundWindow {
		sendXcFail(p.Crypter, send, "terms_mismatch", "seq_locktime leaves no funding window")
		return res, fmt.Errorf("%w: T_seq %d vs tip %d (min window %d)", ErrXcBadTerms, tSeq, seqTip, p.MinSeqFundWindow)
	}

	// 3. Verify the maker's BTC leg against OUR node, polling out propagation
	// and confirmation (the maker broadcasts at 0-conf; we fund the asset only
	// against a leg confirmed to OUR satisfaction). Only a proven-invalid leg
	// is terminal.
	script, err := hex.DecodeString(locked.Leg.RedeemScript)
	if err != nil {
		return res, fmt.Errorf("bad btc redeem_script hex: %w", err)
	}
	var verifiedBtc *xchain.VerifiedBTCLeg
	confDeadline := time.Now().Add(p.Timing.BtcConfWait)
	for {
		verifiedBtc, err = ops.VerifyBTCLeg(hashH, p.BtcClaimKey.PubKey(), makerBtcRefundPub, script,
			tBtc, locked.Leg.Txid, locked.Leg.Vout, locked.Leg.Amount, p.MinBTCConf)
		if err == nil {
			break
		}
		if errors.Is(err, xchain.ErrBTCLegInvalid) {
			sendXcFail(p.Crypter, send, "btc_leg_invalid", err.Error())
			return res, err
		}
		if time.Now().After(confDeadline) {
			sendXcFail(p.Crypter, send, "btc_conf_timeout", "maker btc leg did not confirm in time")
			return res, fmt.Errorf("maker btc leg %s: not confirmed within %s", locked.Leg.Txid, p.Timing.BtcConfWait)
		}
		time.Sleep(p.Timing.Poll)
	}
	res.BtcLeg = verifiedBtc.Leg
	p.logf("maker BTC leg verified + confirmed: %s (%d sats, T_btc=%d)", locked.Leg.Txid, locked.Leg.Amount, tBtc)

	// 3b. ANCHOR PRECONDITION — the mirror of the forward maker's. Here WE are the
	// asset giver, so the maker's gate is the one we must satisfy: the block that
	// confirms our funding has to commit anchorheight >= the maker's BTC-leg
	// height, and that value is immutable once the funding confirms. Wait for our
	// own anchor to reach it FIRST; R2 monotonicity then makes every block
	// extending this tip satisfy the gate by construction. Measured against our
	// own node's verification of the maker's leg (verifiedBtc.Height), raised (never
	// lowered) by anything the maker itself reported, plus one block of slack for
	// the maker's non-atomic height derivation — waiting longer is never unsafe.
	//
	// How long: until the timelock says a leg funded now could no longer be claimed
	// (T_seq for the maker's claim, T_btc for ours), or the user cancels. Never a
	// flat number of minutes (owner ruling 2026-07-25). Aborting here is clean: no
	// asset has moved and the maker refunds at T_btc.
	btcLegH := claimantBtcLegHeight(verifiedBtc.Height, locked.Leg.Height)
	window := fundWindow{
		SeqLocktime:       tSeq,
		MinSeqClaimWindow: p.MinSeqFundWindow,
		BtcLocktime:       tBtc,
		MinBtcClaimWindow: p.BtcClaimMargin,
	}
	if err := waitSeqAnchorReachesBTCLeg(p.Ctx, ops, btcLegH+1, window, p.Timing, p.logf); err != nil {
		sendXcFail(p.Crypter, send, "anchor_not_caught_up", "our anchor has not reached your BTC leg; asset leg not funded")
		return res, fmt.Errorf("%w (BTC-leg height %d; nothing of ours was spent)", err, btcLegH)
	}

	// 3c. RE-VERIFY THE MAKER'S BTC LEG. The wait above is bounded by the timelock,
	// not by a clock, so it can be long — and a BTC HTLC that was confirmed when we
	// checked can be GONE by the time it ends: a one-block parent reorg lets the
	// maker double-spend the input it funded with. Funding our asset against a dead
	// BTC leg is exactly the one-sided loss this gate exists to prevent, so the full
	// verification runs again — same txid, vout, amount, script, locktime,
	// confirmations — immediately before LockSEQLeg. A changed height also
	// invalidates the anchor precondition we just satisfied, so that is a refusal
	// too (fail closed; nothing of ours has moved).
	recheckBtc, rerr := ops.VerifyBTCLeg(hashH, p.BtcClaimKey.PubKey(), makerBtcRefundPub, script,
		tBtc, locked.Leg.Txid, locked.Leg.Vout, locked.Leg.Amount, p.MinBTCConf)
	if rerr != nil {
		sendXcFail(p.Crypter, send, "btc_leg_gone", "your BTC leg no longer verifies; asset leg not funded")
		return res, fmt.Errorf("%w: re-verifying %s after the anchor wait: %v (nothing of ours was spent)",
			ErrBtcLegChanged, locked.Leg.Txid, rerr)
	}
	if recheckBtc.Height != verifiedBtc.Height {
		sendXcFail(p.Crypter, send, "btc_leg_gone", "your BTC leg moved to a different block; asset leg not funded")
		return res, fmt.Errorf("%w: %s was at height %d, now %d (nothing of ours was spent)",
			ErrBtcLegChanged, locked.Leg.Txid, verifiedBtc.Height, recheckBtc.Height)
	}
	// 3d. The timelock re-check at the last possible moment.
	if werr := window.check(ops); werr != nil {
		sendXcFail(p.Crypter, send, "seq_window_closed", "the asset leg's claim window has run down; asset leg not funded")
		return res, fmt.Errorf("%w (nothing of ours was spent)", werr)
	}

	// 4. Fund our SEQ asset leg (claim = the maker's key, refund = ours after
	// T_seq), persisting the moment it is funded; poll out a slow confirmation
	// instead of orphaning the leg.
	seqLeg, seqBlockHash, err := ops.LockSEQLeg(makerSeqClaimPub, p.SeqRefundKey.PubKey(),
		atomsToCoins(takeSeq), p.ExpectAsset, tSeq)
	if seqLeg != nil {
		res.SeqLeg = seqLeg
		if p.OnSeqLegFunded != nil {
			p.OnSeqLegFunded(res)
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
				return res, fmt.Errorf("seq leg %s funded but unconfirmed within %s (refund after T_seq %d)",
					seqLeg.Funded.TxID, p.Timing.SeqLockWait, tSeq)
			}
			time.Sleep(p.Timing.Poll)
		}
	}
	res.SeqBlockHash = seqBlockHash
	if p.OnSeqLegFunded != nil {
		p.OnSeqLegFunded(res)
	}
	// POST-FUNDING CHECK — the mirror of the forward maker's (xdriver.go). The
	// precondition above makes it true by construction on the honest path, but a
	// Sequentia reorg can still land our funding on a lower-anchored branch, and
	// this side previously just shrugged (`anchorH = 0` and announce anyway). It
	// must not: an asset leg whose block anchors BELOW the maker's BTC leg is
	// precisely the leg that can outlive that BTC leg, and inviting the maker to
	// claim it is inviting our own loss.
	//
	// Withholding the announcement is a COURTESY, not a guarantee — the maker minted
	// the secret and holds our refund pubkey, so it can rebuild this redeem script
	// and find the P2SH itself. The real defences are the precondition above (we do
	// not fund early) and the maker's own claimant gate (it refuses on its own
	// node's reading). What withholding does buy: an HONEST maker is told plainly
	// not to claim, and we keep the T_seq refund path clean.
	//
	// The confirming block is re-derived and checked to be on the ACTIVE chain
	// first: a cached hash can point at an orphan whose header commits nothing.
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
		p.logf("NOT announcing our SEQ leg: its block %s anchors at %d (read err %v), below the maker's BTC-leg height %d; "+
			"an honest maker must not claim it. We refund it after T_seq %d.",
			seqBlockHash, anchorH, aerr, btcLegH, tSeq)
		sendXcFail(p.Crypter, send, "seq_leg_underanchored",
			"our asset leg confirmed under-anchored; do NOT claim it (we refund it after T_seq); refund your BTC after T_btc")
	} else {
		fundedMsg := &XcMsg{
			Type: XcSeqLegFunded,
			Leg: &XcLeg{
				Txid:         seqLeg.Funded.TxID,
				Vout:         seqLeg.Funded.Vout,
				Amount:       seqLeg.Funded.Amount,
				Asset:        seqLeg.Funded.AssetID,
				RedeemScript: hex.EncodeToString(seqLeg.Script),
				Locktime:     tSeq,
				BlockHash:    seqBlockHash,
				AnchorHeight: anchorH,
			},
		}
		if err := sendXc(fundedMsg, p.Crypter, send); err != nil {
			// The leg is on-chain: proceed to the watch loop regardless; if the
			// maker never learned of it, our T_seq refund recovers it.
			p.logf("seq_leg_funded announce failed (%v); continuing to watch on-chain", err)
		}
		p.logf("SEQ leg funded: %s in block %s (anchor %d >= BTC-leg height %d)", seqLeg.Funded.TxID, seqBlockHash, anchorH, btcLegH)
	}

	// 5. Watch OUR leg for the maker's claim: its scriptSig carries the secret
	// (the courier's XcSecretRevealed is deliberately not relied upon). If
	// T_seq passes unclaimed, refund the asset.
	for {
		_, secret, werr := ops.WatchSEQClaim(seqLeg)
		if werr == nil && len(secret) > 0 {
			res.Secret = secret
			break
		}
		tip, terr := ops.SeqTip()
		if terr == nil && uint32(tip) >= tSeq {
			raw, rerr := ops.RefundSEQLeg(seqLeg, p.SeqRefundKey, tSeq, xcSeqLegFee(ops, p.ExpectAsset, p.SpendFeeSats, takeSeq))
			if rerr == nil {
				if txid, berr := ops.SeqBroadcast(raw); berr == nil {
					res.SeqRefundTx = txid
					p.logf("refunded SEQ leg after T_seq: %s", txid)
					return res, fmt.Errorf("%w: maker never claimed by T_seq %d", ErrXcRefunded, tSeq)
				}
			}
			// Build/broadcast hiccup: retry next tick until it lands.
		}
		time.Sleep(p.Timing.Poll)
	}

	// 6. Claim the BTC leg with the revealed secret, retried until the maker's
	// refund path nears (T_btc); the margin stops a claim-vs-refund race.
	if err := ops.InjectSecret(res.Secret); err != nil {
		return res, err
	}
	for {
		tip, terr := ops.BtcTip()
		if terr == nil && uint32(tip)+p.BtcClaimMargin >= tBtc {
			return res, fmt.Errorf("btc claim window closed (tip %d within %d of T_btc %d; secret %x persisted)",
				tip, p.BtcClaimMargin, tBtc, res.Secret)
		}
		txid, cerr := ops.ClaimBTCLeg(verifiedBtc.Leg, p.BtcClaimKey, xcSafeFee(p.SpendFeeSats, wantBtc))
		if cerr == nil {
			res.BtcClaimTxid = txid
			p.logf("settled: maker claimed our asset, we claimed BTC (%s)", txid)
			return res, nil
		}
		p.logf("btc claim retrying: %v", cerr)
		time.Sleep(p.Timing.Poll)
	}
}

// RefundTakerSEQ spends the taker's SEQ leg through the CLTV refund path once
// T_seq has passed. With wait=false it returns ErrXcRefundNotDue when early.
func RefundTakerSEQ(ops XcOps, leg *xchain.LegLock, key *xchain.Key, locktime uint32,
	assetHex string, spendFeeSats uint64, wait bool, poll time.Duration) (string, error) {
	if poll <= 0 {
		poll = 15 * time.Second
	}
	if spendFeeSats == 0 {
		spendFeeSats = 1000
	}
	for {
		tip, err := ops.SeqTip()
		if err != nil {
			return "", err
		}
		if uint32(tip) >= locktime {
			break
		}
		if !wait {
			return "", fmt.Errorf("%w: seq tip %d < T_seq %d", ErrXcRefundNotDue, tip, locktime)
		}
		time.Sleep(poll)
	}
	raw, err := ops.RefundSEQLeg(leg, key, locktime, xcSeqLegFee(ops, assetHex, spendFeeSats, leg.Funded.Amount))
	if err != nil {
		return "", err
	}
	return ops.SeqBroadcast(raw)
}

// --- REVERSE maker (buys the asset with BTC; holds the secret) ----------------

// MakerReverseParams configures RunMakerReverse.
type MakerReverseParams struct {
	// NewOps binds the settlement engine to the freshly minted SECRET (the
	// maker is the secret holder in reverse).
	NewOps  func(secret []byte) (XcOps, error)
	Crypter *Crypter

	// Ctx cancels the swap. Like the forward side's, it interrupts only waits
	// where nothing of ours is committed yet (or is already recoverable), so
	// cancelling is the user's call and never loses funds — the wallet's "cancel
	// this trade" button wires straight to it. nil == context.Background().
	Ctx context.Context

	// Tip queries usable BEFORE the engine exists.
	BtcTip func() (int64, error)
	SeqTip func() (int64, error)

	AssetHex  string // SEQ asset we buy (offer pair base)
	SeqAmount uint64 // atoms we require
	BtcAmount uint64 // sats we pay
	FeeBtc    uint64 // advisory fee surfaced in terms

	BtcLocktimeDelta uint32 // default 100 (our BTC refund if the taker vanishes; ~16h)
	SeqLocktimeDelta uint32 // default 240 (the taker's refund horizon; ~2h)

	MinBTCConf     int    // confirmations we need on our OWN BTC leg before the anchor gate (default 1)
	SeqClaimMargin uint32 // never reveal the secret closer than this to T_seq (default 10)
	SpendFeeSats   uint64
	Timing         XcTiming
	Log            func(format string, args ...interface{})

	// OnUpdate persists the evolving result; in reverse the SECRET is the
	// maker's crown jewel and is minted here, so the first call (before any
	// coins move) must already durably hold it and both keys.
	OnUpdate func(*MakerReverseResult)
}

// MakerReverseResult is the reverse maker's persistence record.
type MakerReverseResult struct {
	Secret       []byte
	HashH        []byte
	SeqClaimKey  *xchain.Key // claims the taker's asset leg (reveals the secret)
	BtcRefundKey *xchain.Key // refunds our BTC leg after T_btc
	BtcLocktime  uint32
	SeqLocktime  uint32
	BtcLeg       *xchain.LegLock // our funded BTC leg
	BtcLegHeight int64
	SeqLeg       *xchain.LegLock // the taker's verified asset leg
	SeqBlockHash string
	SeqClaimTxid string
	BtcRefundTx  string
	Settled      bool
	// FilledSeq / FilledBtc are the asset atoms the maker BUYS and the BTC sats it
	// PAYS for this lift (== the offer for a whole lift; a smaller slice + its
	// proportional BTC for a partial). The serve loop re-rests the remainder.
	FilledSeq uint64
	FilledBtc uint64
}

func (p *MakerReverseParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunMakerReverse executes the reverse handshake as the maker: mint the secret
// and per-lift keys, fund the BTC leg FIRST, announce it with the terms, verify
// the taker's asset leg through the anchor gate, and claim it (revealing the
// secret; the taker then claims the BTC leg). If the taker never funds, the
// session ends with the BTC leg persisted for a T_btc refund.
func RunMakerReverse(p MakerReverseParams, in <-chan []byte, send XcSend) (*MakerReverseResult, error) {
	p.Timing.setDefaults()
	if p.NewOps == nil || p.Crypter == nil || p.BtcTip == nil || p.SeqTip == nil {
		return nil, errors.New("maker reverse: incomplete params")
	}
	if p.AssetHex == "" || p.SeqAmount == 0 || p.BtcAmount == 0 {
		return nil, errors.New("maker reverse: offer amounts required")
	}
	if p.BtcLocktimeDelta == 0 {
		p.BtcLocktimeDelta = 100
	}
	if p.SeqLocktimeDelta == 0 {
		p.SeqLocktimeDelta = 240
	}
	// 0 IS A REAL CHOICE — accept the counterparty's BTC HTLC straight from the
	// mempool. It used to be coerced to 1, which made "instant" inexpressible and
	// forced a Bitcoin confirmation into every rail-crossing take: a taker paying
	// over Lightning against a best-priced on-chain maker waited ~10 minutes for a
	// block, even though its own Lightning payment was already held.
	//
	// The user's price should never cost them latency. Rail-blind matching means the
	// best-priced offer wins whatever rail it rests on, and the bridge is supposed to
	// absorb the difference — not pass a block time back to the taker.
	//
	// The trade-off is explicit: at 0 the maker takes double-spend risk on the
	// counterparty's funding tx. That is bounded here because the bridge counterparty
	// is the LSP, which is already holding the taker's Lightning payment when it
	// funds, so it gains nothing by double-spending itself. Raise this for a maker
	// that faces anonymous takers or real value.
	if p.MinBTCConf < 0 {
		p.MinBTCConf = 0
	}
	if p.SeqClaimMargin == 0 {
		p.SeqClaimMargin = 10
	}
	if p.Ctx == nil {
		p.Ctx = context.Background()
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
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
	res := &MakerReverseResult{}

	// 1. Terms request must carry BOTH taker keys (the BTC HTLC pays the
	// taker's claim key; the asset HTLC refunds to the taker's refund key).
	req, err := recvXcType(recv, p.Crypter, XcTermsRequest, p.Timing.TermsReqWait)
	if err != nil {
		return res, err
	}
	takerSeqRefundPub, err := hex.DecodeString(req.TakerSeqRefundPub)
	if err != nil || len(takerSeqRefundPub) != 33 {
		sendXcFail(p.Crypter, send, "bad_pubkey", "taker_seq_refund_pub required for a reverse lift")
		return res, errors.New("bad taker_seq_refund_pub")
	}
	takerBtcClaimPub, err := hex.DecodeString(req.TakerBtcClaimPub)
	if err != nil || len(takerBtcClaimPub) != 33 {
		sendXcFail(p.Crypter, send, "bad_pubkey", "taker_btc_claim_pub required for a reverse lift")
		return res, errors.New("bad taker_btc_claim_pub")
	}

	// Partial fills: req.SeqAmount is the slice the taker sells (0 = the whole offer,
	// back-compat). We PAY the proportional BTC for it (floor, in the maker's favour
	// since we give the BTC) and BUY exactly that slice. Reject an over-ask, or a
	// slice that prices to 0 sats, before any coins move — every value-move fails
	// closed. The serve loop re-rests offer-minus-filled by subtracting the paid sats.
	takeSeq := req.SeqAmount
	if takeSeq == 0 {
		takeSeq = p.SeqAmount
	}
	if takeSeq > p.SeqAmount {
		sendXcFail(p.Crypter, send, "amount_too_large", "requested to sell more asset than the offer buys")
		return res, fmt.Errorf("maker reverse: take %d > offer %d", takeSeq, p.SeqAmount)
	}
	payBtc := ProportionalBtcFloor(p.BtcAmount, takeSeq, p.SeqAmount)
	if payBtc == 0 {
		sendXcFail(p.Crypter, send, "amount_too_small", "slice prices to 0 sats (dust)")
		return res, fmt.Errorf("maker reverse: take %d of %d prices to 0 sats", takeSeq, p.SeqAmount)
	}
	// Minimum-slice guard (BTC side): the BTC leg we are about to FUND (and would
	// refund after T_btc) must clear the safe minimum, else it is sub-dust-after-fee
	// and stranded. Fail CLOSED here, BEFORE the secret is minted or any coin moves;
	// a whole take is exempt. (The asset leg we CLAIM is guarded once the engine
	// exists, below.)
	if err := minSafeBtcErr(takeSeq, p.SeqAmount, payBtc, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}
	res.FilledSeq, res.FilledBtc = takeSeq, payBtc

	// 2. Mint the secret + per-lift keys and PERSIST before any coins move.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return res, err
	}
	hashH := sha256.Sum256(secret)
	seqClaim, err := xchain.NewKey()
	if err != nil {
		return res, err
	}
	btcRefund, err := xchain.NewKey()
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
	res.Secret, res.HashH = secret, hashH[:]
	res.SeqClaimKey, res.BtcRefundKey = seqClaim, btcRefund
	res.BtcLocktime = uint32(btcTip) + p.BtcLocktimeDelta
	res.SeqLocktime = uint32(seqTip) + p.SeqLocktimeDelta
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	ops, err := p.NewOps(secret)
	if err != nil {
		return res, err
	}
	// Minimum-slice guard (asset side): the taker's asset leg we will CLAIM (delivered
	// as takeSeq atoms) must clear the safe minimum in the asset's own atoms, else our
	// claim output is sub-dust and stranded. Fail CLOSED before we fund the BTC leg
	// below; a whole take is exempt. (The BTC leg was guarded before the secret mint.)
	if err := minSafeAssetErr(ops, p.AssetHex, takeSeq, p.SeqAmount, takeSeq, p.SpendFeeSats); err != nil {
		sendXcFail(p.Crypter, send, "amount_too_small", err.Error())
		return res, err
	}

	// 3. Fund the BTC leg FIRST (the reverse design: the taker will only fund
	// the asset against our confirmed leg). Broadcast-only on a live parent.
	btcLeg, _, err := ops.LockBTCLeg(takerBtcClaimPub, btcRefund.PubKey(), atomsToCoins(payBtc), res.BtcLocktime)
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_lock_failed", err.Error())
		return res, err
	}
	res.BtcLeg = btcLeg
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	announce := &XcMsg{
		Type:             XcBtcLegLocked,
		HashH:            hex.EncodeToString(hashH[:]),
		MakerSeqClaimPub: hex.EncodeToString(seqClaim.PubKey()),
		MakerRefundPub:   hex.EncodeToString(btcRefund.PubKey()),
		SeqLocktime:      res.SeqLocktime,
		BtcAmount:        payBtc,
		SeqAmount:        takeSeq,
		FeeBtc:           p.FeeBtc,
		Leg: &XcLeg{
			Txid:         btcLeg.Funded.TxID,
			Vout:         btcLeg.Funded.Vout,
			Amount:       btcLeg.Funded.Amount,
			RedeemScript: hex.EncodeToString(btcLeg.Script),
			Locktime:     res.BtcLocktime,
		},
	}
	if err := sendXc(announce, p.Crypter, send); err != nil {
		// Our BTC is already locked; the taker will never see the leg, so no
		// asset is coming. The leg is persisted; refund after T_btc.
		return res, fmt.Errorf("btc_leg_locked announce failed (BTC refundable after T_btc %d): %w", res.BtcLocktime, err)
	}
	p.logf("BTC leg funded + announced: %s (%d sats, buy %d of %d %s, T_btc=%d T_seq=%d)",
		btcLeg.Funded.TxID, payBtc, takeSeq, p.SeqAmount, p.AssetHex, res.BtcLocktime, res.SeqLocktime)

	// 4. The taker's asset leg (it waits for OUR confirmation first, so give it
	// the long budget). A no-show leaves our BTC leg persisted for the T_btc
	// refund; that griefing cost is inherent to funding first (as in the RFQ).
	funded, err := recvXcType(recv, p.Crypter, XcSeqLegFunded, p.Timing.BtcFundWait)
	if err != nil {
		return res, fmt.Errorf("no seq leg from the taker (BTC refundable after T_btc %d): %w", res.BtcLocktime, err)
	}
	if funded.Leg == nil {
		return res, errors.New("seq_leg_funded without a leg")
	}
	// The taker must deliver exactly the slice it asked for (takeSeq), of our asset.
	if funded.Leg.Amount != takeSeq || funded.Leg.Asset != p.AssetHex {
		sendXcFail(p.Crypter, send, "seq_leg_mismatch", "amount/asset differ from the taken slice")
		return res, fmt.Errorf("seq leg %d %s != taken %d %s", funded.Leg.Amount, funded.Leg.Asset, takeSeq, p.AssetHex)
	}
	if funded.Leg.Locktime != res.SeqLocktime {
		sendXcFail(p.Crypter, send, "seq_leg_mismatch", "locktime differs from terms")
		return res, fmt.Errorf("seq leg locktime %d != terms %d", funded.Leg.Locktime, res.SeqLocktime)
	}
	script, err := hex.DecodeString(funded.Leg.RedeemScript)
	if err != nil {
		return res, fmt.Errorf("bad seq redeem_script hex: %w", err)
	}
	var verifiedSeq *xchain.VerifiedSEQLeg
	verifyDeadline := time.Now().Add(p.Timing.SeqLockWait)
	for {
		verifiedSeq, err = ops.VerifySEQLeg(hashH[:], seqClaim.PubKey(), takerSeqRefundPub, script,
			res.SeqLocktime, funded.Leg.Txid, funded.Leg.Vout, funded.Leg.Amount, funded.Leg.Asset, 1)
		if err == nil {
			break
		}
		if errors.Is(err, xchain.ErrSEQLegInvalid) || time.Now().After(verifyDeadline) {
			sendXcFail(p.Crypter, send, "seq_leg_invalid", err.Error())
			return res, err
		}
		time.Sleep(p.Timing.Poll)
	}
	res.SeqLeg = verifiedSeq.Leg
	res.SeqBlockHash = verifiedSeq.BlockHash
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}

	// 5. Measure our OWN BTC leg's confirmation height (broadcast-only earlier)
	// for the anchor gate: the taker's asset block must anchor at/above it so
	// a parent reorg reverts both legs together before we reveal anything.
	// Derived ATOMICALLY (confirmations read either side of the tip read): a parent
	// block landing between the two RPCs used to inflate this by one, and the gate
	// then demanded an anchor the taker was never told to reach.
	var btcLegHeight int64
	hDeadline := time.Now().Add(p.Timing.BtcConfWait)
	for {
		h, _, herr := btcLegConfirmedHeight(ops, btcLeg.Funded.TxID, p.MinBTCConf)
		if herr == nil {
			btcLegHeight = h
			break
		}
		if time.Now().After(hDeadline) {
			return res, fmt.Errorf("own btc leg %s never reached %d conf (refund after T_btc %d)",
				btcLeg.Funded.TxID, p.MinBTCConf, res.BtcLocktime)
		}
		time.Sleep(p.Timing.Poll)
	}
	res.BtcLegHeight = btcLegHeight
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}

	// 6. Anchor gate on the confirming block, RE-DERIVED from the leg's txid on
	// every pass (gateSeqLeg) rather than cached at verification time, then the
	// no-reveal margin, then claim the asset (revealing the secret on-chain).
	if _, gerr := gateSeqLeg(p.Ctx, ops, verifiedSeq.Leg.Funded.TxID, btcLegHeight,
		claimWindow{SeqLocktime: res.SeqLocktime, Margin: p.SeqClaimMargin}, p.Timing, nil); gerr != nil {
		if errors.Is(gerr, xchain.ErrAnchorOrderingTerminal) {
			return res, fmt.Errorf("anchor gate terminally failed (waiting cannot help): %w (not revealing; both legs refundable)", gerr)
		}
		return res, fmt.Errorf("anchor gate did not pass while T_seq %d left a claim window: %w (not revealing; both legs refundable)",
			res.SeqLocktime, gerr)
	}
	tip, err := p.SeqTip()
	if err != nil {
		return res, err
	}
	if uint32(tip)+p.SeqClaimMargin >= res.SeqLocktime {
		return res, fmt.Errorf("seq tip %d within %d of T_seq %d; not revealing the secret", tip, p.SeqClaimMargin, res.SeqLocktime)
	}
	claimTxid, err := ops.ClaimSEQLeg(verifiedSeq.Leg, seqClaim, xcSeqLegFee(ops, p.AssetHex, p.SpendFeeSats, takeSeq))
	if err != nil {
		return res, fmt.Errorf("seq claim failed: %w", err)
	}
	res.SeqClaimTxid = claimTxid
	res.Settled = true
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	// Courtesy reveal (the taker can also read the secret off our claim).
	reveal := &XcMsg{Type: XcSecretRevealed, Preimage: hex.EncodeToString(secret)}
	if sealed, serr := reveal.Seal(p.Crypter); serr == nil {
		_ = send(sealed)
	}
	p.logf("settled: claimed the asset in %s (secret revealed; taker claims the BTC leg)", claimTxid)
	return res, nil
}

// --- REVERSE maker resume (post-restart) -------------------------------------

// MakerReverseResumeParams reconstructs a reverse maker session from persisted
// state. The maker holds the secret and has funded the BTC leg; depending on how
// far the swap got, resume either claims the taker's asset leg (if the taker
// funded it and it is still claimable before T_seq) or refunds the maker's own
// BTC leg after T_btc. All material comes from the on-disk record.
type MakerReverseResumeParams struct {
	Ops            XcOps
	BtcLeg         *xchain.LegLock // ours; refunded after T_btc if we cannot settle
	SeqLeg         *xchain.LegLock // the taker's asset leg (nil if never funded/verified)
	SeqBlockHash   string          // the taker leg's confirming block (for the anchor gate)
	Secret         []byte
	HashH          []byte
	SeqClaimKey    *xchain.Key // claims the taker's asset leg (reveals the secret)
	BtcRefundKey   *xchain.Key // refunds our BTC leg after T_btc
	BtcLocktime    uint32
	SeqLocktime    uint32
	AssetHex       string
	BtcAmount      uint64
	SeqAmount      uint64
	SeqClaimMargin uint32
	MinBTCConf     int
	SpendFeeSats   uint64
	Timing         XcTiming
	OnUpdate       func(*MakerReverseResult)
	Log            func(string, ...interface{})

	// Ctx cancels the resume. Same contract as MakerReverseParams.Ctx: it only
	// interrupts waits where nothing of ours is committed or everything is still
	// refundable. nil == context.Background().
	Ctx context.Context
}

// ResumeMakerReverse finishes a reverse maker session after a restart. If the
// taker's asset leg is present and still claimable, it anchor-gates and claims
// it (revealing the secret; the taker then claims BTC). Otherwise it refunds the
// maker's BTC leg once T_btc passes. Claim and refund are mutually exclusive, so
// the secret is never revealed on a path we also refund.
func ResumeMakerReverse(p MakerReverseResumeParams) (*MakerReverseResult, error) {
	if p.Ops == nil || p.BtcLeg == nil || p.BtcRefundKey == nil {
		return nil, errors.New("maker reverse resume: incomplete state")
	}
	p.Timing.setDefaults()
	if p.SeqClaimMargin == 0 {
		p.SeqClaimMargin = 10
	}
	if p.Ctx == nil {
		p.Ctx = context.Background()
	}
	// 0 IS A REAL CHOICE — accept the counterparty's BTC HTLC straight from the
	// mempool. It used to be coerced to 1, which made "instant" inexpressible and
	// forced a Bitcoin confirmation into every rail-crossing take: a taker paying
	// over Lightning against a best-priced on-chain maker waited ~10 minutes for a
	// block, even though its own Lightning payment was already held.
	//
	// The user's price should never cost them latency. Rail-blind matching means the
	// best-priced offer wins whatever rail it rests on, and the bridge is supposed to
	// absorb the difference — not pass a block time back to the taker.
	//
	// The trade-off is explicit: at 0 the maker takes double-spend risk on the
	// counterparty's funding tx. That is bounded here because the bridge counterparty
	// is the LSP, which is already holding the taker's Lightning payment when it
	// funds, so it gains nothing by double-spending itself. Raise this for a maker
	// that faces anonymous takers or real value.
	if p.MinBTCConf < 0 {
		p.MinBTCConf = 0
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	logf := func(string, ...interface{}) {}
	if p.Log != nil {
		logf = p.Log
	}
	res := &MakerReverseResult{
		Secret: p.Secret, HashH: p.HashH,
		SeqClaimKey: p.SeqClaimKey, BtcRefundKey: p.BtcRefundKey,
		BtcLocktime: p.BtcLocktime, SeqLocktime: p.SeqLocktime,
		BtcLeg: p.BtcLeg, SeqLeg: p.SeqLeg, SeqBlockHash: p.SeqBlockHash,
	}

	// Try to claim the taker's asset leg if we have it and can still do so
	// safely before T_seq. Anything that prevents a safe claim falls through to
	// the BTC refund.
	if p.SeqLeg != nil && p.SeqClaimKey != nil && len(p.Secret) == 32 && p.SeqBlockHash != "" {
		claimed, err := resumeReverseTryClaim(p, res, logf)
		if err != nil {
			return res, err
		}
		if claimed {
			return res, nil
		}
	} else {
		logf("reverse resume: no claimable asset leg (taker never funded / not verified); will refund the BTC leg after T_btc %d", p.BtcLocktime)
	}

	// Refund our BTC leg once T_btc passes. By then T_seq (the shorter leg) has
	// also passed, so the taker has already (or will) refund its asset leg — no
	// double-settlement.
	for {
		tip, err := p.Ops.BtcTip()
		if err != nil {
			return res, err
		}
		if uint32(tip) >= p.BtcLocktime {
			break
		}
		time.Sleep(p.Timing.Poll)
	}
	txid, err := p.Ops.RefundBTCLeg(p.BtcLeg, p.BtcRefundKey, p.BtcLocktime, xcSafeFee(p.SpendFeeSats, p.BtcAmount))
	if err != nil {
		return res, fmt.Errorf("btc refund after T_btc %d: %w", p.BtcLocktime, err)
	}
	res.BtcRefundTx = txid
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	logf("reverse resume: refunded BTC leg after T_btc: %s", txid)
	return res, fmt.Errorf("%w: reverse lift refunded (btc %s)", ErrXcRefunded, txid)
}

// resumeReverseTryClaim measures the BTC-leg height, runs the anchor gate, checks
// the T_seq margin, and claims the taker's asset leg. Returns (true, nil) on a
// successful claim; (false, nil) means "cannot claim safely, refund BTC instead".
func resumeReverseTryClaim(p MakerReverseResumeParams, res *MakerReverseResult, logf func(string, ...interface{})) (bool, error) {
	// Our BTC leg's confirmation height for the anchor ordering check, derived
	// atomically (see btcLegConfirmedHeight) so a parent block landing mid-read
	// cannot inflate it by one and fail the gate on a good leg.
	var btcLegHeight int64
	hDeadline := time.Now().Add(p.Timing.BtcConfWait)
	for {
		h, _, herr := btcLegConfirmedHeight(p.Ops, p.BtcLeg.Funded.TxID, p.MinBTCConf)
		if herr == nil {
			btcLegHeight = h
			break
		}
		if time.Now().After(hDeadline) {
			logf("reverse resume: own BTC leg never reached %d conf; refunding", p.MinBTCConf)
			return false, nil
		}
		time.Sleep(p.Timing.Poll)
	}
	// Anchor gate on the taker leg's confirming block, RE-DERIVED from the leg's
	// txid every pass: the persisted SeqBlockHash was captured before the restart
	// and a reorg since then would leave it pointing at an orphan, whose header
	// commits nothing. The persisted hash is used only when the leg outpoint is
	// missing (it never is here — the caller checks SeqLeg first).
	if _, gerr := gateSeqLeg(p.Ctx, p.Ops, p.SeqLeg.Funded.TxID, btcLegHeight,
		claimWindow{SeqLocktime: p.SeqLocktime, Margin: p.SeqClaimMargin}, p.Timing, nil); gerr != nil {
		if errors.Is(gerr, xchain.ErrAnchorOrderingTerminal) {
			logf("reverse resume: anchor gate TERMINALLY failed (%v); refunding BTC instead of revealing the secret", gerr)
			return false, nil
		}
		logf("reverse resume: anchor gate did not pass (%v); refunding BTC instead of revealing the secret", gerr)
		return false, nil
	}
	// Never reveal the secret inside the T_seq margin (the taker could be
	// refunding its asset leg).
	tip, err := p.Ops.SeqTip()
	if err != nil {
		return false, err
	}
	if uint32(tip)+p.SeqClaimMargin >= p.SeqLocktime {
		logf("reverse resume: within %d of T_seq %d; too late to claim safely, refunding BTC", p.SeqClaimMargin, p.SeqLocktime)
		return false, nil
	}
	if err := p.Ops.InjectSecret(p.Secret); err != nil {
		return false, err
	}
	txid, err := p.Ops.ClaimSEQLeg(p.SeqLeg, p.SeqClaimKey, xcSeqLegFee(p.Ops, p.AssetHex, p.SpendFeeSats, p.SeqAmount))
	if err != nil {
		return false, fmt.Errorf("seq claim on resume: %w", err)
	}
	res.SeqClaimTxid = txid
	res.Settled = true
	if p.OnUpdate != nil {
		p.OnUpdate(res)
	}
	logf("reverse resume: claimed the taker's asset leg %s (secret revealed)", txid)
	return true, nil
}
