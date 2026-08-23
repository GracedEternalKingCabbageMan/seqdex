package xchain

import (
	"fmt"
	"time"
)

// SubmarineSwap orchestrates a SeqLN Phase 2 submarine swap: a Sequentia asset
// ON-CHAIN leg <-> BTC over VANILLA Lightning, bound by one shared SHA256
// preimage (Case A; design in
// Sequentia/doc/sequentia/seqln-phase2-submarine-swaps.md).
//
// It REUSES the on-chain SEQ leg unchanged: it embeds *Swap purely for its
// SEQ-leg operations (VerifySEQLeg / LockSEQLeg / ClaimSEQLeg / RefundSEQLeg /
// WatchSEQClaim / InjectSecret) and the shared hashlock. The BTC leg is NOT an
// on-chain HTLC here, so the embedded Swap's btcBackend is nil and never used;
// the BTC leg is the LNLeg instead.
//
// The maker plays the Boltz role: non-custodial, runs a SeqLN/CLN node on
// Bitcoin, and is the Sequentia on-chain HTLC counterparty. Worst case is a
// timelock refund on each leg.
//
// The one Sequentia-specific safety rule (design §3.2) is the anchor-depth
// secret-reveal gate: because a Sequentia tx is final only to its Bitcoin-anchor
// depth, the maker must not take the IRREVERSIBLE Lightning action until the
// relevant Sequentia tx is buried by min_anchor_depth Bitcoin blocks. Unlike the
// same-block DEX ordering (VerifySeqLegSafe, deliberately 0-conf on the anchor
// axis), a submarine cross-leg point needs a real depth > 1 (min_anchor_depth):
//   - NORMAL: gate the SEQ *funding* before the maker pays the BOLT11.
//   - REVERSE: gate the taker's SEQ *claim* (which revealed P) before the maker
//     settles the held BTC-LN invoice.
type SubmarineSwap struct {
	*Swap // SEQ-leg operations + shared hashlock (btcBackend nil, unused)

	ln LNLeg // the Bitcoin-Lightning leg (replaces the on-chain btcBackend)
}

// ClaimSEQLeg overrides the embedded Swap.ClaimSEQLeg for submarine swaps so the
// asset-claim fee is SIZED into the leg's own asset via the open-fee-market exchange
// rate before broadcast. The submarine drivers pass a NATIVE-sats target
// (SpendFeeAtoms); without sizing, claiming a high-value asset (e.g. GOLD, rate ~5e12)
// emits that target as a raw atom count whose native-equivalent value blows past the
// node's maxfeerate and sendrawtransaction rejects it. This mirrors the RFQ /
// order-book paths, which already size via FeeExchangeRate. The base Swap.ClaimSEQLeg
// keeps taking an already-sized fee, because its DEX / order-book callers pre-size it —
// so this override, reached only through *SubmarineSwap, never double-sizes them.
func (m *SubmarineSwap) ClaimSEQLeg(leg *LegLock, claimKey *Key, targetNativeFee uint64) (string, error) {
	fee := targetNativeFee
	if leg != nil && leg.Funded != nil {
		var err error
		if fee, err = m.sizeSeqSpendFee(leg.Funded.AssetID, leg.Funded.Amount, targetNativeFee); err != nil {
			return "", err
		}
	}
	return m.Swap.ClaimSEQLeg(leg, claimKey, fee)
}

// NewSubmarineSwap wires a submarine orchestrator to the anchored Sequentia node
// (SEQ leg) and a Bitcoin-Lightning leg (a SeqLN/CLN node on Bitcoin). prim is
// the shared hashlock: for a NORMAL swap the taker holds the secret and the
// maker learns it by paying the invoice; for a REVERSE swap the taker holds the
// secret and reveals it by claiming the SEQ asset on-chain.
func NewSubmarineSwap(seq *Chain, ln LNLeg, prim *HashLock) *SubmarineSwap {
	return &SubmarineSwap{
		Swap: &Swap{
			seq:    seq,
			seqLeg: NewElementsLeg(LegSEQ, prim),
			hash:   prim,
		},
		ln: ln,
	}
}

// SubAnchorEvidence records what the submarine anchor-depth gate checked.
type SubAnchorEvidence struct {
	SeqBlockHash     string
	SeqBlockAnchor   int64 // the SEQ block's recorded Bitcoin-anchor height
	NodeAnchorHeight int64 // the node's current Bitcoin-anchor tip
	AnchorDepth      int64 // NodeAnchorHeight - SeqBlockAnchor (Bitcoin blocks burying the anchor)
	MinAnchorDepth   int64
	AnchorStatus     string
	Certified        bool
	CertPresent      bool
	OK               bool
}

// VerifySeqAnchorBuried is the submarine anchor-depth gate: it confirms the
// Sequentia block seqBlockHash is anchored, quorum-certified (if the node
// reports certification), and that its Bitcoin anchor is BURIED by at least
// minAnchorDepth Bitcoin blocks (node anchor tip - block anchorheight >=
// minAnchorDepth). This is the cross-leg safety point; minAnchorDepth MUST be
// > 1 for submarine swaps (1 is unsafe, design §3.2).
func (m *SubmarineSwap) VerifySeqAnchorBuried(seqBlockHash string, minAnchorDepth int64) (*SubAnchorEvidence, error) {
	// A header is served for a disconnected block too; its anchorheight then commits
	// nothing. Establish the block is still on the active chain before reading it,
	// every pass: this is polled across a wait during which Bitcoin can reorg.
	if active, err := m.seq.BlockOnActiveChain(seqBlockHash); err != nil {
		return nil, err
	} else if !active {
		return nil, fmt.Errorf("%w (submarine gate: seq block %s is not on the active chain)", ErrAnchorOrdering, seqBlockHash)
	}
	anchor, err := m.seq.BlockAnchorHeight(seqBlockHash)
	if err != nil {
		return nil, err
	}
	status, err := m.seq.GetAnchorStatus()
	if err != nil {
		return nil, err
	}
	certified, certPresent, err := m.seq.BlockCertification(seqBlockHash)
	if err != nil {
		return nil, err
	}
	depth := status.AnchorHeight - anchor
	ev := &SubAnchorEvidence{
		SeqBlockHash:     seqBlockHash,
		SeqBlockAnchor:   anchor,
		NodeAnchorHeight: status.AnchorHeight,
		AnchorDepth:      depth,
		MinAnchorDepth:   minAnchorDepth,
		AnchorStatus:     status.AnchorStatus,
		Certified:        certified,
		CertPresent:      certPresent,
		OK:               status.AnchorStatus == "ok" && depth >= minAnchorDepth && (!certPresent || certified),
	}
	if !ev.OK {
		return ev, fmt.Errorf("%w (submarine gate: seq block %s anchorheight=%d, node anchor tip=%d, depth=%d < min=%d, anchorstatus=%q, quorum-certified=%v)",
			ErrAnchorOrdering, seqBlockHash, anchor, status.AnchorHeight, depth, minAnchorDepth, status.AnchorStatus, certified)
	}
	return ev, nil
}

// DefaultSeqClaimWindow is the Sequentia-block margin a payer keeps before the
// counterparty's refund CLTV on the leg it is about to claim.
const DefaultSeqClaimWindow = 24

// requireSeqClaimWindow refuses to take an irreversible action against a SEQ HTLC
// whose refund locktime is within minWindow blocks of the tip (0 = default).
func (m *SubmarineSwap) requireSeqClaimWindow(seqLocktime, minWindow uint32) error {
	if m.seq == nil {
		return nil
	}
	if minWindow == 0 {
		minWindow = DefaultSeqClaimWindow
	}
	tip, err := m.seq.BlockCount()
	if err != nil {
		return fmt.Errorf("seq tip before paying: %w", err)
	}
	if int64(seqLocktime) <= tip+int64(minWindow) {
		return fmt.Errorf("%w: SEQ leg T_seq=%d leaves fewer than %d blocks past tip %d; refusing to pay against a leg the counterparty can refund", ErrLNLegTimeout, seqLocktime, minWindow, tip)
	}
	return nil
}

// waitSeqAnchorBuried polls VerifySeqAnchorBuried until it passes or the deadline.
// This is where the maker deliberately WAITS for real-time anchoring to bury the
// Sequentia tx before it does the irreversible Lightning action.
func (m *SubmarineSwap) waitSeqAnchorBuried(seqBlockHash string, minAnchorDepth int64, timeout time.Duration) (*SubAnchorEvidence, error) {
	deadline := time.Now().Add(timeout)
	var last *SubAnchorEvidence
	for {
		ev, err := m.VerifySeqAnchorBuried(seqBlockHash, minAnchorDepth)
		if err == nil && ev.OK {
			return ev, nil
		}
		last = ev
		if time.Now().After(deadline) {
			if last != nil {
				return last, fmt.Errorf("%w: seq anchor not buried to depth %d within %s (last depth %d)",
					ErrLNLegTimeout, minAnchorDepth, timeout, last.AnchorDepth)
			}
			return nil, fmt.Errorf("%w: seq anchor gate never evaluated within %s: %v", ErrLNLegTimeout, timeout, err)
		}
		time.Sleep(10 * time.Second) // Bitcoin blocks are ~10 min; poll coarsely
	}
}

// --- NORMAL direction: SEQ asset on-chain -> BTC over Lightning -------------

// NormalParams is the maker's input for a NORMAL submarine swap. The taker has
// already funded the SEQ asset HTLC (claim=maker, refund=taker) and given the
// maker the BOLT11 it wants paid (payment_hash = H).
type NormalParams struct {
	HashH             []byte // the shared hashlock (== the invoice payment_hash)
	MakerSeqClaimPub  []byte // maker's SEQ-leg claim pubkey (maker claims with P)
	TakerSeqRefundPub []byte // taker's SEQ-leg refund pubkey
	SeqRedeemScript   []byte // the taker's SEQ HTLC redeemScript (verified byte-for-byte)
	SeqLocktime       uint32 // the SEQ HTLC CLTV height (taker's refund branch)
	SeqTxID           string // the taker's SEQ HTLC funding outpoint
	SeqVout           uint32
	SeqAmountAtoms    uint64 // expected SEQ-leg value
	SeqAssetID        string // expected SEQ-leg asset id (internal hex)
	SeqMinConf        int    // min Sequentia confirmations before considering the leg

	Bolt11         string // the invoice the maker pays (reveals P)
	InvoiceMsat    uint64 // expected invoice amount (0 = don't cross-check)
	MinAnchorDepth int64  // Bitcoin-anchor depth required before paying (> 1)
	AnchorTimeout  time.Duration
	// MinClaimWindow is how many Sequentia blocks must remain before SeqLocktime at
	// the moment the maker pays. The taker chose when to fund and announce, and the
	// anchor wait can consume most of a window, so the check is made again right
	// before the irreversible LN payment: paying at or past T_seq lets the taker
	// refund the asset and keep the BTC. 0 = the default (24 blocks).
	MinClaimWindow uint32
	// Max0ConfAmount is the 0-conf LP-fronting cap (asset atoms). When > 0 and the
	// SEQ-leg value is <= it, the maker SKIPS the anchor-bury wait and pays the LN
	// leg at 0-conf, fronting the Bitcoin-reorg risk on this small amount (instant
	// settlement). Above the cap the anchor gate is enforced as normal.
	Max0ConfAmount uint64
}

// NormalResult reports the outcome of a completed NORMAL swap.
type NormalResult struct {
	Anchor       *SubAnchorEvidence
	Preimage     []byte // P, learned from paying the BOLT11
	SeqClaimTxID string // the maker's SEQ-asset claim (spent with P)
}

// RunNormal executes the maker side of a NORMAL submarine swap:
//
//  1. VERIFY the taker's funded SEQ HTLC matches the quote (claimable by maker).
//  2. WAIT for the SEQ *funding* to be anchor-buried to MinAnchorDepth. Only then
//     is it safe for the maker to take the irreversible Lightning action, because
//     once the funding is that deep it will not reorg out from under the claim.
//  3. PAY the BOLT11 -> learn P (the maker is now committed on the LN side).
//  4. CLAIM the SEQ asset with P (the maker holds the claim key).
//
// The maker never pays the invoice until step 2 passes, so a Bitcoin reorg cannot
// leave it out-of-pocket on LN with an un-fundable SEQ claim.
func (m *SubmarineSwap) RunNormal(p NormalParams, makerClaimKey *Key, seqClaimFee uint64) (*NormalResult, error) {
	// 0-conf LP-fronting (value <= cap) is the DELIBERATE exception to the anchor
	// gate: the small-amount reorg risk is fronted by the maker, so the >= 2 depth
	// floor only binds when we WILL wait for the gate.
	zeroConf := p.Max0ConfAmount > 0 && p.SeqAmountAtoms <= p.Max0ConfAmount
	if !zeroConf && p.MinAnchorDepth < 2 {
		return nil, fmt.Errorf("%w: NORMAL swap MinAnchorDepth=%d must be >= 2 (design §3.2)", ErrLNLegInvalid, p.MinAnchorDepth)
	}
	// 1. Verify the taker's SEQ HTLC (correct H, amount, claimable by the maker).
	vseq, err := m.VerifySEQLeg(
		p.HashH, p.MakerSeqClaimPub, p.TakerSeqRefundPub, p.SeqRedeemScript,
		p.SeqLocktime, p.SeqTxID, p.SeqVout, p.SeqAmountAtoms, p.SeqAssetID, p.SeqMinConf,
	)
	if err != nil {
		return nil, err
	}
	// 2. Wait for the SEQ funding to be anchor-buried (the safety gate) UNLESS this
	// is a 0-conf LP-fronting swap: below the cap the maker pays immediately and
	// fronts the Bitcoin-reorg risk (instant settlement on small amounts).
	var anchor *SubAnchorEvidence
	if zeroConf {
		anchor = &SubAnchorEvidence{SeqBlockHash: vseq.BlockHash, MinAnchorDepth: 0, OK: true}
	} else {
		anchor, err = m.waitSeqAnchorBuried(vseq.BlockHash, p.MinAnchorDepth, p.AnchorTimeout)
		if err != nil {
			return &NormalResult{Anchor: anchor}, err
		}
	}
	// 3. Pay the invoice -> learn P. This is the irreversible LN action; it only
	// happens after the SEQ funding is anchor-deep AND while T_seq still leaves room
	// to claim: the wait above can eat the window the leg had at verify time.
	if err := m.requireSeqClaimWindow(p.SeqLocktime, p.MinClaimWindow); err != nil {
		return &NormalResult{Anchor: anchor}, err
	}
	preimage, err := m.ln.Pay(p.Bolt11, p.HashH, p.InvoiceMsat)
	if err != nil {
		return &NormalResult{Anchor: anchor}, fmt.Errorf("pay BOLT11: %w", err)
	}
	// 4. Claim the SEQ asset with P (the maker's reimbursement leg).
	if err := m.InjectSecret(preimage); err != nil {
		return &NormalResult{Anchor: anchor, Preimage: preimage}, err
	}
	claimTx, err := m.ClaimSEQLeg(vseq.Leg, makerClaimKey, seqClaimFee)
	if err != nil {
		// The maker has paid LN but the SEQ claim failed to broadcast. It still
		// holds P + the claim key, so this is retryable (the funding is anchor-deep
		// and will not vanish); surface the error for the caller to retry.
		return &NormalResult{Anchor: anchor, Preimage: preimage}, fmt.Errorf("claim SEQ leg after paying LN (RETRYABLE, maker holds P): %w", err)
	}
	return &NormalResult{Anchor: anchor, Preimage: preimage, SeqClaimTxID: claimTx}, nil
}

// --- REVERSE direction: BTC over Lightning -> SEQ asset on-chain ------------

// ReverseParams is the maker's input for a REVERSE submarine swap (the DEX taker
// buys the asset with BTC-LN). The taker generated the secret and sent the maker
// H; the maker issues a hold invoice on H and locks the SEQ asset HTLC
// (claim=taker, refund=maker).
type ReverseParams struct {
	HashH             []byte // the shared hashlock (taker generated P; maker knows only H until reveal)
	TakerSeqClaimPub  []byte // taker's SEQ-leg claim pubkey (taker claims with P)
	MakerSeqRefundPub []byte // maker's SEQ-leg refund pubkey (maker reclaims after timeout)
	SeqLocktime       uint32 // the SEQ HTLC CLTV height (maker's refund branch)
	SeqAmountCoins    string // the SEQ asset amount to lock (decimal string)
	SeqAssetLabel     string // the SEQ asset to lock

	InvoiceMsat  uint64        // the BTC-LN amount to hold-invoice
	InvoiceCLTV  uint32        // the hold invoice's min_final_cltv_expiry
	InvoiceLabel string        // a unique label for the hold invoice
	InvoiceDesc  string        // the hold invoice description
	HeldTimeout  time.Duration // how long to wait for the taker's LN HTLC to be held

	MinAnchorDepth int64         // Bitcoin-anchor depth of the SEQ claim required before settling (> 1)
	ClaimTimeout   time.Duration // how long to wait for the taker's on-chain SEQ claim
	AnchorTimeout  time.Duration // how long to wait for that claim to be anchor-buried
}

// ReverseResult reports the outcome of a completed REVERSE swap.
type ReverseResult struct {
	Bolt11       string // the hold invoice the taker paid
	HeldMsat     uint64 // the BTC-LN amount held
	SeqLeg       *LegLock
	SeqClaimTxID string // the taker's on-chain SEQ claim (revealed P)
	Anchor       *SubAnchorEvidence
	Preimage     []byte // P, extracted from the taker's SEQ claim
	Settled      bool   // the maker settled the held BTC-LN invoice
}

// RunReverse executes the maker side of a REVERSE submarine swap:
//
//  1. CREATE a hold invoice on H (the maker will settle it only with P).
//  2. WAIT for the taker's LN HTLC to be HELD (accepted, pending). Only after the
//     BTC-LN is inbound-and-held does the maker put its own asset at risk.
//  3. LOCK the SEQ asset HTLC (claim=taker with P, refund=maker after timeout).
//  4. WATCH for the taker's on-chain SEQ claim and EXTRACT P from it.
//  5. WAIT for that SEQ claim to be anchor-buried to MinAnchorDepth. THE gate: the
//     maker must not settle the BTC-LN leg until the SEQ claim that revealed P is
//     anchor-deep, or a Bitcoin reorg could revert the SEQ claim after the maker
//     has already taken the BTC-LN.
//  6. SETTLE the held invoice with P (the maker collects the BTC-LN).
//
// The returned *ReverseResult always carries whatever was accomplished so the
// caller can drive the refund path (CancelHold + RefundReverseSEQ) on any error.
// The maker's SEQ refund PRIVATE key is not needed here — it is only used later,
// at CLTV time, via RefundReverseSEQ; RunReverse only needs the refund PUBKEY
// (p.MakerSeqRefundPub) to build the HTLC.
func (m *SubmarineSwap) RunReverse(p ReverseParams) (*ReverseResult, error) {
	if p.MinAnchorDepth < 2 {
		return nil, fmt.Errorf("%w: REVERSE swap MinAnchorDepth=%d must be >= 2 (design §3.2)", ErrLNLegInvalid, p.MinAnchorDepth)
	}
	res := &ReverseResult{}

	// 1. Create the hold invoice on H.
	bolt11, err := m.ln.CreateHoldInvoice(p.HashH, p.InvoiceMsat, p.InvoiceCLTV, p.InvoiceLabel, p.InvoiceDesc)
	if err != nil {
		return res, fmt.Errorf("create hold invoice: %w", err)
	}
	res.Bolt11 = bolt11

	// 2. Wait for the taker's LN HTLC to be held before risking the asset.
	held, err := m.ln.WaitHeld(p.HashH, p.HeldTimeout)
	if err != nil {
		// Nothing at risk yet: cancel the invoice so it doesn't linger.
		_ = m.ln.CancelHold(p.HashH)
		return res, fmt.Errorf("wait for BTC-LN hold: %w", err)
	}
	res.HeldMsat = held
	if p.InvoiceMsat != 0 && held < p.InvoiceMsat {
		_ = m.ln.CancelHold(p.HashH)
		return res, fmt.Errorf("%w: held %d msat < invoice %d", ErrLNLegInvalid, held, p.InvoiceMsat)
	}

	// 3. Lock the SEQ asset HTLC (claim=taker, refund=maker).
	seqLeg, _, err := m.LockSEQLeg(p.TakerSeqClaimPub, p.MakerSeqRefundPub, p.SeqAmountCoins, p.SeqAssetLabel, p.SeqLocktime)
	if err != nil {
		// Could not lock the asset: refund the taker's LN so no one is stuck.
		_ = m.ln.CancelHold(p.HashH)
		if seqLeg != nil {
			res.SeqLeg = seqLeg // funded-but-unconfirmed; caller refunds after CLTV
		}
		return res, fmt.Errorf("lock SEQ leg: %w", err)
	}
	res.SeqLeg = seqLeg

	// 4. Watch for the taker's on-chain SEQ claim and extract P.
	claimTxid, secret, err := m.watchSEQClaimUntil(seqLeg, p.ClaimTimeout)
	if err != nil {
		// Taker never claimed: cancel the LN hold (auto-refunds the taker) and let
		// the caller reclaim the asset via RefundSEQLeg after SeqLocktime.
		_ = m.ln.CancelHold(p.HashH)
		return res, fmt.Errorf("wait for taker SEQ claim: %w", err)
	}
	res.SeqClaimTxID = claimTxid
	res.Preimage = secret

	// 5. THE anchor-depth gate: wait until the taker's SEQ claim is anchor-buried.
	claimBlock, err := m.seq.BlockHashOfTx(claimTxid)
	if err != nil {
		return res, fmt.Errorf("locate SEQ-claim block: %w", err)
	}
	anchor, err := m.waitSeqAnchorBuried(claimBlock, p.MinAnchorDepth, p.AnchorTimeout)
	res.Anchor = anchor
	if err != nil {
		// The claim is not (yet) anchor-deep. Do NOT settle: the maker must not take
		// the BTC-LN while the SEQ claim could still reorg out. Cancel the hold and
		// let the maker reclaim the asset via RefundSEQLeg if the claim reverts.
		_ = m.ln.CancelHold(p.HashH)
		return res, fmt.Errorf("SEQ claim not anchor-buried, refusing to settle BTC-LN: %w", err)
	}

	// 6. Settle the held invoice with P (collect the BTC-LN). Safe now: the SEQ
	// claim that revealed P is anchor-deep.
	if err := m.InjectSecret(secret); err != nil {
		return res, err
	}
	if err := m.ln.SettleHold(p.HashH, secret); err != nil {
		return res, fmt.Errorf("settle hold invoice (RETRYABLE, maker holds P): %w", err)
	}
	res.Settled = true
	return res, nil
}

// watchSEQClaimUntil polls WatchSEQClaim until the taker's SEQ claim is seen (and
// its preimage extracted) or the deadline passes.
func (m *SubmarineSwap) watchSEQClaimUntil(seqLeg *LegLock, timeout time.Duration) (string, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		claimTxid, secret, err := m.WatchSEQClaim(seqLeg)
		if err == nil && claimTxid != "" && secret != nil {
			return claimTxid, secret, nil
		}
		if time.Now().After(deadline) {
			return "", nil, fmt.Errorf("%w: no SEQ claim within %s", ErrLNLegTimeout, timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

// RefundReverseSEQ builds the maker's SEQ-leg CLTV refund for a REVERSE swap that
// stalled (taker paid neither leg through, or the claim never buried). It is a
// thin pass-through to RefundSEQLeg so callers drive the whole refund path
// (CancelHold on the LN side + this on the SEQ side) from the SubmarineSwap.
func (m *SubmarineSwap) RefundReverseSEQ(seqLeg *LegLock, makerRefundKey *Key, nLockTime uint32, fee uint64) (string, error) {
	return m.RefundSEQLeg(seqLeg, makerRefundKey, nLockTime, fee)
}

// --- REVERSE (maker-secret / taker-gated): plugin-free ----------------------
//
// The hold-invoice REVERSE (RunReverse) makes the MAKER non-custodial by having
// the taker generate P and the maker settle the held invoice only after the
// asset claim is anchor-deep. That needs a hold-invoice plugin on the node.
//
// This second REVERSE mode needs NO plugin: the MAKER generates P, locks the
// asset HTLC (claim=taker), and issues a PLAIN invoice on H. The safety of the
// cross-leg point moves to the TAKER, who must verify the asset HTLC (correct
// script/amount/claim key) AND that it is anchor-buried >= min_anchor_depth
// BEFORE paying — because paying the plain invoice settles the BTC-LN
// irreversibly and reveals P, which the taker then uses to claim the asset.
// (The taker uses VerifySeqAnchorBuried for that gate; it is the SAME rule, just
// enforced pre-payment on the taker side instead of pre-settle on the maker.)
//
// Trade-off vs the hold mode: with a plain invoice the taker cannot be refunded
// on LN if the maker's asset HTLC turns out bad, so the taker MUST do the
// verify+anchor gate first. In exchange it ships with a stock SeqLN/CLN node.

// ReverseMakerSecretParams is the maker's input for the plugin-free reverse mode.
// The SubmarineSwap MUST have been built with a full HashLock (NewHashLock(P)) so
// the maker knows P.
type ReverseMakerSecretParams struct {
	TakerSeqClaimPub  []byte // taker's SEQ-leg claim pubkey (taker claims with P)
	MakerSeqRefundPub []byte // maker's SEQ-leg refund pubkey (reclaim after CLTV)
	SeqLocktime       uint32 // the SEQ HTLC CLTV height (maker's refund branch)
	SeqAmountCoins    string // the SEQ asset amount to lock (decimal string)
	SeqAssetLabel     string // the SEQ asset to lock

	InvoiceMsat  uint64 // the BTC-LN amount to invoice
	InvoiceCLTV  uint32 // the invoice's min_final_cltv_expiry
	InvoiceLabel string // a unique label for the invoice (also used to await payment)
	InvoiceDesc  string // the invoice description
}

// ReverseMakerSecretOffer is what the maker hands the taker: the invoice to pay
// and the funded asset HTLC to verify + anchor-gate before paying.
type ReverseMakerSecretOffer struct {
	Bolt11    string
	HashH     []byte
	SeqLeg    *LegLock
	SeqBlock  string // the Sequentia block that confirmed the asset HTLC (for the taker's anchor gate)
	Label     string
	AmountSat uint64
}

// OfferReverseMakerSecret performs the maker's side of the plugin-free reverse
// swap up to the point the taker must act: it locks the asset HTLC (claim=taker)
// and issues a plain invoice on H = SHA256(P). The maker must know P (the swap
// was built with NewHashLock(P)). The returned offer carries everything the taker
// needs to verify the HTLC and run its own anchor-depth gate before paying.
func (m *SubmarineSwap) OfferReverseMakerSecret(p ReverseMakerSecretParams) (*ReverseMakerSecretOffer, error) {
	if len(m.hash.Secret) == 0 {
		return nil, fmt.Errorf("%w: maker-secret reverse requires the maker to know P (build with NewHashLock)", ErrLNLegInvalid)
	}
	// Lock the asset HTLC first (claim=taker) so the taker has something to verify.
	seqLeg, seqBlock, err := m.LockSEQLeg(p.TakerSeqClaimPub, p.MakerSeqRefundPub, p.SeqAmountCoins, p.SeqAssetLabel, p.SeqLocktime)
	if err != nil {
		return &ReverseMakerSecretOffer{SeqLeg: seqLeg}, fmt.Errorf("lock SEQ leg: %w", err)
	}
	bolt11, err := m.ln.CreateInvoice(m.hash.Secret, p.InvoiceMsat, p.InvoiceCLTV, p.InvoiceLabel, p.InvoiceDesc)
	if err != nil {
		// The asset is locked but no invoice exists: the caller reclaims it via
		// RefundReverseSEQ after SeqLocktime.
		return &ReverseMakerSecretOffer{SeqLeg: seqLeg, SeqBlock: seqBlock}, fmt.Errorf("create invoice: %w", err)
	}
	return &ReverseMakerSecretOffer{
		Bolt11:    bolt11,
		HashH:     append([]byte(nil), m.hash.Hash...),
		SeqLeg:    seqLeg,
		SeqBlock:  seqBlock,
		Label:     p.InvoiceLabel,
		AmountSat: p.InvoiceMsat / 1000,
	}, nil
}

// AwaitReversePayment blocks until the taker pays the invoice (label), returning
// the received amount (msat). After this the taker holds P (learned from paying)
// and can claim the asset; if it never returns before SeqLocktime the maker
// reclaims the asset via RefundReverseSEQ.
func (m *SubmarineSwap) AwaitReversePayment(label string, timeout time.Duration) (uint64, error) {
	return m.ln.WaitInvoicePaid(label, timeout)
}

// --- taker-side helpers (NORMAL: taker sells the asset, receives BTC-LN) -----

// MintInvoice creates a plain BOLT11 on THIS node bound to preimage (payment_hash
// = SHA256(preimage)). Used by the NORMAL-direction TAKER, which chooses P and
// mints the invoice it wants the maker to pay (paying it delivers the taker's
// BTC-LN and reveals P to the maker so it can claim the asset). Delegates to the
// LN leg's CreateInvoice.
func (m *SubmarineSwap) MintInvoice(preimage []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (string, error) {
	return m.ln.CreateInvoice(preimage, amountMsat, cltvExpiry, label, description)
}

// AwaitInvoicePaid blocks until the invoice with the given label is paid on THIS
// node (or the deadline). Generic sibling of AwaitReversePayment.
func (m *SubmarineSwap) AwaitInvoicePaid(label string, timeout time.Duration) (uint64, error) {
	return m.ln.WaitInvoicePaid(label, timeout)
}

// PayInvoice pays a BOLT11 whose payment_hash MUST equal wantHash and returns the
// revealed preimage. Used by the REVERSE-direction TAKER, which pays the maker's
// invoice and thereby LEARNS P (then claims the asset with it). It refuses any
// invoice not bound to the swap H. Delegates to the LN leg's Pay.
func (m *SubmarineSwap) PayInvoice(bolt11 string, wantHash []byte, amountMsat uint64) ([]byte, error) {
	return m.ln.Pay(bolt11, wantHash, amountMsat)
}
