package client

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xdriver_submarine_reverse.go runs the REVERSE submarine-swap lift (BTC over
// Lightning -> Sequentia asset on-chain), the plugin-free MAKER-SECRET mode: the
// maker generates P, locks the asset HTLC (claim=taker), and issues a PLAIN
// invoice on H. The taker verifies the HTLC and runs the anchor-depth gate BEFORE
// paying (a plain invoice cannot be refunded once paid), pays (learning P), and
// claims the asset. It reuses the same courier + XcMsg envelope as the NORMAL
// driver; only the roles/messages differ. Settlement is the proven pkg/xchain
// SubmarineSwap (OfferReverseMakerSecret / PayInvoice), unchanged.
//
//	REVERSE maker-secret (ln_direction = LnBTCForAsset; maker SELLS the asset)
//	  taker -> XcSubTermsRequest{taker_seq_claim_pub}
//	  maker  : generate P; OfferReverseMakerSecret locks the asset (claim=taker,
//	           refund=maker) and mints a plain invoice on H
//	  maker -> XcSubAssetLocked{hash_h, maker_refund_pub (seq), seq_locktime,
//	           leg(SEQ block_hash+anchor), bolt11}
//	  taker  : VerifySEQLeg -> wait VerifySeqAnchorBuried >= min_anchor_depth ->
//	           PayInvoice (learns P) -> ClaimSEQLeg(P)
//	  maker  : AwaitReversePayment (got the BTC-LN); refund the asset after
//	           seq_locktime if the taker never pays.

// Reverse-submarine HOLD-INVOICE coupling (fund-safety invariant).
//
// The reverse-submarine TAKER refuses a hold-invoice MASQUERADE (subswap.js
// holdCltvSafeVsTseq): a bolt11 hold invoice is byte-identical to a plain one, so
// the taker gates on the invoice's BTC-LN min_final_cltv (fc, in BTC blocks). It
// converts fc into a Sequentia settle-deadline with the CONSERVATIVE INVERSE ratio
// (SEQ slots per BTC block). The SEQ slot is DETERMINISTIC (30 s,
// g_pos_slot_interval), so the ENTIRE margin belongs on the VARIABLE Bitcoin side:
// Bitcoin's nominal block is ~600 s, but a sustained hashrate-drop lull can average
// ~1500-1800 s/block over a short window, so we assume Bitcoin as SLOW as ~1800 s
// and Sequentia at its exact 30 s slot => 1800/30 = 60 SEQ slots per BTC block.
//
// The gate is evaluated at the POST-anchor-bury tip. The wallet taker
// (minAnchorDepth=3, max0conf=0) waits ~minAnchorDepth BTC confs to bury the fresh
// asset HTLC before paying, and the SEQ tip advances ~minAnchorDepth*ratio blocks
// during that wait, so:
//
//	settleDeadlineSeq = seqTip2 + ceil(fc * SubReverseConservativeRatio)
//	gate PASSES iff    settleDeadlineSeq + SubReverseClaimMargin < T_seq
//	  where seqTip2 = M + SubReverseMinAnchorDepth*ratio  (the tip AFTER the bury)
//
// For an HONEST maker's offer to CLEAR that gate, the maker must size its asset leg
// T_seq (SeqLocktimeDelta) and the minted bolt11's min_final_cltv from ONE
// invariant, so the two can never silently drift apart — and T_seq MUST also absorb
// the anchor-bury tip advance (minAnchorDepth*ratio), since the gate only runs AFTER
// the taker has waited out the bury:
//
//	SeqLocktimeDelta >= (fc + minAnchorDepth)*ratio + claimMargin + buffer
//
// The maker mints a BOUNDED-SMALL fc (viable over the short LSP-hub routes these
// swaps take) and DERIVES T_seq from it. This coupling is SINGLE-CHAIN and unrelated
// to the cross-path W1/W2 delta (cmd/seqob-maker -seq-locktime-delta, sized against
// T_btc); the reverse-submarine maker uses THESE constants, not that flag.
//
// RESIDUAL (irreducible, documented — NOT a logic bug). This is a FIXED SEQ window
// versus a VARIABLE, unbounded Bitcoin block time. The ratio is sized for a
// sustained ~1800 s/block lull; if the REAL Bitcoin average over the fc-block window
// exceeds ratio*30 s, a hold-masquerade maker could reveal P PAST the claim window
// and capture the BTC-LN with no asset for the taker. It is BOUNDED, never a
// permanent freeze: the taker caps its OWN outgoing max-cltv-delay at fc, so a HELD
// payment refunds if unsettled — the loss materialises only if the maker actively
// settles late, is borne by the taker/LSP, and never leaves the taker's BTC merely
// HELD unrecoverable. Mitigated by the generous ratio + the bounded fc + the
// max-cltv cap; NOT eliminated (Bitcoin block time is unbounded) — the SAME known
// limitation every Lightning CLTV delta lives with.
const (
	SubReverseInvoiceCLTV       = 8   // fc: min_final_cltv (BTC blocks) on the reverse maker's bolt11. BOUNDED-SMALL: below the bolt11 default 18, viable over short LSP-hub routes; the maker is the final hop so it fully controls it. A smaller fc keeps the coupled T_seq sane.
	SubReverseConservativeRatio = 60  // conservative SEQ slots per BTC block: SEQ slot deterministic 30 s (g_pos_slot_interval); assume Bitcoin as SLOW as ~1800 s/block (a sustained hashrate-lull average, ~3x nominal) => 1800/30 = 60x. ALL the margin sits on the VARIABLE Bitcoin side. MUST equal subswap.js SLOW_BTC_SECS/FAST_SEQ_SECS.
	SubReverseClaimMargin       = 120 // SEQ blocks the taker keeps to claim after the latest possible reveal (matches subswap.js claimMargin)
	SubReverseTseqBuffer        = 40  // slack absorbing ceil() + the SEQ-tip advance during the pre-bury handshake (kept < ratio, so the honest fc is exactly the largest CLTV the T_seq admits)
	SubReverseMinAnchorDepth    = 3   // BTC confs the wallet taker buries the fresh asset HTLC before paying (matches the wallet minAnchorDepth default, max0conf=0). The SEQ tip advances ~this*ratio blocks during that wait, so T_seq MUST include it or the gate (run at the post-bury tip) refuses an honest offer.

	// SubReverseSeqLocktimeDelta is the coupled T_seq for the default fc: it MUST
	// clear the taker's gate for fc = SubReverseInvoiceCLTV, evaluated at the
	// POST-anchor-bury tip (seqTip2 = M + SubReverseMinAnchorDepth*ratio).
	//   (8+3)*60 + 120 + 40 = 820 SEQ blocks (~6.8 h at a 30 s slot).
	SubReverseSeqLocktimeDelta = (SubReverseInvoiceCLTV+SubReverseMinAnchorDepth)*SubReverseConservativeRatio + SubReverseClaimMargin + SubReverseTseqBuffer
)

// coupleSubReverse returns the (fc, T_seq) the reverse maker MUST use so an honest
// offer clears the taker's hold-CLTV gate evaluated at the POST-anchor-bury tip. It
// defaults fc to the bounded value and RAISES T_seq to the coupled minimum
// ((fc+minAnchorDepth)*ratio + claimMargin + buffer) whenever a caller-supplied
// delta falls short — an honest maker may pick a LARGER T_seq, but never a smaller
// one, or the taker would (correctly) refuse the offer. The minAnchorDepth*ratio
// term is what absorbs the SEQ-tip advance while the taker waits out the bury.
func coupleSubReverse(fc, seqDelta uint32) (uint32, uint32) {
	if fc == 0 {
		fc = SubReverseInvoiceCLTV
	}
	minDelta := (fc+SubReverseMinAnchorDepth)*SubReverseConservativeRatio + SubReverseClaimMargin + SubReverseTseqBuffer
	if seqDelta < minDelta {
		seqDelta = minDelta
	}
	return fc, seqDelta
}

// SubReverseMakerOps is the settlement seam for the reverse maker.
type SubReverseMakerOps interface {
	OfferReverseMakerSecret(p xchain.ReverseMakerSecretParams) (*xchain.ReverseMakerSecretOffer, error)
	AwaitReversePayment(label string, timeout time.Duration) (uint64, error)
	RefundReverseSEQ(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error)
}

// SubReverseTakerOps is the settlement seam for the reverse taker.
type SubReverseTakerOps interface {
	VerifySEQLeg(hashH, claimPub, refundPub, providedScript []byte, seqLocktime uint32,
		txid string, vout uint32, amount uint64, assetID string, minConf int) (*xchain.VerifiedSEQLeg, error)
	VerifySeqAnchorBuried(seqBlockHash string, minAnchorDepth int64) (*xchain.SubAnchorEvidence, error)
	PayInvoice(bolt11 string, wantHash []byte, amountMsat uint64) ([]byte, error)
	InjectSecret(secret []byte) error
	ClaimSEQLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error)
}

// LiveSubReverseMakerOps / LiveSubReverseTakerOps bind the seams to a real
// *xchain.SubmarineSwap (built with NewHashLock(P) for the maker, or
// NewHashLockFromHash(H) for the taker).
type LiveSubReverseMakerOps struct{ Sub *xchain.SubmarineSwap }

func (o *LiveSubReverseMakerOps) OfferReverseMakerSecret(p xchain.ReverseMakerSecretParams) (*xchain.ReverseMakerSecretOffer, error) {
	return o.Sub.OfferReverseMakerSecret(p)
}
func (o *LiveSubReverseMakerOps) AwaitReversePayment(label string, timeout time.Duration) (uint64, error) {
	return o.Sub.AwaitReversePayment(label, timeout)
}
func (o *LiveSubReverseMakerOps) RefundReverseSEQ(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Sub.RefundReverseSEQ(leg, key, nLockTime, fee)
}

type LiveSubReverseTakerOps struct{ Sub *xchain.SubmarineSwap }

func (o *LiveSubReverseTakerOps) VerifySEQLeg(hashH, claimPub, refundPub, providedScript []byte, seqLocktime uint32,
	txid string, vout uint32, amount uint64, assetID string, minConf int) (*xchain.VerifiedSEQLeg, error) {
	return o.Sub.VerifySEQLeg(hashH, claimPub, refundPub, providedScript, seqLocktime, txid, vout, amount, assetID, minConf)
}
func (o *LiveSubReverseTakerOps) VerifySeqAnchorBuried(seqBlockHash string, minAnchorDepth int64) (*xchain.SubAnchorEvidence, error) {
	return o.Sub.VerifySeqAnchorBuried(seqBlockHash, minAnchorDepth)
}
func (o *LiveSubReverseTakerOps) PayInvoice(bolt11 string, wantHash []byte, amountMsat uint64) ([]byte, error) {
	return o.Sub.PayInvoice(bolt11, wantHash, amountMsat)
}
func (o *LiveSubReverseTakerOps) InjectSecret(secret []byte) error { return o.Sub.InjectSecret(secret) }
func (o *LiveSubReverseTakerOps) ClaimSEQLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error) {
	return o.Sub.ClaimSEQLeg(leg, key, fee)
}

// --- REVERSE maker -----------------------------------------------------------

// proportionalInvoiceMsat prices a slice of a submarine offer as a whole number of
// SATOSHIS, returned in msat.
//
// The offer's price is quoted in sats, and the taker sizes its side in sats and then
// multiplies by 1000. Rounding in msat instead produces a sub-satoshi invoice that the
// taker's exact-match check rejects: an 8% slice of a 77474-sat offer ceils to 6198 sats
// (6_198_000 msat) for the taker but to 6_197_920 msat for a maker rounding in msat, and
// the swap dies with "the invoice demands 6197920 msat != the offer's 6198000 msat".
//
// So round UP to a whole sat — the same direction and the same unit as the taker — and
// only then convert. A whole-offer take is unchanged, since wholeMsat is already a whole
// number of sats.
func proportionalInvoiceMsat(wholeMsat, takeSeq, wholeSeq uint64) uint64 {
	return ProportionalBtc(wholeMsat/1000, takeSeq, wholeSeq) * 1000
}

type MakerReverseSubmarineParams struct {
	// NewMakerOps binds the settlement engine to the maker-generated secret (the
	// maker builds the SubmarineSwap with NewHashLock(secret)).
	NewMakerOps func(secret []byte) SubReverseMakerOps
	Crypter     *Crypter
	SeqTip      func() (int64, error)

	AssetHex string // SEQ asset the maker sells
	// SeqAmount is the WHOLE signed offer, in atoms — the maximum a taker may take,
	// not the amount every lift locks. The taker names its slice in the terms
	// request; see MakerReverseSubmarineResult.FilledSeq / .RemainderSeq.
	SeqAmount   uint64
	InvoiceMsat uint64 // BTC-LN the offer wants

	// SeqLocktimeDelta (T_seq above the current SEQ tip) and InvoiceCLTV
	// (min_final_cltv on the maker's bolt11) are COUPLED by coupleSubReverse: a 0
	// InvoiceCLTV defaults to SubReverseInvoiceCLTV, and SeqLocktimeDelta is raised
	// to the coupled minimum so an honest offer clears the taker's hold-CLTV gate.
	SeqLocktimeDelta uint32
	InvoiceCLTV      uint32
	Timing           XcTiming
	Log              func(format string, args ...interface{})
}

type MakerReverseSubmarineResult struct {
	HashH        []byte
	SeqRefundKey *xchain.Key // refunds the asset after T_seq if the taker never pays
	SeqLeg       *xchain.LegLock
	SeqLocktime  uint32
	Bolt11       string
	Label        string
	PaidMsat     uint64
	Settled      bool

	// FilledSeq / FilledMsat are what THIS lift settled; RemainderSeq is what is
	// left of the offer, which the caller RE-RESTS rather than retiring the offer.
	FilledSeq    uint64
	FilledMsat   uint64
	RemainderSeq uint64
}

func (p *MakerReverseSubmarineParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunMakerReverseSubmarine executes the reverse maker-secret handshake: receive the
// taker's claim pubkey, generate P, lock the asset (claim=taker) + issue a plain
// invoice on H, announce it, and wait for the taker to pay. If the taker never
// pays, the caller refunds the asset via RefundReverseSEQ after T_seq (the result
// carries the leg + refund key).
func RunMakerReverseSubmarine(p MakerReverseSubmarineParams, in <-chan []byte, send XcSend) (*MakerReverseSubmarineResult, error) {
	p.Timing.setDefaults()
	if p.NewMakerOps == nil || p.Crypter == nil || p.SeqTip == nil {
		return nil, errors.New("maker reverse submarine: incomplete params")
	}
	if p.AssetHex == "" || p.SeqAmount == 0 || p.InvoiceMsat == 0 {
		return nil, errors.New("maker reverse submarine: offer amounts required")
	}
	// Couple the invoice min_final_cltv and T_seq from ONE invariant so an honest
	// offer clears the taker's hold-CLTV masquerade gate (see coupleSubReverse):
	// default fc to the bounded value, then RAISE T_seq to
	// (fc+minAnchorDepth)*ratio+claimMargin+buffer if the caller's delta is too small
	// (e.g. the cross-sized default 240). The minAnchorDepth*ratio term covers the
	// SEQ-tip advance while the taker buries the asset HTLC before paying.
	p.InvoiceCLTV, p.SeqLocktimeDelta = coupleSubReverse(p.InvoiceCLTV, p.SeqLocktimeDelta)
	recv := chanRecv(in)
	res := &MakerReverseSubmarineResult{}

	// 1. Terms request carries the taker's SEQ-claim pubkey (the asset HTLC claim
	// branch pays the taker, so its key must arrive before we lock that leg).
	tr, err := recvXcType(recv, p.Crypter, XcSubTermsRequest, p.Timing.TermsReqWait)
	if err != nil {
		return res, err
	}
	takerSeqClaimPub, err := hex.DecodeString(tr.TakerSeqClaimPub)
	if err != nil || len(takerSeqClaimPub) != 33 {
		sendXcFail(p.Crypter, send, "BAD_PUBKEY", "taker_seq_claim_pub must be 33-byte hex")
		return res, errors.New("maker reverse submarine: bad taker_seq_claim_pub")
	}

	// PARTIAL FILLS. The request carries the slice the taker wants (XcMsg.SeqAmount,
	// the field the cross rail already uses). It previously carried only
	// TakerSeqClaimPub, so the maker locked the WHOLE offer on every lift. Zero, or
	// the whole, keeps the classic behaviour, so an older taker is unaffected.
	takeSeq := tr.SeqAmount
	if takeSeq == 0 {
		takeSeq = p.SeqAmount
	}
	if takeSeq > p.SeqAmount {
		sendXcFail(p.Crypter, send, "BAD_AMOUNT", "requested slice exceeds the offer")
		return res, fmt.Errorf("maker reverse submarine: take %d exceeds the offer's %d", takeSeq, p.SeqAmount)
	}
	// We GIVE the asset and RECEIVE the invoice here, so the invoice rounds UP —
	// the mirror of the forward direction, and in our favour on the same principle.
	// Rounded to a whole SAT, which is the unit the offer is priced in and the unit
	// the taker rounds in; see proportionalInvoiceMsat.
	invoiceMsat := proportionalInvoiceMsat(p.InvoiceMsat, takeSeq, p.SeqAmount)
	if invoiceMsat == 0 {
		sendXcFail(p.Crypter, send, "DUST", "that slice prices to zero")
		return res, fmt.Errorf("maker reverse submarine: take %d of %d prices to 0 msat", takeSeq, p.SeqAmount)
	}
	res.FilledSeq = takeSeq
	res.FilledMsat = invoiceMsat
	res.RemainderSeq = p.SeqAmount - takeSeq

	// 2. Generate P and a fresh SEQ-refund key; lock the asset + mint the invoice.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return res, err
	}
	makerSeqRefund, err := xchain.NewKey()
	if err != nil {
		return res, err
	}
	res.SeqRefundKey = makerSeqRefund
	seqTip, err := p.SeqTip()
	if err != nil {
		return res, err
	}
	seqLocktime := uint32(seqTip) + p.SeqLocktimeDelta
	res.SeqLocktime = seqLocktime
	label := "seqob-sub-reverse-" + hex.EncodeToString(secret[:8])

	ops := p.NewMakerOps(secret)
	offer, err := ops.OfferReverseMakerSecret(xchain.ReverseMakerSecretParams{
		TakerSeqClaimPub:  takerSeqClaimPub,
		MakerSeqRefundPub: makerSeqRefund.PubKey(),
		SeqLocktime:       seqLocktime,
		SeqAmountCoins:    atomsToCoins(takeSeq),
		SeqAssetLabel:     p.AssetHex,
		InvoiceMsat:       invoiceMsat,
		InvoiceCLTV:       p.InvoiceCLTV,
		InvoiceLabel:      label,
		InvoiceDesc:       "SeqOB submarine (BTC-LN -> asset)",
	})
	if offer != nil {
		res.SeqLeg = offer.SeqLeg
	}
	if err != nil {
		sendXcFail(p.Crypter, send, "LOCK_FAILED", err.Error())
		return res, fmt.Errorf("maker reverse submarine offer: %w", err)
	}
	res.HashH = offer.HashH
	res.Bolt11 = offer.Bolt11
	res.Label = label

	// 3. Announce the locked asset + the invoice to pay.
	if err := sendXc(&XcMsg{
		Type:           XcSubAssetLocked,
		HashH:          hex.EncodeToString(offer.HashH),
		MakerRefundPub: hex.EncodeToString(makerSeqRefund.PubKey()),
		SeqLocktime:    seqLocktime,
		Bolt11:         offer.Bolt11,
		Leg: &XcLeg{
			Txid:         offer.SeqLeg.Funded.TxID,
			Vout:         offer.SeqLeg.Funded.Vout,
			Amount:       offer.SeqLeg.Funded.Amount,
			Asset:        p.AssetHex,
			RedeemScript: hex.EncodeToString(offer.SeqLeg.Script),
			Locktime:     offer.SeqLeg.Locktime,
			BlockHash:    offer.SeqBlock,
		},
	}, p.Crypter, send); err != nil {
		return res, err
	}
	p.logf("reverse maker: locked asset (claim=taker) T_seq=%d (delta=%d), issued invoice min_final_cltv=%d BTC blocks on H=%x; awaiting payment (taker hold-CLTV gate at the post-bury tip: (%d+%d)*%d+%d=%d < delta)",
		seqLocktime, p.SeqLocktimeDelta, p.InvoiceCLTV, offer.HashH,
		p.InvoiceCLTV, SubReverseMinAnchorDepth, SubReverseConservativeRatio, SubReverseClaimMargin,
		(p.InvoiceCLTV+SubReverseMinAnchorDepth)*SubReverseConservativeRatio+SubReverseClaimMargin)

	// 4. Wait for the taker to pay our invoice. Once paid we have the BTC-LN and
	// the taker will claim the asset with the P it learned by paying.
	paid, err := ops.AwaitReversePayment(label, p.Timing.BtcConfWait)
	if err != nil {
		return res, fmt.Errorf("reverse maker await payment (refund asset after T_seq=%d): %w", seqLocktime, err)
	}
	res.PaidMsat = paid
	res.Settled = true
	p.logf("reverse maker: received %d msat over BTC-LN; the taker claims the asset with P", paid)
	_ = sendXc(&XcMsg{Type: XcSubSettled}, p.Crypter, send)
	return res, nil
}

// --- REVERSE taker -----------------------------------------------------------

type TakerReverseSubmarineParams struct {
	// NewTakerOps binds the engine once H arrives (taker builds the SubmarineSwap
	// with NewHashLockFromHash(H); it learns P only by paying).
	NewTakerOps func(hashH []byte) SubReverseTakerOps
	Crypter     *Crypter

	SeqClaimKey *xchain.Key // claims the asset with the learned P

	ExpectAsset     string // SEQ asset hex the offer sells (required)
	ExpectSeqAmount uint64 // atoms the WHOLE signed offer promises (required)

	// TakeSeqAmount is the slice of the offer to buy (asset atoms). 0 (or the
	// whole) buys the WHOLE offer. For a partial we pay
	// ProportionalBtc(ExpectInvoiceMsat, TakeSeqAmount, ExpectSeqAmount) — ceil,
	// in the maker's favour since the MAKER GIVES the asset — recomputed from OUR
	// verified offer values and required exactly, so the maker cannot re-price it.
	TakeSeqAmount     uint64
	ExpectInvoiceMsat uint64 // BTC-LN we expect to pay (required)

	MinAnchorDepth int64  // anchor-depth gate we require before paying (>=2, default 3)
	Max0ConfAmount uint64 // 0-conf LP-fronting cap (asset atoms); value <= it skips the bury wait
	SpendFeeAtoms  uint64 // NATIVE-sats target for the asset-claim spend (sized per-asset; default 1000)
	Timing         XcTiming
	Log            func(format string, args ...interface{})

	// OnPaid is invoked the instant the invoice is paid and P is learned, BEFORE
	// the asset claim, so the caller persists P + the leg: a crash between paying
	// (irreversible) and claiming must not lose P (the only key to the asset).
	OnPaid func(preimage []byte, leg *xchain.LegLock)
}

type TakerReverseSubmarineResult struct {
	Terms        *XcMsg
	SeqLeg       *xchain.LegLock
	Anchor       *xchain.SubAnchorEvidence
	Preimage     []byte
	SeqClaimTxid string
	Settled      bool

	// FilledSeq / FilledMsat are what THIS lift bought: the asset atoms received
	// and the invoice msat paid for them. Equal to the offer on a whole lift.
	FilledSeq  uint64
	FilledMsat uint64
}

func (p *TakerReverseSubmarineParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunTakerReverseSubmarine executes the reverse maker-secret handshake as the
// taker: request terms with a fresh claim key, verify the maker's locked asset
// HTLC, run the anchor-depth gate BEFORE paying, pay the invoice (learning P), and
// claim the asset. It NEVER pays until the asset HTLC is anchor-buried, because a
// plain invoice cannot be refunded once paid.
func RunTakerReverseSubmarine(p TakerReverseSubmarineParams, send XcSend, recv XcRecv) (*TakerReverseSubmarineResult, error) {
	p.Timing.setDefaults()
	if p.NewTakerOps == nil || p.Crypter == nil || p.SeqClaimKey == nil {
		return nil, errors.New("taker reverse submarine: incomplete params")
	}
	if p.ExpectAsset == "" || p.ExpectSeqAmount == 0 || p.ExpectInvoiceMsat == 0 {
		return nil, errors.New("taker reverse submarine: offer expectations required")
	}
	if p.MinAnchorDepth < 2 {
		p.MinAnchorDepth = 3
	}
	if p.SpendFeeAtoms == 0 {
		p.SpendFeeAtoms = 1000
	}
	res := &TakerReverseSubmarineResult{}

	// PARTIAL FILLS: decide the slice and price it ourselves before asking.
	takeSeq := p.TakeSeqAmount
	if takeSeq == 0 || takeSeq > p.ExpectSeqAmount {
		takeSeq = p.ExpectSeqAmount
	}
	wantMsat := proportionalInvoiceMsat(p.ExpectInvoiceMsat, takeSeq, p.ExpectSeqAmount)
	if wantMsat == 0 {
		return res, fmt.Errorf("%w: take %d of %d prices to 0 msat (dust)", ErrXcBadTerms, takeSeq, p.ExpectSeqAmount)
	}
	res.FilledSeq, res.FilledMsat = takeSeq, wantMsat

	// 1. Request terms; hand the maker our SEQ-claim pubkey and the slice up front.
	//
	// WIRE NOTE: seq_amount rides as a JSON NUMBER — a quoted string unmarshals to
	// 0 in the Go peer, which here would read as "the whole offer".
	if err := sendXc(&XcMsg{
		Type:             XcSubTermsRequest,
		TakerSeqClaimPub: hex.EncodeToString(p.SeqClaimKey.PubKey()),
		SeqAmount:        takeSeq,
	}, p.Crypter, send); err != nil {
		return res, err
	}
	locked, err := recvXcType(recv, p.Crypter, XcSubAssetLocked, p.Timing.SeqLockWait)
	if err != nil {
		return res, err
	}
	res.Terms = locked

	hashH, err := hex.DecodeString(locked.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "BAD_HASH", "hash_h must be 32-byte hex")
		return res, fmt.Errorf("%w: hash_h", ErrXcBadTerms)
	}
	makerSeqRefundPub, err := hex.DecodeString(locked.MakerRefundPub)
	if err != nil || len(makerSeqRefundPub) != 33 {
		sendXcFail(p.Crypter, send, "BAD_PUBKEY", "maker_refund_pub must be 33-byte hex")
		return res, fmt.Errorf("%w: maker_refund_pub", ErrXcBadTerms)
	}
	leg := locked.Leg
	if leg == nil || locked.Bolt11 == "" {
		sendXcFail(p.Crypter, send, "MISSING_LEG", "asset leg and bolt11 are required")
		return res, fmt.Errorf("%w: missing leg/bolt11", ErrXcBadTerms)
	}
	// Bind to the SIGNED offer.
	if leg.Amount != takeSeq {
		sendXcFail(p.Crypter, send, "BAD_AMOUNT", "asset leg amount != offer")
		return res, fmt.Errorf("%w: asset leg amount %d != the %d we asked for", ErrXcBadTerms, leg.Amount, takeSeq)
	}
	if leg.Asset != "" && leg.Asset != p.ExpectAsset {
		sendXcFail(p.Crypter, send, "BAD_ASSET", "asset leg asset != offer")
		return res, fmt.Errorf("%w: asset leg asset %s != offer %s", ErrXcBadTerms, leg.Asset, p.ExpectAsset)
	}
	script, err := hex.DecodeString(leg.RedeemScript)
	if err != nil {
		sendXcFail(p.Crypter, send, "BAD_SCRIPT", "redeem_script must be hex")
		return res, fmt.Errorf("%w: redeem_script", ErrXcBadTerms)
	}

	ops := p.NewTakerOps(hashH)

	// 2. Verify the asset HTLC is a real Design-A HTLC claimable by us.
	vseq, err := ops.VerifySEQLeg(hashH, p.SeqClaimKey.PubKey(), makerSeqRefundPub, script,
		leg.Locktime, leg.Txid, leg.Vout, takeSeq, p.ExpectAsset, 1)
	if err != nil {
		sendXcFail(p.Crypter, send, "SEQ_LEG_INVALID", err.Error())
		return res, fmt.Errorf("taker reverse verify asset HTLC: %w", err)
	}
	res.SeqLeg = vseq.Leg

	// 3. THE GATE: do NOT pay until the asset HTLC is anchor-buried (a plain invoice
	// is irreversible once paid) — UNLESS this is a 0-conf LP-fronting swap. Below the
	// cap the taker pays immediately and fronts the Bitcoin-reorg risk on this small
	// amount (instant settlement; the asset leg is still verified + already confirmed
	// on Sequentia, only the Bitcoin-anchor DEPTH bury is skipped).
	if p.Max0ConfAmount > 0 && vseq.Leg.Funded.Amount <= p.Max0ConfAmount {
		res.Anchor = &xchain.SubAnchorEvidence{SeqBlockHash: leg.BlockHash, MinAnchorDepth: 0, OK: true}
		p.logf("reverse taker: 0-CONF FRONTING — asset leg %d atoms <= cap %d; skipping the %d-block anchor-bury wait (accepting Bitcoin-reorg risk), paying the invoice",
			vseq.Leg.Funded.Amount, p.Max0ConfAmount, p.MinAnchorDepth)
	} else {
		ev, err := waitAnchorBuried(ops, leg.BlockHash, p.MinAnchorDepth, p.Timing.AnchorWait)
		res.Anchor = ev
		if err != nil {
			sendXcFail(p.Crypter, send, "ANCHOR_TIMEOUT", err.Error())
			return res, fmt.Errorf("taker reverse anchor gate: %w", err)
		}
		p.logf("reverse taker: asset HTLC anchor-buried (depth=%d); paying the invoice", ev.AnchorDepth)
	}

	// 4. Pay the invoice -> learn P. Irreversible.
	preimage, err := ops.PayInvoice(locked.Bolt11, hashH, p.ExpectInvoiceMsat)
	if err != nil {
		return res, fmt.Errorf("taker reverse pay invoice: %w", err)
	}
	res.Preimage = preimage
	if p.OnPaid != nil {
		p.OnPaid(preimage, vseq.Leg) // persist P before the claim (crash-safety)
	}

	// 5. Claim the asset with the learned P.
	if err := ops.InjectSecret(preimage); err != nil {
		return res, err
	}
	// Pass the NATIVE-sats target; LiveSubReverseTakerOps.ClaimSEQLeg (via
	// SubmarineSwap.ClaimSEQLeg) sizes it into the leg's own asset and clamps it.
	claimTx, err := ops.ClaimSEQLeg(vseq.Leg, p.SeqClaimKey, p.SpendFeeAtoms)
	if err != nil {
		return res, fmt.Errorf("taker reverse claim asset (RETRYABLE, taker holds P): %w", err)
	}
	res.SeqClaimTxid = claimTx
	res.Settled = true
	p.logf("reverse taker: paid BTC-LN, learned P, claimed the asset in %s", claimTx)
	return res, nil
}

// waitAnchorBuried polls VerifySeqAnchorBuried until it passes or the deadline.
func waitAnchorBuried(ops SubReverseTakerOps, blockHash string, minDepth int64, wait time.Duration) (*xchain.SubAnchorEvidence, error) {
	deadline := time.Now().Add(wait)
	var last *xchain.SubAnchorEvidence
	for {
		ev, err := ops.VerifySeqAnchorBuried(blockHash, minDepth)
		if err == nil && ev != nil && ev.OK {
			return ev, nil
		}
		last = ev
		if time.Now().After(deadline) {
			depth := int64(-1)
			if last != nil {
				depth = last.AnchorDepth
			}
			return last, fmt.Errorf("asset anchor not buried to depth %d within %s (last depth %d)", minDepth, wait, depth)
		}
		time.Sleep(10 * time.Second)
	}
}
