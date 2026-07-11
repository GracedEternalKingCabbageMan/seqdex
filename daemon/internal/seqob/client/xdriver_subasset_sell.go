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

// xdriver_subasset_sell.go runs the SUB-ASSET SELL swap: a taker pays an asset OVER
// LIGHTNING and receives BTC ON-CHAIN. It is the MIRROR of xdriver_subasset.go: the
// MAKER now locks the on-chain BTC HTLC and holds the preimage; the taker pays the
// asset over LN and claims the BTC.
//
// Preimage flow (one shared secret P, H = SHA256(P), MAKER-held):
//  1. Maker generates P, funds a BTC HTLC (claim = taker with P, refund = maker after
//     T_btc), and issues an asset HOLD invoice on H.
//  2. Taker verifies the on-chain BTC HTLC (claim=taker, amount, H, T_btc) and pays the
//     asset hold invoice over LN from its OWN asset node (device co-signs).
//  3. Maker's node holds the asset payment; the maker SETTLES it with P, taking the
//     asset and revealing P to the taker (paying a hold invoice returns P on settle).
//  4. Taker claims the on-chain BTC HTLC with P (device-signed).
//
// Safety: the BTC leg is on Bitcoin (the anchor) so no Sequentia-anchor gate is needed.
// The taker only pays the asset after verifying the maker's confirmed BTC HTLC and a
// T_btc far enough out to still claim after learning P. If the taker never pays, its LN
// payment never leaves (nothing lost) and the maker reclaims the BTC HTLC at T_btc.
// Non-custodial: the taker's LN spend + on-chain claim are device-keyed; neither leg
// is fronted by the LSP.

// --- maker ops --------------------------------------------------------------

// SubAssetSellMakerOps is the maker seam. The maker owns P: it funds the BTC HTLC,
// issues + settles the asset hold invoice, and reclaims the BTC on timeout.
type SubAssetSellMakerOps interface {
	BtcTip() (int64, error)
	BtcConfirmations(txid string) (int, error)
	// LockBTCLeg funds the BTC HTLC (claim = takerClaimPub with P, refund = refundPub
	// after locktime). The maker funds it from its OWN BTC wallet.
	LockBTCLeg(takerClaimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error)
	// CreateAssetHold issues the asset HOLD invoice on paymentHash h for amtMsat
	// (requires the holdinvoice plugin on the maker's asset node).
	CreateAssetHold(h []byte, amtMsat uint64) (bolt11 string, err error)
	// WaitAssetHeld blocks until the taker's asset payment for h is accepted-and-held.
	WaitAssetHeld(h []byte, timeout time.Duration) error
	// SettleAssetHold settles the held invoice with P, taking the asset + revealing P.
	SettleAssetHold(h, preimage []byte) error
	// CancelAssetHold fails the held invoice back (taker never paid / abort).
	CancelAssetHold(h []byte) error
	// RefundBTCLeg reclaims the maker's BTC HTLC via the CLTV refund branch.
	RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error)
}

// LiveSubAssetSellMakerOps binds the maker seam to a real BTC-leg swap + asset LN leg.
// Swap must be built with NewSwapBitcoin over a HashLock that KNOWS P (NewHashLock).
type LiveSubAssetSellMakerOps struct {
	Swap    *xchain.Swap
	AssetLN xchain.LNLeg
	BTC     *xchain.BitcoinChain
}

func (o *LiveSubAssetSellMakerOps) BtcTip() (int64, error) { return o.BTC.BlockCount() }
func (o *LiveSubAssetSellMakerOps) BtcConfirmations(txid string) (int, error) {
	return o.BTC.Confirmations(txid)
}
func (o *LiveSubAssetSellMakerOps) LockBTCLeg(takerClaimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	return o.Swap.LockBTCLeg(takerClaimPub, refundPub, amountCoins, locktime)
}
func (o *LiveSubAssetSellMakerOps) CreateAssetHold(h []byte, amtMsat uint64) (string, error) {
	label := "subassell-" + hex.EncodeToString(h[:8])
	return o.AssetLN.CreateHoldInvoice(h, amtMsat, 0, label, "sub-asset SELL: maker buys the asset over LN")
}
func (o *LiveSubAssetSellMakerOps) WaitAssetHeld(h []byte, timeout time.Duration) error {
	_, err := o.AssetLN.WaitHeld(h, timeout)
	return err
}
func (o *LiveSubAssetSellMakerOps) SettleAssetHold(h, preimage []byte) error {
	return o.AssetLN.SettleHold(h, preimage)
}
func (o *LiveSubAssetSellMakerOps) CancelAssetHold(h []byte) error { return o.AssetLN.CancelHold(h) }
func (o *LiveSubAssetSellMakerOps) RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Swap.RefundBTCLeg(leg, refundKey, nLockTime, fee)
}

// --- taker ops --------------------------------------------------------------

// SubAssetSellTakerOps is the taker seam. The taker verifies the maker's BTC HTLC,
// pays the asset (learning P), and claims the BTC.
type SubAssetSellTakerOps interface {
	BtcTip() (int64, error)
	VerifyBTCLeg(hashH, takerClaimPub, makerRefundPub, providedScript []byte, btcLocktime uint32,
		txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error)
	// PayAsset pays the maker's asset hold invoice (bolt11 bound to wantHash) for
	// amtMsat and BLOCKS until the maker settles it, returning the revealed preimage P.
	PayAsset(bolt11 string, wantHash []byte, amtMsat uint64) (preimage []byte, err error)
	// InjectSecret feeds P into the BTC-leg hashlock so the claim witness can be built.
	InjectSecret(preimage []byte) error
	// ClaimBTCLeg spends the maker's BTC HTLC via the claim/IF branch with P.
	ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error)
}

// LiveSubAssetSellTakerOps binds the taker seam. Swap is built with a hash-only lock
// (NewHashLockFromHash) — the taker learns P from paying the asset invoice.
type LiveSubAssetSellTakerOps struct {
	Swap    *xchain.Swap
	AssetLN xchain.LNLeg
	BTC     *xchain.BitcoinChain
}

func (o *LiveSubAssetSellTakerOps) BtcTip() (int64, error) { return o.BTC.BlockCount() }
func (o *LiveSubAssetSellTakerOps) VerifyBTCLeg(hashH, takerClaimPub, makerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	return o.Swap.VerifyBTCLeg(hashH, takerClaimPub, makerRefundPub, providedScript, btcLocktime, txid, vout, amount, "", minConf)
}
func (o *LiveSubAssetSellTakerOps) PayAsset(bolt11 string, wantHash []byte, amtMsat uint64) ([]byte, error) {
	return o.AssetLN.Pay(bolt11, wantHash, amtMsat)
}
func (o *LiveSubAssetSellTakerOps) InjectSecret(preimage []byte) error {
	return o.Swap.InjectSecret(preimage)
}
func (o *LiveSubAssetSellTakerOps) ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error) {
	return o.Swap.ClaimBTCLeg(leg, claimKey, fee)
}

// --- maker ------------------------------------------------------------------

type MakerSubAssetSellParams struct {
	// NewMakerOps binds the settlement engine to the maker's freshly-minted P (the
	// BTC-leg hashlock KNOWS P, and the asset hold invoice is on H = SHA256(P)).
	NewMakerOps    func(preimage []byte) SubAssetSellMakerOps
	Crypter        *Crypter
	BtcAmount      uint64        // sats the maker locks on-chain (the taker claims)
	AssetAmount    uint64        // asset atoms the maker receives over LN
	BtcLocktime    uint32        // T_btc: CLTV for the MAKER's refund branch
	MinBTCConf     int           // confs the maker waits on its own BTC funding before advertising (default 1)
	SpendFeeSats   uint64        // BTC refund fee target (native sats; default 1000)
	HoldTimeout    time.Duration // wait for the taker to pay the held invoice (default 5m)
	MakerRefundKey *xchain.Key   // reclaims the BTC HTLC after T_btc (minted if nil)
	Preimage       []byte        // 32-byte P (minted if nil)
	Timing         XcTiming
	Log            func(format string, args ...interface{})
}

type MakerSubAssetSellResult struct {
	HashH   []byte
	Settled bool // the maker took the asset (settled the hold with P)
	// For the refund path if the taker never pays.
	BtcLeg      *xchain.LegLock
	BtcLocktime uint32
}

func (p *MakerSubAssetSellParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// RunMakerSubAssetSell executes the sub-asset SELL handshake as the maker: receive the
// taker's BTC claim pubkey, fund a BTC HTLC (claim=taker, refund=maker), issue an asset
// hold invoice on H, advertise both, wait for the held asset payment, and settle it with
// P (taking the asset). On timeout the caller refunds the BTC HTLC via RefundSubAssetSellBTC.
func RunMakerSubAssetSell(p MakerSubAssetSellParams, in <-chan []byte, send XcSend) (*MakerSubAssetSellResult, error) {
	p.Timing.setDefaults()
	if p.NewMakerOps == nil || p.Crypter == nil {
		return nil, fmt.Errorf("subasset-sell maker: NewMakerOps and Crypter are required")
	}
	if p.MinBTCConf <= 0 {
		p.MinBTCConf = 1
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	if p.HoldTimeout <= 0 {
		p.HoldTimeout = 5 * time.Minute
	}
	refundKey := p.MakerRefundKey
	if refundKey == nil {
		var err error
		if refundKey, err = xchain.NewKey(); err != nil {
			return nil, fmt.Errorf("subasset-sell maker: mint refund key: %w", err)
		}
	}
	secret := p.Preimage
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	hashArr := sha256.Sum256(secret)
	hashH := hashArr[:]
	recv := chanRecv(in)
	res := &MakerSubAssetSellResult{HashH: hashH}

	// 1. Receive the taker's BTC claim pubkey (the HTLC claim branch = the taker).
	req, err := recvXcType(recv, p.Crypter, XcSubAsSellTermsRequest, p.Timing.TermsReqWait)
	if err != nil {
		return res, err
	}
	takerClaimPub, err := hex.DecodeString(req.TakerBtcClaimPub)
	if err != nil || len(takerClaimPub) == 0 {
		sendXcFail(p.Crypter, send, "bad_pubkey", "malformed taker_btc_claim_pub")
		return res, fmt.Errorf("subasset-sell maker: bad taker claim pubkey")
	}
	ops := p.NewMakerOps(secret)

	// 2. Fund the BTC HTLC (claim=taker with P, refund=maker after T_btc) from the
	//    maker's OWN BTC wallet, and wait out our own confirmation.
	p.logf("subasset-sell maker: locking BTC HTLC %d sats, T_btc=%d (claim=taker, refund=maker)", p.BtcAmount, p.BtcLocktime)
	btcLeg, hp, err := ops.LockBTCLeg(takerClaimPub, refundKey.PubKey(), atomsToCoins(p.BtcAmount), p.BtcLocktime)
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_lock_failed", err.Error())
		return res, fmt.Errorf("subasset-sell maker: lock BTC leg: %w", err)
	}
	res.BtcLeg = btcLeg
	res.BtcLocktime = p.BtcLocktime
	if hp <= 0 {
		confDeadline := time.Now().Add(p.Timing.BtcConfWait)
		for {
			confs, cerr := ops.BtcConfirmations(btcLeg.Funded.TxID)
			if cerr == nil && confs >= p.MinBTCConf {
				break
			}
			if time.Now().After(confDeadline) {
				sendXcFail(p.Crypter, send, "btc_conf_timeout", "maker btc funding did not confirm")
				return res, fmt.Errorf("subasset-sell maker: btc funding %s not %d-conf in time", btcLeg.Funded.TxID, p.MinBTCConf)
			}
			time.Sleep(p.Timing.Poll)
		}
	}
	p.logf("subasset-sell maker: BTC HTLC %s confirmed", btcLeg.Funded.TxID)

	// 3. Issue the asset HOLD invoice on H and advertise both legs.
	bolt11, err := ops.CreateAssetHold(hashH, p.AssetAmount*1000)
	if err != nil {
		sendXcFail(p.Crypter, send, "asset_invoice", err.Error())
		return res, fmt.Errorf("subasset-sell maker: create asset hold invoice (refund BTC at T_btc): %w", err)
	}
	if err := sendXc(&XcMsg{
		Type:           XcSubAsSellTerms,
		HashH:          hex.EncodeToString(hashH),
		Bolt11:         bolt11,
		BtcAmount:      p.BtcAmount,
		SeqAmount:      p.AssetAmount,
		MakerRefundPub: hex.EncodeToString(refundKey.PubKey()),
		Leg: &XcLeg{
			Txid:         btcLeg.Funded.TxID,
			Vout:         btcLeg.Funded.Vout,
			Amount:       btcLeg.Funded.Amount,
			RedeemScript: hex.EncodeToString(btcLeg.Script),
			Locktime:     btcLeg.Locktime,
		},
	}, p.Crypter, send); err != nil {
		return res, err
	}

	// 4. Wait for the taker's asset payment to be HELD, then SETTLE it with P — taking
	//    the asset and revealing P to the taker (who then claims the BTC).
	p.logf("subasset-sell maker: awaiting the taker's held asset payment on H")
	if err := ops.WaitAssetHeld(hashH, p.HoldTimeout); err != nil {
		_ = ops.CancelAssetHold(hashH)
		return res, fmt.Errorf("subasset-sell maker: taker never paid the asset (reclaim BTC at T_btc): %w", err)
	}
	if err := ops.SettleAssetHold(hashH, secret); err != nil {
		return res, fmt.Errorf("subasset-sell maker: settle asset hold (payment held!): %w", err)
	}
	res.Settled = true
	_ = sendXc(&XcMsg{Type: XcSubAsSellSettled}, p.Crypter, send)
	p.logf("subasset-sell maker: settled the asset hold with P; took the asset (taker now claims the BTC with P)")
	return res, nil
}

// RefundSubAssetSellBTC reclaims the maker's funded BTC HTLC via the CLTV refund branch
// after T_btc, when the taker never paid.
func RefundSubAssetSellBTC(ops SubAssetSellMakerOps, leg *xchain.LegLock, refundKey *xchain.Key, btcLocktime uint32, spendFeeSats, btcAmount uint64) (string, error) {
	if spendFeeSats == 0 {
		spendFeeSats = 1000
	}
	return ops.RefundBTCLeg(leg, refundKey, btcLocktime, xcSafeFee(spendFeeSats, btcAmount))
}

// --- taker ------------------------------------------------------------------

type TakerSubAssetSellParams struct {
	// NewTakerOps binds the settlement engine to the maker's H once Terms arrive — the
	// BTC-leg hashlock must embed H for VerifyBTCLeg/ClaimBTCLeg to recompute + match it.
	NewTakerOps    func(hashH []byte) SubAssetSellTakerOps
	Crypter        *Crypter
	BtcAmount      uint64      // sats to receive on-chain (must match the offer)
	AssetAmount    uint64      // asset atoms to pay over LN (must match the offer)
	MinBTCConf     int         // confs required on the maker's BTC HTLC before paying (default 1)
	MinClaimWindow uint32      // require T_btc at least this many blocks past tip before paying (default 6)
	SpendFeeSats   uint64      // BTC claim fee target (native sats; default 1000)
	BtcClaimKey    *xchain.Key // claims the BTC HTLC with P (device-keyed; minted if nil)
	Timing         XcTiming
	Log            func(format string, args ...interface{})
}

type TakerSubAssetSellResult struct {
	HashH        []byte
	Preimage     []byte
	BtcClaimTxid string
	Received     bool
}

func (p *TakerSubAssetSellParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// RunTakerSubAssetSell executes the sub-asset SELL handshake as the taker: send its BTC
// claim pubkey, receive the maker's asset hold invoice + funded BTC HTLC, verify the
// HTLC, pay the asset invoice over LN (learning P when the maker settles), and claim the
// BTC HTLC with P. If the maker never settles, the LN payment simply returns — nothing lost.
func RunTakerSubAssetSell(p TakerSubAssetSellParams, send XcSend, recv XcRecv) (*TakerSubAssetSellResult, error) {
	p.Timing.setDefaults()
	if p.NewTakerOps == nil || p.Crypter == nil {
		return nil, fmt.Errorf("subasset-sell taker: NewTakerOps and Crypter are required")
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
	claimKey := p.BtcClaimKey
	if claimKey == nil {
		var err error
		if claimKey, err = xchain.NewKey(); err != nil {
			return nil, fmt.Errorf("subasset-sell taker: mint claim key: %w", err)
		}
	}
	res := &TakerSubAssetSellResult{}

	// 1. Request terms, offering our BTC claim pubkey (the HTLC claim branch = us).
	if err := sendXc(&XcMsg{Type: XcSubAsSellTermsRequest, TakerBtcClaimPub: hex.EncodeToString(claimKey.PubKey())}, p.Crypter, send); err != nil {
		return res, err
	}
	terms, err := recvXcType(recv, p.Crypter, XcSubAsSellTerms, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	hashH, err := hex.DecodeString(terms.HashH)
	if err != nil || len(hashH) != 32 {
		return res, fmt.Errorf("%w: malformed hash_h", ErrXcBadTerms)
	}
	res.HashH = hashH
	makerRefundPub, err := hex.DecodeString(terms.MakerRefundPub)
	if err != nil || len(makerRefundPub) == 0 {
		return res, fmt.Errorf("%w: malformed maker_refund_pub", ErrXcBadTerms)
	}
	if terms.Leg == nil || terms.Bolt11 == "" {
		return res, fmt.Errorf("%w: terms missing leg or invoice", ErrXcBadTerms)
	}
	if terms.BtcAmount != 0 && terms.BtcAmount != p.BtcAmount {
		return res, fmt.Errorf("%w: maker BTC amount %d != expected %d", ErrXcBadTerms, terms.BtcAmount, p.BtcAmount)
	}
	if terms.SeqAmount != 0 && terms.SeqAmount != p.AssetAmount {
		return res, fmt.Errorf("%w: maker asset amount %d != expected %d", ErrXcBadTerms, terms.SeqAmount, p.AssetAmount)
	}
	script, err := hex.DecodeString(terms.Leg.RedeemScript)
	if err != nil {
		return res, fmt.Errorf("subasset-sell taker: bad redeem script: %w", err)
	}
	tBtc := terms.Leg.Locktime

	// Bind the settlement engine to the maker's H (VerifyBTCLeg/ClaimBTCLeg recompute
	// against it; PayAsset learns P by paying the invoice on H).
	ops := p.NewTakerOps(hashH)

	// 2. Verify the maker's on-chain BTC HTLC (H, claim=us, refund=maker, amount, confs)
	//    and a T_btc far enough out to claim after we learn P.
	var verified *xchain.VerifiedBTCLeg
	verifyDeadline := time.Now().Add(p.Timing.SeqLockWait)
	for {
		verified, err = ops.VerifyBTCLeg(hashH, claimKey.PubKey(), makerRefundPub, script,
			tBtc, terms.Leg.Txid, terms.Leg.Vout, terms.Leg.Amount, p.MinBTCConf)
		if err == nil {
			break
		}
		if errors.Is(err, xchain.ErrBTCLegInvalid) || time.Now().After(verifyDeadline) {
			sendXcFail(p.Crypter, send, "btc_leg_invalid", err.Error())
			return res, fmt.Errorf("subasset-sell taker: maker BTC HTLC invalid/unconfirmed: %w", err)
		}
		time.Sleep(p.Timing.Poll)
	}
	if terms.Leg.Amount != p.BtcAmount {
		return res, fmt.Errorf("subasset-sell taker: btc leg %d != quote %d", terms.Leg.Amount, p.BtcAmount)
	}
	tip, err := ops.BtcTip()
	if err != nil {
		return res, fmt.Errorf("subasset-sell taker: btc tip: %w", err)
	}
	if tBtc <= uint32(tip) || tBtc-uint32(tip) < p.MinClaimWindow {
		return res, fmt.Errorf("subasset-sell taker: T_btc %d within %d of tip %d; not paying", tBtc, p.MinClaimWindow, tip)
	}
	p.logf("subasset-sell taker: maker BTC HTLC %s verified (%d sats, T_btc=%d); paying the asset over LN", terms.Leg.Txid, p.BtcAmount, tBtc)

	// 3. Pay the maker's asset hold invoice over LN (device co-signs). Blocks until the
	//    maker settles it, returning P. If the maker never settles, the LN payment fails
	//    back and nothing is lost.
	preimage, err := ops.PayAsset(terms.Bolt11, hashH, p.AssetAmount*1000)
	if err != nil {
		return res, fmt.Errorf("subasset-sell taker: pay asset invoice (nothing lost, LN returns): %w", err)
	}
	if gotH := sha256.Sum256(preimage); hex.EncodeToString(gotH[:]) != terms.HashH {
		return res, fmt.Errorf("subasset-sell taker: settled preimage does not hash to H")
	}
	res.Preimage = preimage
	p.logf("subasset-sell taker: paid the asset + learned P; claiming the BTC HTLC on-chain")

	// 4. Claim the maker's BTC HTLC with P (device-signed).
	if err := ops.InjectSecret(preimage); err != nil {
		return res, fmt.Errorf("subasset-sell taker: inject secret (RETRYABLE, taker holds P): %w", err)
	}
	claimTxid, err := ops.ClaimBTCLeg(verified.Leg, claimKey, xcSafeFee(p.SpendFeeSats, p.BtcAmount))
	if err != nil {
		return res, fmt.Errorf("subasset-sell taker: claim BTC HTLC (RETRYABLE, taker holds P): %w", err)
	}
	res.BtcClaimTxid = claimTxid
	res.Received = true
	p.logf("subasset-sell taker: claimed BTC on-chain in %s (asset paid over LN, BTC received)", claimTxid)
	return res, nil
}
