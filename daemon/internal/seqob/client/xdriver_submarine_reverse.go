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

type MakerReverseSubmarineParams struct {
	// NewMakerOps binds the settlement engine to the maker-generated secret (the
	// maker builds the SubmarineSwap with NewHashLock(secret)).
	NewMakerOps func(secret []byte) SubReverseMakerOps
	Crypter     *Crypter
	SeqTip      func() (int64, error)

	AssetHex    string // SEQ asset the maker sells
	SeqAmount   uint64 // atoms the offer sells
	InvoiceMsat uint64 // BTC-LN the offer wants

	SeqLocktimeDelta uint32 // T_seq above the current SEQ tip (default 240)
	InvoiceCLTV      uint32 // min_final_cltv on the maker's invoice
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
	if p.SeqLocktimeDelta == 0 {
		p.SeqLocktimeDelta = 240
	}
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
		SeqAmountCoins:    atomsToCoins(p.SeqAmount),
		SeqAssetLabel:     p.AssetHex,
		InvoiceMsat:       p.InvoiceMsat,
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
	p.logf("reverse maker: locked asset (claim=taker), issued invoice on H=%x; awaiting payment", offer.HashH)

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

	ExpectAsset       string // SEQ asset hex the offer sells (required)
	ExpectSeqAmount   uint64 // atoms the offer promises (required)
	ExpectInvoiceMsat uint64 // BTC-LN we expect to pay (required)

	MinAnchorDepth int64  // anchor-depth gate we require before paying (>=2, default 3)
	SpendFeeAtoms  uint64 // asset-claim fee (default 1000)
	Timing         XcTiming
	Log            func(format string, args ...interface{})
}

type TakerReverseSubmarineResult struct {
	Terms        *XcMsg
	SeqLeg       *xchain.LegLock
	Anchor       *xchain.SubAnchorEvidence
	Preimage     []byte
	SeqClaimTxid string
	Settled      bool
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

	// 1. Request terms; hand the maker our SEQ-claim pubkey up front.
	if err := sendXc(&XcMsg{Type: XcSubTermsRequest, TakerSeqClaimPub: hex.EncodeToString(p.SeqClaimKey.PubKey())}, p.Crypter, send); err != nil {
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
	if leg.Amount != p.ExpectSeqAmount {
		sendXcFail(p.Crypter, send, "BAD_AMOUNT", "asset leg amount != offer")
		return res, fmt.Errorf("%w: asset leg amount %d != offer %d", ErrXcBadTerms, leg.Amount, p.ExpectSeqAmount)
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
		leg.Locktime, leg.Txid, leg.Vout, p.ExpectSeqAmount, p.ExpectAsset, 1)
	if err != nil {
		sendXcFail(p.Crypter, send, "SEQ_LEG_INVALID", err.Error())
		return res, fmt.Errorf("taker reverse verify asset HTLC: %w", err)
	}
	res.SeqLeg = vseq.Leg

	// 3. THE GATE: do NOT pay until the asset HTLC is anchor-buried (a plain
	// invoice is irreversible once paid).
	ev, err := waitAnchorBuried(ops, leg.BlockHash, p.MinAnchorDepth, p.Timing.AnchorWait)
	res.Anchor = ev
	if err != nil {
		sendXcFail(p.Crypter, send, "ANCHOR_TIMEOUT", err.Error())
		return res, fmt.Errorf("taker reverse anchor gate: %w", err)
	}
	p.logf("reverse taker: asset HTLC anchor-buried (depth=%d); paying the invoice", ev.AnchorDepth)

	// 4. Pay the invoice -> learn P. Irreversible.
	preimage, err := ops.PayInvoice(locked.Bolt11, hashH, p.ExpectInvoiceMsat)
	if err != nil {
		return res, fmt.Errorf("taker reverse pay invoice: %w", err)
	}
	res.Preimage = preimage

	// 5. Claim the asset with the learned P.
	if err := ops.InjectSecret(preimage); err != nil {
		return res, err
	}
	claimTx, err := ops.ClaimSEQLeg(vseq.Leg, p.SeqClaimKey, xcSafeFee(p.SpendFeeAtoms, vseq.Leg.Funded.Amount))
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
