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
	// AssetLNNodeID returns the maker's asset LN node id (the taker pays the hold by
	// bare hash to it — the holdinvoice plugin holds by hash, no bolt11).
	AssetLNNodeID() (string, error)
	// LockBTCLeg funds the BTC HTLC (claim = takerClaimPub with P, refund = refundPub
	// after locktime). The maker funds it from its OWN BTC wallet.
	LockBTCLeg(takerClaimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error)
	// CreateAssetHold registers a HOLD on paymentHash h for amtMsat on the maker's
	// asset node (holdinvoice plugin). The taker pays the bare hash; no bolt11.
	CreateAssetHold(h []byte, amtMsat uint64) error
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
func (o *LiveSubAssetSellMakerOps) AssetLNNodeID() (string, error) { return o.AssetLN.NodeID() }
func (o *LiveSubAssetSellMakerOps) LockBTCLeg(takerClaimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	return o.Swap.LockBTCLeg(takerClaimPub, refundPub, amountCoins, locktime)
}
func (o *LiveSubAssetSellMakerOps) CreateAssetHold(h []byte, amtMsat uint64) error {
	label := "subassell-" + hex.EncodeToString(h[:8])
	_, err := o.AssetLN.CreateHoldInvoice(h, amtMsat, 0, label, "sub-asset SELL: maker buys the asset over LN")
	return err // bolt11 is null for this plugin; the taker pays the bare hash to our node
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
	// PayAsset pays the maker's held asset by BARE HASH to makerNodeID for amtMsat
	// (the holdinvoice plugin holds by hash, no bolt11) and BLOCKS until the maker
	// settles, returning the revealed preimage P.
	PayAsset(makerNodeID string, wantHash []byte, amtMsat uint64) (preimage []byte, err error)
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
func (o *LiveSubAssetSellTakerOps) PayAsset(makerNodeID string, wantHash []byte, amtMsat uint64) ([]byte, error) {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return o.AssetLN.PayHash(makerNodeID, wantHash, amtMsat, 18, secret)
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
	if p.MinBTCConf < 0 { // honor explicit 0-conf (LP fronts reorg risk)
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

	// 3. Register the asset HOLD on H and advertise both legs. The holdinvoice plugin
	//    holds by hash (no bolt11), so the taker pays the bare hash to our node id.
	if err := ops.CreateAssetHold(hashH, p.AssetAmount*1000); err != nil {
		sendXcFail(p.Crypter, send, "asset_hold", err.Error())
		return res, fmt.Errorf("subasset-sell maker: register asset hold (reclaim BTC at T_btc): %w", err)
	}
	nodeID, err := ops.AssetLNNodeID()
	if err != nil {
		sendXcFail(p.Crypter, send, "maker_node", err.Error())
		return res, fmt.Errorf("subasset-sell maker: asset node id: %w", err)
	}
	if err := sendXc(&XcMsg{
		Type:           XcSubAsSellTerms,
		HashH:          hex.EncodeToString(hashH),
		MakerLNNodeID:  nodeID,
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

// SubAssetSellState is the taker-side crash-recovery snapshot handed to
// TakerSubAssetSellParams.OnState. It carries H, the full BTC HTLC terms (everything the
// wallet needs to claim on-chain with its device key) and — once known — the revealed
// preimage P. A holder of a Phase=="paid" snapshot can reconstruct the settled swap result
// even if this process dies before RunTakerSubAssetSell returns (the whole point: close the
// window where the asset is paid + the maker settles but the caller never captures P).
type SubAssetSellState struct {
	Phase          string // "verified" (BTC HTLC checked; asset NOT yet paid) | "paid" (P learned)
	HashH          []byte
	Preimage       []byte // nil until Phase == "paid"
	BtcTxid        string
	BtcVout        uint32
	BtcAmount      uint64
	BtcRedeemHex   string
	BtcLocktime    uint32 // T_btc
	TakerClaimPub  []byte
	MakerRefundPub []byte
	MakerLNNodeID  string
}

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
	// ExternalClaimPub is the wallet's device BTC-claim pubkey. When set, the driver runs
	// LSP-side: it offers this pubkey in the courier, verifies the HTLC claim branch = it,
	// pays the asset over LN, and RETURNS P + the HTLC terms WITHOUT claiming on-chain — the
	// wallet holds the matching privkey and claims itself. Keeps the LSP non-custodial.
	ExternalClaimPub []byte
	// OnState, when non-nil, is invoked at each crash-recovery checkpoint so the caller can
	// persist the swap's recoverable state: once right AFTER the maker's BTC HTLC is verified
	// (Phase "verified", Preimage nil — the asset is not yet paid) and again right AFTER the
	// asset is paid and the maker reveals P (Phase "paid", Preimage set). Purely additive: a
	// nil OnState changes nothing. Lets the CLI mirror the BUY's -state-file so a taker crash
	// after paying (but before the caller captures P) can still be recovered.
	OnState func(SubAssetSellState)
	Timing  XcTiming
	Log     func(format string, args ...interface{})
}

type TakerSubAssetSellResult struct {
	HashH        []byte
	Preimage     []byte
	BtcClaimTxid string
	Received     bool
	// Terms echoed for the wallet to claim the BTC HTLC itself (device-claim mode). When
	// ExternalClaimPub was set, Preimage + these fields are returned and Received stays false
	// (the wallet claims on-chain with its device key); otherwise they are populated too but
	// the driver has already claimed (BtcClaimTxid set, Received true).
	MakerLNNodeID  string
	MakerRefundPub []byte
	TakerClaimPub  []byte
	BtcTxid        string
	BtcVout        uint32
	BtcRedeemHex   string
	BtcLocktime    uint32
}

func (p *TakerSubAssetSellParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// onState invokes the optional persist callback (no-op when nil).
func (p *TakerSubAssetSellParams) onState(s SubAssetSellState) {
	if p.OnState != nil {
		p.OnState(s)
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
	if p.MinBTCConf < 0 { // honor explicit 0-conf
		p.MinBTCConf = 1
	}
	if p.MinClaimWindow == 0 {
		p.MinClaimWindow = 6
	}
	if p.SpendFeeSats == 0 {
		p.SpendFeeSats = 1000
	}
	// Device-claim (LSP-side) mode: the wallet supplies its claim PUBKEY; we never hold the
	// privkey, so we cannot (and must not) claim the BTC — we return P + terms for the wallet.
	deviceClaim := len(p.ExternalClaimPub) > 0
	var claimPub []byte
	claimKey := p.BtcClaimKey
	if deviceClaim {
		claimPub = p.ExternalClaimPub
	} else {
		if claimKey == nil {
			var err error
			if claimKey, err = xchain.NewKey(); err != nil {
				return nil, fmt.Errorf("subasset-sell taker: mint claim key: %w", err)
			}
		}
		claimPub = claimKey.PubKey()
	}
	res := &TakerSubAssetSellResult{TakerClaimPub: claimPub}

	// 1. Request terms, offering our BTC claim pubkey (the HTLC claim branch = us).
	if err := sendXc(&XcMsg{Type: XcSubAsSellTermsRequest, TakerBtcClaimPub: hex.EncodeToString(claimPub)}, p.Crypter, send); err != nil {
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
	res.MakerRefundPub = makerRefundPub
	res.MakerLNNodeID = terms.MakerLNNodeID
	if terms.Leg == nil || terms.MakerLNNodeID == "" {
		return res, fmt.Errorf("%w: terms missing BTC leg or maker asset node id", ErrXcBadTerms)
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
	res.BtcTxid = terms.Leg.Txid
	res.BtcVout = terms.Leg.Vout
	res.BtcRedeemHex = terms.Leg.RedeemScript
	res.BtcLocktime = tBtc

	// Bind the settlement engine to the maker's H (VerifyBTCLeg/ClaimBTCLeg recompute
	// against it; PayAsset learns P by paying the invoice on H).
	ops := p.NewTakerOps(hashH)

	// 2. Verify the maker's on-chain BTC HTLC (H, claim=us, refund=maker, amount, confs)
	//    and a T_btc far enough out to claim after we learn P.
	var verified *xchain.VerifiedBTCLeg
	verifyDeadline := time.Now().Add(p.Timing.SeqLockWait)
	for {
		verified, err = ops.VerifyBTCLeg(hashH, claimPub, makerRefundPub, script,
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

	// Crash-recovery checkpoints: snapshot the full BTC HTLC terms now (asset NOT yet paid),
	// and again with P once the asset is paid. All res.* terms are populated by this point.
	// A nil OnState is a no-op; this lets the CLI persist a -state-file so a crash after paying
	// can still be recovered by the caller (the whole fund-safety point of this callback).
	persistState := func(phase string, preimage []byte) {
		p.onState(SubAssetSellState{
			Phase:          phase,
			HashH:          res.HashH,
			Preimage:       preimage,
			BtcTxid:        res.BtcTxid,
			BtcVout:        res.BtcVout,
			BtcAmount:      p.BtcAmount,
			BtcRedeemHex:   res.BtcRedeemHex,
			BtcLocktime:    res.BtcLocktime,
			TakerClaimPub:  res.TakerClaimPub,
			MakerRefundPub: res.MakerRefundPub,
			MakerLNNodeID:  res.MakerLNNodeID,
		})
	}
	persistState("verified", nil) // asset not yet paid; Preimage nil

	// 3. Pay the maker's held asset by bare hash over LN (device co-signs). Blocks until
	//    the maker settles, returning P. If the maker never settles, the LN payment fails
	//    back and nothing is lost.
	preimage, err := ops.PayAsset(terms.MakerLNNodeID, hashH, p.AssetAmount*1000)
	if err != nil {
		return res, fmt.Errorf("subasset-sell taker: pay asset invoice (nothing lost, LN returns): %w", err)
	}
	if gotH := sha256.Sum256(preimage); hex.EncodeToString(gotH[:]) != terms.HashH {
		return res, fmt.Errorf("subasset-sell taker: settled preimage does not hash to H")
	}
	res.Preimage = preimage
	persistState("paid", preimage) // P learned; asset is paid — enough to reconstruct the settled result

	// Device-claim (LSP-side) mode: we hold no claim privkey. Return P + the HTLC terms so
	// the wallet claims the BTC on-chain itself with its device key. The LSP stays custody-free.
	if deviceClaim {
		p.logf("subasset-sell taker: paid the asset + learned P; returning P+terms for the wallet to claim (LSP does not claim)")
		return res, nil
	}
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

// --- mixed same-chain ops (rails 7/8, SELL orientation): the on-chain leg is
// an ISSUED asset ------------------------------------------------------------
//
// The sub-asset SELL construction with the QUOTE asset standing in BTC's
// structural place: the maker LOCKS the quote asset in an on-chain HTLC on the
// SEQUENTIA chain (claim=taker with P, refund=maker) and receives the base
// asset over Lightning; the taker pays the base over LN (learning P) and
// claims the on-chain quote HTLC. Identical courier protocol and drivers —
// these ops re-point the chain-facing methods at the Sequentia chain + quote
// asset (xchain.NewSwapAsset).

// LiveSubAssetSellMakerOpsSeq is LiveSubAssetSellMakerOps for the mixed
// same-chain shape. Heights/locktimes are SEQUENTIA heights; LockBTCLeg funds
// the HTLC with the QUOTE asset from the node wallet behind seq.
type LiveSubAssetSellMakerOpsSeq struct {
	LiveSubAssetSellMakerOps
	Seq *xchain.Chain
}

// NewLiveSubAssetSellMakerOpsSeq builds the sell-maker's mixed same-chain ops
// over a HashLock that KNOWS P (NewHashLock), like the live ops it wraps.
func NewLiveSubAssetSellMakerOpsSeq(seq *xchain.Chain, quoteAsset string, assetLN xchain.LNLeg, preimage []byte) *LiveSubAssetSellMakerOpsSeq {
	return &LiveSubAssetSellMakerOpsSeq{
		LiveSubAssetSellMakerOps: LiveSubAssetSellMakerOps{
			Swap:    xchain.NewSwapAsset(seq, quoteAsset, xchain.NewHashLock(preimage)),
			AssetLN: assetLN,
		},
		Seq: seq,
	}
}

func (o *LiveSubAssetSellMakerOpsSeq) BtcTip() (int64, error) { return o.Seq.BlockCount() }
func (o *LiveSubAssetSellMakerOpsSeq) BtcConfirmations(txid string) (int, error) {
	return o.Seq.TxConfirmations(txid)
}

// LiveSubAssetSellTakerOpsSeq is LiveSubAssetSellTakerOps for the mixed
// same-chain shape: the taker verifies + claims the maker's on-chain HTLC on
// the QUOTE asset on the Sequentia chain.
type LiveSubAssetSellTakerOpsSeq struct {
	LiveSubAssetSellTakerOps
	Seq        *xchain.Chain
	QuoteAsset string
}

// NewLiveSubAssetSellTakerOpsSeq builds the sell-taker's mixed same-chain ops
// over a hash-only lock (the taker learns P by paying the asset over LN).
func NewLiveSubAssetSellTakerOpsSeq(seq *xchain.Chain, quoteAsset string, assetLN xchain.LNLeg, hashH []byte) *LiveSubAssetSellTakerOpsSeq {
	return &LiveSubAssetSellTakerOpsSeq{
		LiveSubAssetSellTakerOps: LiveSubAssetSellTakerOps{
			Swap:    xchain.NewSwapAsset(seq, quoteAsset, xchain.NewHashLockFromHash(hashH)),
			AssetLN: assetLN,
		},
		Seq:        seq,
		QuoteAsset: quoteAsset,
	}
}

func (o *LiveSubAssetSellTakerOpsSeq) BtcTip() (int64, error) { return o.Seq.BlockCount() }
func (o *LiveSubAssetSellTakerOpsSeq) VerifyBTCLeg(hashH, takerClaimPub, makerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	return o.Swap.VerifyBTCLeg(hashH, takerClaimPub, makerRefundPub, providedScript, btcLocktime, txid, vout, amount, o.QuoteAsset, minConf)
}
