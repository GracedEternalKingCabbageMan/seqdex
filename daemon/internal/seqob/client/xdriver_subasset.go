package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xdriver_subasset.go runs the SUB-ASSET swap over the opaque relay courier: a
// taker pays BTC ON-CHAIN and receives a Sequentia asset OVER LIGHTNING. It is the
// mirror of xdriver_submarine.go (asset on-chain <-> BTC over LN), built by
// gluing two proven primitives:
//   - the on-chain BTC HTLC leg from the `cross` mode (*xchain.Swap over a real
//     BitcoinChain: LockBTCLeg / VerifyBTCLeg / ClaimBTCLeg / RefundBTCLeg), and
//   - the asset Lightning hold-invoice leg from the `pureln` mode (an *xchain*
//     asset LNLeg: CreateHoldInvoice / WaitHeld / SettleHold / Pay).
//
// Preimage flow (one shared secret P, H = SHA256(P)):
//  1. Taker generates P and funds a BTC HTLC (claim = maker with P, refund = taker
//     after T_btc). It issues an asset hold invoice on H on its OWN asset LN node.
//  2. Maker verifies the funded, confirmed BTC HTLC matches the quote.
//  3. Maker PAYS the asset hold invoice; the payment is HELD at the taker's node
//     (the maker has not yet parted with anything it cannot recover, and does not
//     yet know P).
//  4. Taker sees the held asset payment and SETTLES the hold invoice with P,
//     receiving the asset. Settling reveals P to the maker (the maker's Pay call
//     returns P).
//  5. Maker claims the on-chain BTC HTLC with P.
//
// Safety: the BTC leg is on Bitcoin (the anchor), so no Sequentia-anchor-depth
// gate is needed on it -- Bitcoin confirmations are final by construction. The
// maker only pays the asset once the BTC HTLC is confirmed and T_btc is far
// enough out that it can claim after learning P. If the maker never pays, the
// taker refunds the BTC HTLC at T_btc; nothing is lost. The taker cannot both
// keep the asset and refund the BTC: settling reveals P, and the maker claims the
// BTC with P long before the taker's CLTV refund branch matures.

// --- ops seams --------------------------------------------------------------

// SubAssetMakerOps is the narrow settlement seam the maker driver runs against.
// LiveSubAssetMakerOps binds it to a real *xchain.Swap (BTC leg) + asset LNLeg;
// tests substitute a fake to exercise the handshake without RPC/LN.
type SubAssetMakerOps interface {
	// AssetLNNodeID returns the maker's asset LN node id (informational; the maker
	// PAYS the taker's invoice, so the taker never dials the maker).
	AssetLNNodeID() (string, error)
	// BtcTip returns the parent (Bitcoin) chain tip height.
	BtcTip() (int64, error)
	// VerifyBTCLeg checks the taker's funded BTC HTLC matches the agreed params
	// (H, claim=maker, refund=taker, amount, locktime) and has minConf confs.
	VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
		txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error)
	// PayAssetHold pays the taker's asset hold invoice bound to h for amtMsat and
	// BLOCKS until the taker settles it, returning the revealed preimage P.
	PayAssetHold(bolt11 string, h []byte, amtMsat uint64) (preimage []byte, err error)
	// InjectSecret feeds the learned preimage P into the BTC-leg hashlock so the
	// claim witness can be built (the maker built the swap with a hash-only lock).
	InjectSecret(preimage []byte) error
	// ClaimBTCLeg spends the verified BTC HTLC via the claim/IF branch with P.
	ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error)
}

// SubAssetTakerOps is the settlement seam the taker driver runs against.
type SubAssetTakerOps interface {
	BtcTip() (int64, error)
	BtcConfirmations(txid string) (int, error)
	// LockBTCLeg funds the BTC HTLC (claim=makerClaimPub with P, refund=refundPub
	// after locktime). Returns the funded leg and its confirmation height (0 =
	// broadcast-only on a live parent, poll BtcConfirmations to bury it).
	LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error)
	// PrepareAssetHold issues the asset hold invoice on H = SHA256(p) for amtMsat,
	// holding p secret. Returns the bolt11 and h.
	PrepareAssetHold(p []byte, amtMsat uint64) (bolt11 string, h []byte, err error)
	// WaitAssetHeld blocks until the maker's asset payment for h is accepted-and-
	// held at the taker's node (state "accepted"), or the deadline.
	WaitAssetHeld(h []byte, timeout time.Duration) error
	// SettleAssetHold releases p on the held asset invoice, receiving the asset and
	// revealing p to the maker.
	SettleAssetHold(h, preimage []byte) error
	// CancelAssetHold fails the held asset invoice back (maker never paid / abort).
	CancelAssetHold(h []byte) error
	// RefundBTCLeg reclaims the BTC HTLC via the refund/ELSE (CLTV) branch.
	RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error)
}

// --- live ops ---------------------------------------------------------------

// LiveSubAssetMakerOps binds the maker seam to a real BTC-leg swap + asset LN leg.
type LiveSubAssetMakerOps struct {
	Swap    *xchain.Swap    // built with xchain.NewSwapBitcoin (real BitcoinChain BTC leg)
	AssetLN xchain.LNLeg    // the maker's SeqLN-on-Sequentia asset node (pays the taker's invoice)
	BTC     *xchain.BitcoinChain
}

func (o *LiveSubAssetMakerOps) AssetLNNodeID() (string, error) { return o.AssetLN.NodeID() }
func (o *LiveSubAssetMakerOps) BtcTip() (int64, error)         { return o.BTC.BlockCount() }
func (o *LiveSubAssetMakerOps) VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	// assetID "" = real BTC on the parent chain.
	return o.Swap.VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript, btcLocktime, txid, vout, amount, "", minConf)
}
func (o *LiveSubAssetMakerOps) PayAssetHold(bolt11 string, h []byte, amtMsat uint64) ([]byte, error) {
	return o.AssetLN.Pay(bolt11, h, amtMsat)
}
func (o *LiveSubAssetMakerOps) InjectSecret(preimage []byte) error { return o.Swap.InjectSecret(preimage) }
func (o *LiveSubAssetMakerOps) ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error) {
	return o.Swap.ClaimBTCLeg(leg, claimKey, fee)
}

// LiveSubAssetTakerOps binds the taker seam to a real BTC-leg swap + asset LN leg.
type LiveSubAssetTakerOps struct {
	Swap    *xchain.Swap
	AssetLN xchain.LNLeg
	BTC     *xchain.BitcoinChain
}

func (o *LiveSubAssetTakerOps) BtcTip() (int64, error)                    { return o.BTC.BlockCount() }
func (o *LiveSubAssetTakerOps) BtcConfirmations(txid string) (int, error) { return o.BTC.Confirmations(txid) }
func (o *LiveSubAssetTakerOps) LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	return o.Swap.LockBTCLeg(claimPub, refundPub, amountCoins, locktime)
}
func (o *LiveSubAssetTakerOps) PrepareAssetHold(p []byte, amtMsat uint64) (string, []byte, error) {
	h := sha256.Sum256(p)
	label := "subas-" + hex.EncodeToString(h[:8])
	bolt11, err := o.AssetLN.CreateHoldInvoice(h[:], amtMsat, 0, label, "sub-asset swap: taker receives asset over LN")
	if err != nil {
		return "", nil, err
	}
	return bolt11, h[:], nil
}
func (o *LiveSubAssetTakerOps) WaitAssetHeld(h []byte, timeout time.Duration) error {
	_, err := o.AssetLN.WaitHeld(h, timeout)
	return err
}
func (o *LiveSubAssetTakerOps) SettleAssetHold(h, preimage []byte) error {
	return o.AssetLN.SettleHold(h, preimage)
}
func (o *LiveSubAssetTakerOps) CancelAssetHold(h []byte) error { return o.AssetLN.CancelHold(h) }
func (o *LiveSubAssetTakerOps) RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Swap.RefundBTCLeg(leg, refundKey, nLockTime, fee)
}

// --- maker ------------------------------------------------------------------

// MakerSubAssetParams configures RunMakerSubAsset. Amounts come from the SIGNED
// offer; the maker mints a fresh BTC claim key per lift and advertises its pubkey.
type MakerSubAssetParams struct {
	Ops          SubAssetMakerOps
	Crypter      *Crypter
	BtcAmount    uint64        // sats the taker locks on-chain (the maker receives)
	AssetAmount  uint64        // asset atoms the maker pays over Lightning
	BtcLocktime  uint32        // T_btc: the CLTV height for the taker's BTC refund branch
	MinBTCConf   int           // confirmations required on the taker's BTC leg (default 1)
	MinClaimWindow uint32      // reject if T_btc is within this many blocks of the tip (default 6)
	SpendFeeSats uint64        // BTC HTLC claim fee target in native sats (default 1000)
	HoldTimeout  time.Duration // how long to wait for the taker to settle after we pay (default 2m)
	MakerBtcClaimKey *xchain.Key // the key that claims the BTC HTLC (minted if nil)
	Timing       XcTiming
	Log          func(format string, args ...interface{})
}

type MakerSubAssetResult struct {
	HashH       []byte
	Preimage    []byte
	BtcClaimTxid string
	Settled     bool
}

func (p *MakerSubAssetParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// RunMakerSubAsset executes the sub-asset handshake as the maker: advertise terms
// (BTC claim pubkey + T_btc + amounts + asset LN node id), receive the taker's
// funded BTC HTLC + asset hold invoice, verify the BTC leg, pay the asset invoice
// (blocking until the taker settles -> learn P), and claim the BTC HTLC with P.
func RunMakerSubAsset(p MakerSubAssetParams, in <-chan []byte, send XcSend) (*MakerSubAssetResult, error) {
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil {
		return nil, fmt.Errorf("subasset maker: Ops and Crypter are required")
	}
	if p.MinBTCConf <= 0 {
		p.MinBTCConf = 1
	}
	if p.MinClaimWindow == 0 {
		p.MinClaimWindow = 6
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	if p.HoldTimeout <= 0 {
		p.HoldTimeout = 2 * time.Minute
	}
	claimKey := p.MakerBtcClaimKey
	if claimKey == nil {
		var err error
		claimKey, err = xchain.NewKey()
		if err != nil {
			return nil, fmt.Errorf("subasset maker: mint claim key: %w", err)
		}
	}
	recv := chanRecv(in)
	res := &MakerSubAssetResult{}

	// 1. Terms request -> advertise terms.
	if _, err := recvXcType(recv, p.Crypter, XcSubAsTermsRequest, p.Timing.TermsReqWait); err != nil {
		return res, err
	}
	assetNodeID, err := p.Ops.AssetLNNodeID()
	if err != nil {
		sendXcFail(p.Crypter, send, "maker_node", err.Error())
		return res, fmt.Errorf("subasset maker: asset LN node id: %w", err)
	}
	if err := sendXc(&XcMsg{
		Type:             XcSubAsTerms,
		MakerBtcClaimPub: hex.EncodeToString(claimKey.PubKey()),
		BtcLocktime:      p.BtcLocktime,
		BtcAmount:        p.BtcAmount,
		SeqAmount:        p.AssetAmount,
		MakerLNNodeID:    assetNodeID,
	}, p.Crypter, send); err != nil {
		return res, err
	}

	// 2. Receive the taker's funded BTC leg + asset hold invoice.
	funded, err := recvXcType(recv, p.Crypter, XcSubAsBtcFunded, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	hashH, err := hex.DecodeString(funded.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "bad_hash", "funded message carried a malformed hash")
		return res, fmt.Errorf("subasset maker: bad hash_h %q", funded.HashH)
	}
	res.HashH = hashH
	takerRefundPub, err := hex.DecodeString(funded.TakerBtcRefundPub)
	if err != nil || len(takerRefundPub) == 0 {
		sendXcFail(p.Crypter, send, "bad_pubkey", "malformed taker refund pubkey")
		return res, fmt.Errorf("subasset maker: bad taker_btc_refund_pub")
	}
	if funded.Leg == nil || funded.Bolt11 == "" {
		sendXcFail(p.Crypter, send, "bad_funded", "funded message missing leg or invoice")
		return res, fmt.Errorf("subasset maker: funded message missing leg/invoice")
	}
	script, err := hex.DecodeString(funded.Leg.RedeemScript)
	if err != nil {
		sendXcFail(p.Crypter, send, "bad_script", "malformed redeem script")
		return res, fmt.Errorf("subasset maker: bad btc redeem_script hex: %w", err)
	}

	// 3. Verify the on-chain BTC HTLC (H, claim=maker, refund=taker, amount, confs).
	//    The taker announces the instant ITS node sees MinBTCConf; ours can lag by
	//    propagation, so poll: only a proven-INVALID leg is terminal.
	var verified *xchain.VerifiedBTCLeg
	verifyDeadline := time.Now().Add(p.Timing.SeqLockWait)
	for {
		verified, err = p.Ops.VerifyBTCLeg(hashH, claimKey.PubKey(), takerRefundPub, script,
			p.BtcLocktime, funded.Leg.Txid, funded.Leg.Vout, funded.Leg.Amount, p.MinBTCConf)
		if err == nil {
			break
		}
		// A proven-INVALID leg is terminal; unconfirmed / not-yet-visible is polled
		// out until the deadline (the taker announces the instant ITS node sees the
		// confs; ours can lag by propagation).
		if errors.Is(err, xchain.ErrBTCLegInvalid) || time.Now().After(verifyDeadline) {
			sendXcFail(p.Crypter, send, "btc_leg_invalid", err.Error())
			return res, err
		}
		time.Sleep(p.Timing.Poll)
	}
	if funded.Leg.Amount != p.BtcAmount {
		sendXcFail(p.Crypter, send, "amount_mismatch", "btc leg amount != quote")
		return res, fmt.Errorf("subasset maker: btc leg %d != quote %d", funded.Leg.Amount, p.BtcAmount)
	}

	// 4. Claim-window guard: never pay the asset unless T_btc is far enough out to
	//    still claim the BTC after we learn P. On the anchor chain (Bitcoin) confs
	//    are final, so a comfortable block margin is the whole safety condition.
	tip, err := p.Ops.BtcTip()
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_tip", err.Error())
		return res, fmt.Errorf("subasset maker: btc tip: %w", err)
	}
	if p.BtcLocktime <= uint32(tip) || p.BtcLocktime-uint32(tip) < p.MinClaimWindow {
		sendXcFail(p.Crypter, send, "claim_window", "T_btc too close to tip to safely claim")
		return res, fmt.Errorf("subasset maker: T_btc %d within %d of tip %d; not paying", p.BtcLocktime, p.MinClaimWindow, tip)
	}
	p.logf("subasset maker: BTC HTLC %s verified (%d sats, T_btc=%d, tip=%d); paying asset over LN", funded.Leg.Txid, p.BtcAmount, p.BtcLocktime, tip)

	if err := sendXc(&XcMsg{Type: XcSubAsBtcVerified, HashH: funded.HashH}, p.Crypter, send); err != nil {
		return res, err
	}

	// 5. Pay the taker's asset hold invoice. This BLOCKS until the taker settles it
	//    with P; on settle we learn P. The payment is a HELD LN HTLC until then, so
	//    a taker that never settles simply times it out (nothing delivered).
	preimage, err := p.Ops.PayAssetHold(funded.Bolt11, hashH, p.AssetAmount*1000)
	if err != nil {
		sendXcFail(p.Crypter, send, "pay_asset", err.Error())
		return res, fmt.Errorf("subasset maker: pay asset hold invoice: %w", err)
	}
	// The revealed preimage must hash to H, or the taker cheated the settle.
	if gotH := sha256.Sum256(preimage); hex.EncodeToString(gotH[:]) != funded.HashH {
		sendXcFail(p.Crypter, send, "bad_preimage", "settled preimage does not hash to H")
		return res, fmt.Errorf("subasset maker: settled preimage does not hash to H")
	}
	res.Preimage = preimage
	p.logf("subasset maker: asset paid + settled; learned P; claiming BTC HTLC")

	// 6. Feed P into the BTC-leg hashlock, then claim the on-chain BTC HTLC with it.
	if err := p.Ops.InjectSecret(preimage); err != nil {
		return res, fmt.Errorf("subasset maker: inject secret (RETRYABLE, maker holds P): %w", err)
	}
	claimTxid, err := p.Ops.ClaimBTCLeg(verified.Leg, claimKey, xcSafeFee(p.SpendFeeSats, p.BtcAmount))
	if err != nil {
		// We hold P; this is retryable out of band (the leg is confirmed and ours
		// to claim until T_btc). Surface it, but the value is recoverable.
		return res, fmt.Errorf("subasset maker: claim BTC HTLC after paying asset (RETRYABLE, maker holds P): %w", err)
	}
	res.BtcClaimTxid = claimTxid
	res.Settled = true
	_ = sendXc(&XcMsg{Type: XcSubAsSettled, Preimage: hex.EncodeToString(preimage), SettleTxid: claimTxid}, p.Crypter, send)
	p.logf("subasset maker: SETTLED; claimed BTC HTLC in %s", claimTxid)
	return res, nil
}

// --- taker ------------------------------------------------------------------

// TakerSubAssetParams configures RunTakerSubAsset.
type TakerSubAssetParams struct {
	Ops          SubAssetTakerOps
	Crypter      *Crypter
	BtcAmount    uint64      // sats to lock on-chain (must match the offer)
	AssetAmount  uint64      // asset atoms to receive over LN (must match the offer)
	MinBTCConf   int         // confs to wait on our own BTC leg before announcing (default 1)
	SpendFeeSats uint64      // BTC HTLC refund fee target in native sats (default 1000)
	BtcRefundKey *xchain.Key // reclaims the BTC HTLC after T_btc (minted if nil)
	Preimage     []byte      // 32-byte secret P (minted if nil)
	// OnBtcLegFunded fires once the BTC HTLC is broadcast (before its confirmation
	// wait), so a CLI can persist the refund material (leg + T_btc) to disk before
	// anything downstream can fail. The result already carries BtcLeg/BtcLocktime.
	OnBtcLegFunded func(*TakerSubAssetResult)
	Timing         XcTiming
	Log            func(format string, args ...interface{})
}

type TakerSubAssetResult struct {
	HashH    []byte
	Preimage []byte
	// BtcLeg + BtcLocktime are retained so the caller can drive RefundSubAssetBTC
	// if the swap aborts after funding.
	BtcLeg      *xchain.LegLock
	BtcLocktime uint32
	Received    bool // the asset hold invoice was settled (the asset was received)
}

func (p *TakerSubAssetParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// RunTakerSubAsset executes the sub-asset handshake as the taker: request terms,
// fund the BTC HTLC (claim=maker with P, refund=taker after T_btc), issue an asset
// hold invoice on H, hand the maker the funded leg + invoice, wait for the maker's
// held asset payment, and SETTLE it with P (receiving the asset + revealing P).
//
// On any failure after funding, the returned result carries BtcLeg + BtcLocktime
// so the caller can refund the BTC HTLC at T_btc via RefundSubAssetBTC.
func RunTakerSubAsset(p TakerSubAssetParams, send XcSend, recv XcRecv) (*TakerSubAssetResult, error) {
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil {
		return nil, fmt.Errorf("subasset taker: Ops and Crypter are required")
	}
	if p.MinBTCConf <= 0 {
		p.MinBTCConf = 1
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	refundKey := p.BtcRefundKey
	if refundKey == nil {
		var err error
		refundKey, err = xchain.NewKey()
		if err != nil {
			return nil, fmt.Errorf("subasset taker: mint refund key: %w", err)
		}
	}
	secret := p.Preimage
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	res := &TakerSubAssetResult{}

	// 1. Request terms.
	if err := sendXc(&XcMsg{Type: XcSubAsTermsRequest}, p.Crypter, send); err != nil {
		return res, err
	}
	terms, err := recvXcType(recv, p.Crypter, XcSubAsTerms, p.Timing.TermsWait)
	if err != nil {
		return res, err
	}
	makerClaimPub, err := hex.DecodeString(terms.MakerBtcClaimPub)
	if err != nil || len(makerClaimPub) == 0 {
		return res, fmt.Errorf("%w: malformed maker_btc_claim_pub", ErrXcBadTerms)
	}
	if terms.BtcLocktime == 0 {
		return res, fmt.Errorf("%w: terms carried no T_btc", ErrXcBadTerms)
	}
	if terms.BtcAmount != 0 && terms.BtcAmount != p.BtcAmount {
		return res, fmt.Errorf("%w: maker BTC amount %d != expected %d", ErrXcBadTerms, terms.BtcAmount, p.BtcAmount)
	}
	if terms.SeqAmount != 0 && terms.SeqAmount != p.AssetAmount {
		return res, fmt.Errorf("%w: maker asset amount %d != expected %d", ErrXcBadTerms, terms.SeqAmount, p.AssetAmount)
	}
	// Refuse a T_btc that leaves no time to confirm our funding before it matures.
	tip, err := p.Ops.BtcTip()
	if err != nil {
		return res, fmt.Errorf("subasset taker: btc tip: %w", err)
	}
	if terms.BtcLocktime <= uint32(tip) {
		return res, fmt.Errorf("%w: T_btc %d already at/below tip %d", ErrXcBadTerms, terms.BtcLocktime, tip)
	}
	res.BtcLocktime = terms.BtcLocktime

	// 2. Fund the BTC HTLC (claim=maker with P, refund=us after T_btc).
	hashArr := sha256.Sum256(secret)
	hashH := hashArr[:]
	res.HashH = hashH
	p.logf("subasset taker: locking BTC HTLC: %d sats, T_btc=%d", p.BtcAmount, terms.BtcLocktime)
	btcLeg, hp, err := p.Ops.LockBTCLeg(makerClaimPub, refundKey.PubKey(), atomsToCoins(p.BtcAmount), terms.BtcLocktime)
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_lock_failed", err.Error())
		return res, fmt.Errorf("subasset taker: lock BTC leg: %w", err)
	}
	res.BtcLeg = btcLeg
	if p.OnBtcLegFunded != nil {
		p.OnBtcLegFunded(res)
	}

	// 3. Wait out our own confirmation on a live parent (broadcast-only -> hp==0).
	if hp <= 0 {
		confDeadline := time.Now().Add(p.Timing.BtcConfWait)
		for {
			confs, cerr := p.Ops.BtcConfirmations(btcLeg.Funded.TxID)
			if cerr == nil && confs >= p.MinBTCConf {
				break
			}
			if time.Now().After(confDeadline) {
				sendXcFail(p.Crypter, send, "btc_conf_timeout", "btc leg did not confirm in time")
				return res, fmt.Errorf("subasset taker: btc leg %s: no %d-conf within %s (refund after T_btc %d)",
					btcLeg.Funded.TxID, p.MinBTCConf, p.Timing.BtcConfWait, terms.BtcLocktime)
			}
			time.Sleep(p.Timing.Poll)
		}
	}
	p.logf("subasset taker: BTC HTLC %s confirmed", btcLeg.Funded.TxID)

	// 4. Issue the asset hold invoice on H (we hold P; released only when we settle).
	invoice, invH, err := p.Ops.PrepareAssetHold(secret, p.AssetAmount*1000)
	if err != nil {
		sendXcFail(p.Crypter, send, "asset_invoice", err.Error())
		return res, fmt.Errorf("subasset taker: prepare asset hold invoice (refund BTC at T_btc): %w", err)
	}
	if hex.EncodeToString(invH) != hex.EncodeToString(hashH) {
		return res, fmt.Errorf("subasset taker: hold invoice hash != H (internal)")
	}

	// 5. Announce the funded leg + invoice.
	if err := sendXc(&XcMsg{
		Type:              XcSubAsBtcFunded,
		HashH:             hex.EncodeToString(hashH),
		TakerBtcRefundPub: hex.EncodeToString(refundKey.PubKey()),
		Bolt11:            invoice,
		Leg: &XcLeg{
			Txid:         btcLeg.Funded.TxID,
			Vout:         btcLeg.Funded.Vout,
			Amount:       btcLeg.Funded.Amount,
			RedeemScript: hex.EncodeToString(btcLeg.Script),
			Locktime:     terms.BtcLocktime,
		},
	}, p.Crypter, send); err != nil {
		return res, err
	}

	// 6. Await the maker's "verified, about to pay" ack (best-effort; a failing
	//    maker instead couriers XcFail, which recvXcType surfaces).
	if _, err := recvXcType(recv, p.Crypter, XcSubAsBtcVerified, p.Timing.SeqLockWait); err != nil {
		_ = p.Ops.CancelAssetHold(hashH)
		return res, fmt.Errorf("subasset taker: maker did not verify the BTC leg (refund BTC at T_btc): %w", err)
	}

	// 7. Wait for the maker's asset payment to be HELD at our node, then SETTLE it
	//    with P -- receiving the asset and revealing P to the maker.
	p.logf("subasset taker: awaiting the maker's held asset payment on H")
	if err := p.Ops.WaitAssetHeld(hashH, p.Timing.SeqLockWait); err != nil {
		_ = p.Ops.CancelAssetHold(hashH)
		return res, fmt.Errorf("subasset taker: maker never paid the asset (refund BTC at T_btc): %w", err)
	}
	if err := p.Ops.SettleAssetHold(hashH, secret); err != nil {
		return res, fmt.Errorf("subasset taker: settle asset hold (asset payment held!): %w", err)
	}
	res.Preimage = secret
	res.Received = true
	p.logf("subasset taker: settled the asset hold with P; asset received (maker now claims BTC with P)")
	return res, nil
}

// RefundSubAssetBTC reclaims the taker's funded BTC HTLC via the CLTV refund branch
// after T_btc. Call it when RunTakerSubAsset returns a result with a non-nil BtcLeg
// and Received == false (the swap aborted before the asset was received).
func RefundSubAssetBTC(ops SubAssetTakerOps, leg *xchain.LegLock, refundKey *xchain.Key, btcLocktime uint32, spendFeeSats, btcAmount uint64) (string, error) {
	if spendFeeSats == 0 {
		spendFeeSats = 1000
	}
	return ops.RefundBTCLeg(leg, refundKey, btcLocktime, xcSafeFee(spendFeeSats, btcAmount))
}
