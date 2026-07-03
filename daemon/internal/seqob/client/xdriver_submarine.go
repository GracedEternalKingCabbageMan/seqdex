package client

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xdriver_submarine.go runs the SUBMARINE-SWAP lift handshake (xcourier_submarine.go)
// over the opaque relay courier, settling with the proven pkg/xchain submarine
// engine (SubmarineSwap). It is the Lightning sibling of xdriver.go; it does NOT
// reimplement settlement. v1 = the NORMAL direction (offer sells a Sequentia asset
// for BTC-LN; secret holder = TAKER), which needs no hold-invoice plugin.
//
// The BTC leg is a BOLT11 the taker mints on its chosen preimage P; the maker pays
// it (learning P) and claims the asset. The maker's Sequentia-safety gate (the
// asset funding must be Bitcoin-anchor-buried >= min_anchor_depth before it pays)
// lives inside SubmarineSwap.RunNormal, so this driver only carries the handshake.

// SubMakerOps is the narrow settlement seam the maker driver runs against.
// LiveSubMakerOps binds it to a real *xchain.SubmarineSwap; tests fake it.
type SubMakerOps interface {
	// RunNormal verifies the taker's funded asset HTLC, waits for it to be
	// anchor-buried, pays the BOLT11 (learning P), and claims the asset with P.
	RunNormal(p xchain.NormalParams, makerClaimKey *xchain.Key, seqClaimFee uint64) (*xchain.NormalResult, error)
}

// SubTakerOps is the settlement seam the taker driver runs against.
type SubTakerOps interface {
	SeqTip() (int64, error)
	// LockSEQLeg funds the asset HTLC (claim=maker, refund=taker) from the taker's
	// wallet, returning the leg and the confirming Sequentia block hash.
	LockSEQLeg(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*xchain.LegLock, string, error)
	// MintInvoice mints a plain BOLT11 on the taker's node bound to preimage P.
	MintInvoice(preimage []byte, amountMsat uint64, cltv uint32, label, desc string) (string, error)
	// AwaitInvoicePaid blocks until that invoice is paid (the taker's BTC-LN).
	AwaitInvoicePaid(label string, timeout time.Duration) (uint64, error)
	// RefundSEQLeg reclaims the asset HTLC via its CLTV branch after T_seq.
	RefundSEQLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error)
	// SeqBlockAnchorHeightOf reports a Sequentia block's Bitcoin-anchor height
	// (conveyed to the maker for transparency; the maker re-reads it).
	SeqBlockAnchorHeightOf(blockHash string) (int64, error)
}

// LiveSubMakerOps implements SubMakerOps over a real submarine swap.
type LiveSubMakerOps struct{ Sub *xchain.SubmarineSwap }

func (o *LiveSubMakerOps) RunNormal(p xchain.NormalParams, key *xchain.Key, fee uint64) (*xchain.NormalResult, error) {
	return o.Sub.RunNormal(p, key, fee)
}

// LiveSubTakerOps implements SubTakerOps over a real submarine swap + its SEQ node.
type LiveSubTakerOps struct {
	Sub *xchain.SubmarineSwap
	SEQ *xchain.Chain
}

func (o *LiveSubTakerOps) SeqTip() (int64, error) { return o.SEQ.BlockCount() }
func (o *LiveSubTakerOps) LockSEQLeg(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*xchain.LegLock, string, error) {
	return o.Sub.LockSEQLeg(claimPub, refundPub, amountCoins, assetLabel, locktime)
}
func (o *LiveSubTakerOps) MintInvoice(preimage []byte, amountMsat uint64, cltv uint32, label, desc string) (string, error) {
	return o.Sub.MintInvoice(preimage, amountMsat, cltv, label, desc)
}
func (o *LiveSubTakerOps) AwaitInvoicePaid(label string, timeout time.Duration) (uint64, error) {
	return o.Sub.AwaitInvoicePaid(label, timeout)
}
func (o *LiveSubTakerOps) RefundSEQLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Sub.RefundReverseSEQ(leg, key, nLockTime, fee)
}
func (o *LiveSubTakerOps) SeqBlockAnchorHeightOf(blockHash string) (int64, error) {
	return o.SEQ.BlockAnchorHeight(blockHash)
}

// --- NORMAL maker -----------------------------------------------------------

// MakerSubmarineParams configures RunMakerSubmarineNormal. Amounts come from the
// SIGNED offer; the courier peer and relay are untrusted beyond it.
type MakerSubmarineParams struct {
	// NewMakerOps binds the settlement engine once H arrives (the maker knows only
	// H, so it builds the SubmarineSwap with NewHashLockFromHash(H)).
	NewMakerOps func(hashH []byte) SubMakerOps
	Crypter     *Crypter
	SeqTip      func() (int64, error)

	AssetHex    string // SEQ asset the offer sells
	SeqAmount   uint64 // atoms the offer sells (whole-HTLC lift)
	InvoiceMsat uint64 // BTC-LN the offer wants (cross-checks the taker's bolt11)

	SeqLocktimeDelta uint32 // T_seq above the current SEQ tip (default 240)
	MinAnchorDepth   int64  // the funding anchor-depth gate before paying (default 3, >=2)
	SpendFeeAtoms    uint64 // fee for the maker's asset-claim spend (default 1000)
	AnchorTimeout    time.Duration
	Timing           XcTiming
	Log              func(format string, args ...interface{})
}

// MakerSubmarineResult is returned even alongside an error, carrying whatever was
// accomplished (for retry: the maker holds P after a successful pay).
type MakerSubmarineResult struct {
	HashH        []byte
	SeqClaimKey  *xchain.Key
	SeqLocktime  uint32
	Preimage     []byte
	SeqClaimTxid string
	Settled      bool
}

func (p *MakerSubmarineParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunMakerSubmarineNormal executes the NORMAL submarine handshake as the maker:
// advertise per-lift terms with a fresh SEQ-claim key, receive the taker's funded
// asset HTLC + BOLT11, and settle (verify -> anchor-gate -> pay -> claim) via
// SubmarineSwap.RunNormal. `in` delivers sealed courier frames for this session.
func RunMakerSubmarineNormal(p MakerSubmarineParams, in <-chan []byte, send XcSend) (*MakerSubmarineResult, error) {
	p.Timing.setDefaults()
	if p.NewMakerOps == nil || p.Crypter == nil || p.SeqTip == nil {
		return nil, errors.New("maker submarine: incomplete params")
	}
	if p.AssetHex == "" || p.SeqAmount == 0 || p.InvoiceMsat == 0 {
		return nil, errors.New("maker submarine: offer amounts required")
	}
	if p.SeqLocktimeDelta == 0 {
		p.SeqLocktimeDelta = 240
	}
	if p.MinAnchorDepth < 2 {
		p.MinAnchorDepth = 3
	}
	if p.SpendFeeAtoms == 0 {
		p.SpendFeeAtoms = 1000
	}
	recv := chanRecv(in)
	res := &MakerSubmarineResult{}

	// 1. Terms request -> mint per-lift terms with a FRESH SEQ-claim key.
	if _, err := recvXcType(recv, p.Crypter, XcSubTermsRequest, p.Timing.TermsReqWait); err != nil {
		return res, err
	}
	makerSeqClaim, err := xchain.NewKey()
	if err != nil {
		return res, err
	}
	res.SeqClaimKey = makerSeqClaim
	seqTip, err := p.SeqTip()
	if err != nil {
		return res, err
	}
	seqLocktime := uint32(seqTip) + p.SeqLocktimeDelta
	res.SeqLocktime = seqLocktime

	if err := sendXc(&XcMsg{
		Type:             XcSubTerms,
		MakerSeqClaimPub: hex.EncodeToString(makerSeqClaim.PubKey()),
		SeqLocktime:      seqLocktime,
		SeqAmount:        p.SeqAmount,
		MinAnchorDepth:   uint32(p.MinAnchorDepth),
	}, p.Crypter, send); err != nil {
		return res, err
	}
	p.logf("submarine maker: sent terms (seq_amount=%d, seq_locktime=%d, min_anchor_depth=%d)", p.SeqAmount, seqLocktime, p.MinAnchorDepth)

	// 2. Receive the taker's funded asset HTLC + the BOLT11 to pay.
	funded, err := recvXcType(recv, p.Crypter, XcSubAssetFunded, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	hashH, err := hex.DecodeString(funded.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "BAD_HASH", "hash_h must be 32-byte hex")
		return res, errors.New("submarine maker: bad hash_h")
	}
	res.HashH = hashH
	takerSeqRefundPub, err := hex.DecodeString(funded.TakerSeqRefundPub)
	if err != nil || len(takerSeqRefundPub) != 33 {
		sendXcFail(p.Crypter, send, "BAD_PUBKEY", "taker_seq_refund_pub must be 33-byte hex")
		return res, errors.New("submarine maker: bad taker_seq_refund_pub")
	}
	leg := funded.Leg
	if leg == nil || funded.Bolt11 == "" {
		sendXcFail(p.Crypter, send, "MISSING_LEG", "asset leg and bolt11 are required")
		return res, errors.New("submarine maker: missing asset leg / bolt11")
	}
	// Bind the leg to the SIGNED offer: amount + asset must match our terms; the
	// script/locktime/P2SH/value are re-derived and byte-checked inside RunNormal
	// (VerifySEQLeg) against the terms we pass, so a lying taker cannot inflate.
	if leg.Amount != p.SeqAmount {
		sendXcFail(p.Crypter, send, "BAD_AMOUNT", "asset leg amount != offer")
		return res, fmt.Errorf("submarine maker: asset leg amount %d != offer %d", leg.Amount, p.SeqAmount)
	}
	if leg.Asset != "" && leg.Asset != p.AssetHex {
		sendXcFail(p.Crypter, send, "BAD_ASSET", "asset leg asset != offer")
		return res, fmt.Errorf("submarine maker: asset leg asset %s != offer %s", leg.Asset, p.AssetHex)
	}
	script, err := hex.DecodeString(leg.RedeemScript)
	if err != nil {
		sendXcFail(p.Crypter, send, "BAD_SCRIPT", "redeem_script must be hex")
		return res, errors.New("submarine maker: bad redeem_script")
	}

	// 3. Settle: verify -> wait anchor-buried -> pay -> claim.
	ops := p.NewMakerOps(hashH)
	nr, err := ops.RunNormal(xchain.NormalParams{
		HashH:             hashH,
		MakerSeqClaimPub:  makerSeqClaim.PubKey(),
		TakerSeqRefundPub: takerSeqRefundPub,
		SeqRedeemScript:   script,
		SeqLocktime:       seqLocktime,
		SeqTxID:           leg.Txid,
		SeqVout:           leg.Vout,
		SeqAmountAtoms:    p.SeqAmount,
		SeqAssetID:        p.AssetHex,
		SeqMinConf:        1,
		Bolt11:            funded.Bolt11,
		InvoiceMsat:       p.InvoiceMsat,
		MinAnchorDepth:    p.MinAnchorDepth,
		AnchorTimeout:     p.AnchorTimeout,
	}, makerSeqClaim, p.SpendFeeAtoms)
	if nr != nil {
		res.Preimage = nr.Preimage
		res.SeqClaimTxid = nr.SeqClaimTxID
	}
	if err != nil {
		sendXcFail(p.Crypter, send, "SETTLE_FAILED", err.Error())
		return res, fmt.Errorf("submarine maker settle: %w", err)
	}
	res.Settled = true
	p.logf("submarine maker: settled, claimed asset in %s", nr.SeqClaimTxID)

	// 4. Courtesy notice (the taker already has its BTC-LN).
	_ = sendXc(&XcMsg{Type: XcSubSettled, SettleTxid: nr.SeqClaimTxID}, p.Crypter, send)
	return res, nil
}

// --- NORMAL taker -----------------------------------------------------------

// TakerSubmarineParams configures RunTakerSubmarineNormal. Expectations come from
// the SIGNED offer the taker verified before lifting.
type TakerSubmarineParams struct {
	Ops     SubTakerOps
	Crypter *Crypter

	Secret       []byte      // 32-byte preimage P (taker chooses; the invoice is minted on it)
	SeqRefundKey *xchain.Key // refunds the asset HTLC after T_seq

	ExpectAsset       string // SEQ asset hex the offer sells (required)
	ExpectSeqAmount   uint64 // atoms the offer promises (required)
	ExpectInvoiceMsat uint64 // BTC-LN we expect to receive (mint the invoice for this)

	MinSeqClaimWindow uint32 // refuse terms whose T_seq leaves less than this window (default 120)
	InvoiceCLTV       uint32 // min_final_cltv on the minted invoice (0 = node default)
	SpendFeeAtoms     uint64 // refund fee (default 1000)
	Timing            XcTiming
	Log               func(format string, args ...interface{})

	// OnAssetFunded is invoked the moment the asset HTLC is funded, so the caller
	// persists the leg + keys + secret before the (potentially long) settle wait.
	OnAssetFunded func(*TakerSubmarineResult)
}

// TakerSubmarineResult is returned even alongside an error once the asset HTLC is
// funded, so the caller can persist it and refund after T_seq.
type TakerSubmarineResult struct {
	Terms        *XcMsg
	SeqLeg       *xchain.LegLock
	SeqBlock     string
	SeqLocktime  uint32
	Bolt11       string
	InvoiceLabel string
	PaidMsat     uint64
	Settled      bool
}

func (p *TakerSubmarineParams) logf(format string, args ...interface{}) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// RunTakerSubmarineNormal executes the NORMAL submarine handshake as the taker:
// request terms, mint a BOLT11 on P, fund the asset HTLC (claim=maker), announce
// both, and wait for the invoice to be paid (its BTC-LN). If the maker never pays
// before T_seq, the caller refunds the asset HTLC (RefundTakerSubmarine).
func RunTakerSubmarineNormal(p TakerSubmarineParams, send XcSend, recv XcRecv) (*TakerSubmarineResult, error) {
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil || len(p.Secret) != 32 || p.SeqRefundKey == nil {
		return nil, errors.New("taker submarine: incomplete params")
	}
	if p.ExpectAsset == "" || p.ExpectSeqAmount == 0 || p.ExpectInvoiceMsat == 0 {
		return nil, errors.New("taker submarine: offer expectations required")
	}
	if p.MinSeqClaimWindow == 0 {
		p.MinSeqClaimWindow = 120
	}
	if p.SpendFeeAtoms == 0 {
		p.SpendFeeAtoms = 1000
	}
	res := &TakerSubmarineResult{}

	// 1. Request terms.
	if err := sendXc(&XcMsg{Type: XcSubTermsRequest}, p.Crypter, send); err != nil {
		return res, err
	}
	terms, err := recvXcType(recv, p.Crypter, XcSubTerms, p.Timing.TermsWait)
	if err != nil {
		return res, err
	}
	res.Terms = terms

	// 2. Validate terms against the signed offer.
	makerSeqClaimPub, err := hex.DecodeString(terms.MakerSeqClaimPub)
	if err != nil || len(makerSeqClaimPub) != 33 {
		sendXcFail(p.Crypter, send, "BAD_PUBKEY", "maker_seq_claim_pub must be 33-byte hex")
		return res, fmt.Errorf("%w: maker_seq_claim_pub", ErrXcBadTerms)
	}
	if terms.SeqAmount != p.ExpectSeqAmount {
		sendXcFail(p.Crypter, send, "BAD_AMOUNT", "seq_amount != offer")
		return res, fmt.Errorf("%w: seq_amount %d != offer %d", ErrXcBadTerms, terms.SeqAmount, p.ExpectSeqAmount)
	}
	seqTip, err := p.Ops.SeqTip()
	if err != nil {
		return res, err
	}
	if terms.SeqLocktime <= uint32(seqTip) || terms.SeqLocktime-uint32(seqTip) < p.MinSeqClaimWindow {
		sendXcFail(p.Crypter, send, "BAD_LOCKTIME", "seq_locktime leaves too small a refund window")
		return res, fmt.Errorf("%w: seq_locktime %d vs tip %d (min window %d)", ErrXcBadTerms, terms.SeqLocktime, seqTip, p.MinSeqClaimWindow)
	}
	res.SeqLocktime = terms.SeqLocktime

	// 3. Mint the BOLT11 on P (payment_hash = H); we receive its BTC-LN.
	h := sha256.Sum256(p.Secret)
	label := "seqob-sub-normal-" + hex.EncodeToString(h[:8])
	res.InvoiceLabel = label
	bolt11, err := p.Ops.MintInvoice(p.Secret, p.ExpectInvoiceMsat, p.InvoiceCLTV, label, "SeqOB submarine (asset -> BTC-LN)")
	if err != nil {
		return res, fmt.Errorf("taker submarine mint invoice: %w", err)
	}
	res.Bolt11 = bolt11

	// 4. Fund the asset HTLC (claim=maker, refund=taker).
	seqLeg, seqBlock, err := p.Ops.LockSEQLeg(makerSeqClaimPub, p.SeqRefundKey.PubKey(), atomsToCoins(p.ExpectSeqAmount), p.ExpectAsset, terms.SeqLocktime)
	if seqLeg != nil {
		res.SeqLeg = seqLeg
		res.SeqBlock = seqBlock
		if p.OnAssetFunded != nil {
			p.OnAssetFunded(res)
		}
	}
	if err != nil {
		return res, fmt.Errorf("taker submarine lock asset HTLC: %w", err)
	}
	p.logf("submarine taker: asset HTLC funded %s:%d in block %s", seqLeg.Funded.TxID, seqLeg.Funded.Vout, seqBlock)

	// 5. Announce the funded leg + the invoice.
	anchorHeight, _ := p.Ops.SeqBlockAnchorHeightOf(seqBlock)
	if err := sendXc(&XcMsg{
		Type:              XcSubAssetFunded,
		HashH:             hex.EncodeToString(h[:]),
		TakerSeqRefundPub: hex.EncodeToString(p.SeqRefundKey.PubKey()),
		Bolt11:            bolt11,
		Leg: &XcLeg{
			Txid:         seqLeg.Funded.TxID,
			Vout:         seqLeg.Funded.Vout,
			Amount:       seqLeg.Funded.Amount,
			Asset:        p.ExpectAsset,
			RedeemScript: hex.EncodeToString(seqLeg.Script),
			Locktime:     seqLeg.Locktime,
			BlockHash:    seqBlock,
			AnchorHeight: anchorHeight,
		},
	}, p.Crypter, send); err != nil {
		return res, err
	}

	// 6. Wait for our invoice to be paid (the BTC-LN we wanted). If it never pays
	// before the anchor+claim window, the caller refunds the asset HTLC after T_seq.
	paid, err := p.Ops.AwaitInvoicePaid(label, p.Timing.BtcConfWait)
	if err != nil {
		return res, fmt.Errorf("taker submarine await payment (refund asset after T_seq): %w", err)
	}
	res.PaidMsat = paid
	res.Settled = true
	p.logf("submarine taker: received %d msat over BTC-LN; asset claimed by maker", paid)
	return res, nil
}

// RefundTakerSubmarine reclaims the taker's asset HTLC via its CLTV branch after
// T_seq, for a NORMAL swap where the maker never paid. Mirrors RefundTakerBTC.
func RefundTakerSubmarine(ops SubTakerOps, leg *xchain.LegLock, key *xchain.Key, seqLocktime uint32, spendFeeAtoms uint64) (string, error) {
	tip, err := ops.SeqTip()
	if err != nil {
		return "", err
	}
	if uint32(tip) < seqLocktime {
		return "", fmt.Errorf("%w: seq tip %d < T_seq %d", ErrXcRefundNotDue, tip, seqLocktime)
	}
	return ops.RefundSEQLeg(leg, key, seqLocktime, spendFeeAtoms)
}

// chanRecv adapts a <-chan []byte to the XcRecv signature used by recvXcType.
func chanRecv(in <-chan []byte) XcRecv {
	return func(timeout time.Duration) ([]byte, error) {
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
}
