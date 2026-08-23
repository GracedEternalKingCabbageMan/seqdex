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
// tests substitute a fake to exercise the handshake without RPC/LN. It is built
// per-swap by NewMakerOps once the taker's H is known (the BTC-leg hashlock must
// embed H for VerifyBTCLeg/ClaimBTCLeg).
type SubAssetMakerOps interface {
	// BtcTip returns the parent (Bitcoin) chain tip height.
	BtcTip() (int64, error)
	// VerifyBTCLeg checks the taker's funded BTC HTLC matches the agreed params
	// (H, claim=maker, refund=taker, amount, locktime) and has minConf confs.
	VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
		txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error)
	// PayAssetHold pays the taker's asset hold invoice bound to h for amtMsat and
	// BLOCKS until the taker settles it, returning the revealed preimage P.
	PayAssetHold(bolt11 string, h []byte, amtMsat uint64, maxDelay uint32) (preimage []byte, err error)
	// PayAssetHashHold pays the taker's HELD invoice by BARE HASH to takerNodeID
	// (the taker's device registered a hold on h at its OWN hosted node; there is no
	// bolt11), BLOCKS until the DEVICE settles, and returns the revealed preimage P.
	// It is the maker's mirror of the SELL taker's PayAsset (pay-by-hash to a hold).
	PayAssetHashHold(takerNodeID string, h []byte, amtMsat uint64) (preimage []byte, err error)
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
	// WaitAssetPaid blocks until the asset invoice with payment_hash h is PAID on
	// the receiving node. Used by the DEVICE-PREIMAGE (external-invoice) mode: the
	// invoice was created out-of-band by the device with its OWN preimage, so the
	// driver never mints/settles it — it only observes that the maker's payment
	// arrived (the device's node auto-settles, revealing P to the maker).
	WaitAssetPaid(h []byte, timeout time.Duration) error
	// RefundBTCLeg reclaims the BTC HTLC via the refund/ELSE (CLTV) branch.
	RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error)
}

// --- live ops ---------------------------------------------------------------

// LiveSubAssetMakerOps binds the maker seam to a real BTC-leg swap + asset LN leg.
type LiveSubAssetMakerOps struct {
	Swap    *xchain.Swap // built with xchain.NewSwapBitcoin (real BitcoinChain BTC leg)
	AssetLN xchain.LNLeg // the maker's SeqLN-on-Sequentia asset node (pays the taker's invoice)
	BTC     *xchain.BitcoinChain
}

func (o *LiveSubAssetMakerOps) AssetLNNodeID() (string, error) { return o.AssetLN.NodeID() }
func (o *LiveSubAssetMakerOps) BtcTip() (int64, error)         { return o.BTC.BlockCount() }

// NewLiveSubAssetMakerOps builds the maker's live ops for a swap whose taker H is
// hashH: the BTC-leg swap embeds hashH so VerifyBTCLeg/ClaimBTCLeg recompute and
// match it. seq is unused (the asset leg is over LN, not on-chain) and may be nil.
func NewLiveSubAssetMakerOps(btc *xchain.BitcoinChain, assetLN xchain.LNLeg, hashH []byte) *LiveSubAssetMakerOps {
	return &LiveSubAssetMakerOps{
		Swap:    xchain.NewSwapBitcoin(btc, nil, xchain.NewHashLockFromHash(hashH)),
		AssetLN: assetLN,
		BTC:     btc,
	}
}
func (o *LiveSubAssetMakerOps) VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	// assetID "" = real BTC on the parent chain.
	return o.Swap.VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript, btcLocktime, txid, vout, amount, "", minConf)
}
func (o *LiveSubAssetMakerOps) PayAssetHold(bolt11 string, h []byte, amtMsat uint64, maxDelay uint32) ([]byte, error) {
	capped, ok := o.AssetLN.(interface {
		PayCapped(string, []byte, uint64, uint32) ([]byte, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: asset leg cannot cap its timelock", xchain.ErrCltvUncapped)
	}
	return capped.PayCapped(bolt11, h, amtMsat, maxDelay)
}

// PayAssetHashHold pays the taker's HELD invoice by BARE HASH to takerNodeID: the
// taker's device pre-registered a hold on h at its own hosted node (no bolt11), so
// the maker routes a hash-locked payment there and blocks (through the HELD state)
// until the device settles, learning P. Mirror of LiveSubAssetSellTakerOps.PayAsset.
func (o *LiveSubAssetMakerOps) PayAssetHashHold(takerNodeID string, h []byte, amtMsat uint64) ([]byte, error) {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return o.AssetLN.PayHash(takerNodeID, h, amtMsat, 18, secret)
}

// SubAssetPayMarginSecs is the time the sub-asset BUY maker keeps between the
// latest its asset payment may resolve and T_btc: learning P and claiming the BTC
// on-chain before the taker's refund branch opens.
const SubAssetPayMarginSecs = 60 * 60

// subAssetPayCap is the asset-leg (Sequentia-block) timelock cap for paying the
// taker's hold while its BTC HTLC refunds at tBtc, btcRemaining blocks away.
func subAssetPayCap(btcRemaining uint32) uint32 {
	return xchain.CapDelay(btcRemaining, xchain.BTCTiming, xchain.SeqTiming, SubAssetPayMarginSecs)
}
func (o *LiveSubAssetMakerOps) InjectSecret(preimage []byte) error {
	return o.Swap.InjectSecret(preimage)
}
func (o *LiveSubAssetMakerOps) ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error) {
	return o.Swap.ClaimBTCLeg(leg, claimKey, fee)
}

// LiveSubAssetTakerOps binds the taker seam to a real BTC-leg swap + asset LN leg.
//
// The asset LN invoice has two equally-atomic modes, selected by Plain:
//   - HOLD (Plain=false, the directive's shape): a holdinvoice-plugin invoice on
//     H. The maker's payment is HELD at the taker's node; the taker explicitly
//     settles with P, releasing it. Needs the holdinvoice plugin on the asset node.
//   - PLAIN (Plain=true): a bare BOLT11 whose payment_hash = H, preimage = P (the
//     taker holds P). The maker's payment auto-settles at the taker's node,
//     revealing P to the maker. Needs no plugin. This is safe here because the
//     ON-CHAIN BTC HTLC is the hold (hashlock + CLTV): the maker cannot claim the
//     BTC without P, and P is only revealed by the maker paying (delivering) the
//     asset. It is exactly how the deployed pure-LN BUY pays the asset leg.
type LiveSubAssetTakerOps struct {
	Swap    *xchain.Swap
	AssetLN xchain.LNLeg
	BTC     *xchain.BitcoinChain
	Plain   bool // true = plain BOLT11 (no holdinvoice plugin); false = hold invoice

	label string // the plain invoice's label, for WaitInvoicePaid
}

func (o *LiveSubAssetTakerOps) BtcTip() (int64, error) { return o.BTC.BlockCount() }
func (o *LiveSubAssetTakerOps) BtcConfirmations(txid string) (int, error) {
	return o.BTC.Confirmations(txid)
}
func (o *LiveSubAssetTakerOps) LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	return o.Swap.LockBTCLeg(claimPub, refundPub, amountCoins, locktime)
}
func (o *LiveSubAssetTakerOps) PrepareAssetHold(p []byte, amtMsat uint64) (string, []byte, error) {
	h := sha256.Sum256(p)
	o.label = "subas-" + hex.EncodeToString(h[:8])
	if o.Plain {
		// A plain BOLT11 on preimage P (payment_hash = H); it auto-settles when paid.
		bolt11, err := o.AssetLN.CreateInvoice(p, amtMsat, 0, o.label, "sub-asset swap: taker receives asset over LN")
		if err != nil {
			return "", nil, err
		}
		return bolt11, h[:], nil
	}
	bolt11, err := o.AssetLN.CreateHoldInvoice(h[:], amtMsat, 0, o.label, "sub-asset swap: taker receives asset over LN")
	if err != nil {
		return "", nil, err
	}
	return bolt11, h[:], nil
}
func (o *LiveSubAssetTakerOps) WaitAssetHeld(h []byte, timeout time.Duration) error {
	if o.Plain {
		// A plain invoice auto-settles; "held" == "paid" (the asset has arrived and
		// P is already revealed to the maker). Block until the maker pays it.
		_, err := o.AssetLN.WaitInvoicePaid(o.label, timeout)
		return err
	}
	_, err := o.AssetLN.WaitHeld(h, timeout)
	return err
}
func (o *LiveSubAssetTakerOps) SettleAssetHold(h, preimage []byte) error {
	if o.Plain {
		return nil // already auto-settled by the payment
	}
	return o.AssetLN.SettleHold(h, preimage)
}
func (o *LiveSubAssetTakerOps) CancelAssetHold(h []byte) error {
	if o.Plain {
		return nil // nothing to cancel; an unpaid plain invoice simply expires
	}
	return o.AssetLN.CancelHold(h)
}
func (o *LiveSubAssetTakerOps) WaitAssetPaid(h []byte, timeout time.Duration) error {
	_, err := o.AssetLN.WaitPaidByHash(h, timeout)
	return err
}
func (o *LiveSubAssetTakerOps) RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return o.Swap.RefundBTCLeg(leg, refundKey, nLockTime, fee)
}

// --- maker ------------------------------------------------------------------

// MakerSubAssetParams configures RunMakerSubAsset. Amounts come from the SIGNED
// offer; the maker mints a fresh BTC claim key per lift and advertises its pubkey.
type MakerSubAssetParams struct {
	// NewMakerOps binds the settlement engine (BTC-leg swap + asset LN) to the
	// taker's H once it arrives — the maker never knows H until the taker announces
	// its funded BTC HTLC, and the BTC-leg hashlock must embed H.
	NewMakerOps      func(hashH []byte) SubAssetMakerOps
	AssetLNNodeID    string // advisory: the maker's asset LN node id, put in Terms
	Crypter          *Crypter
	BtcAmount        uint64        // sats the taker locks on-chain (the maker receives)
	AssetAmount      uint64        // asset atoms the maker pays over Lightning
	BtcLocktime      uint32        // T_btc: the CLTV height for the taker's BTC refund branch
	MinBTCConf       int           // confirmations required on the taker's BTC leg (default 1)
	MinClaimWindow   uint32        // reject if T_btc is within this many blocks of the tip (default 6)
	MaxClaimWindow   uint32        // reject if T_btc is more than this many blocks past the tip (0 = no bound)
	SpendFeeSats     uint64        // BTC HTLC claim fee target in native sats (default 1000)
	HoldTimeout      time.Duration // how long to wait for the taker to settle after we pay (default 2m)
	MakerBtcClaimKey *xchain.Key   // the key that claims the BTC HTLC (minted if nil)
	Timing           XcTiming
	Log              func(format string, args ...interface{})
}

type MakerSubAssetResult struct {
	HashH        []byte
	Preimage     []byte
	BtcClaimTxid string
	Settled      bool
	// Partial fills (T8): the asset atoms + BTC sats this lift ACTUALLY took (<= the
	// offer). For a whole-offer lift these equal the offer amounts; the maker's serve
	// loop re-rests the remainder (offer - filled) when they are smaller.
	FilledAsset uint64
	FilledBtc   uint64
}

// ProportionalBtc is the BTC (sats) owed for taking `takeAsset` atoms out of an offer
// of `wholeAsset` atoms priced at `wholeBtc` sats. Rounded UP so a partial taker never
// underpays the maker's price (a sub-atom rounding always favors the maker). Both the
// taker (what to lock) and the maker (what to require) compute it identically.
func ProportionalBtc(wholeBtc, takeAsset, wholeAsset uint64) uint64 {
	if wholeAsset == 0 || takeAsset >= wholeAsset {
		return wholeBtc
	}
	// ceil(wholeBtc*takeAsset / wholeAsset) with a 128-bit product. A bare uint64 multiply overflows
	// for realistic sizes (e.g. 1e8 sats * 2.1e15 atoms = 2.1e23 >> 2^64), silently returning a tiny
	// price — which, since taker and maker agree on the same wrapped value, would let a partial taker
	// pay a few sats for a large asset slice. mulDiv64 does the multiply in 128 bits (see proRata); the
	// early return guarantees takeAsset < wholeAsset, so the quotient is < wholeBtc and bits.Div64 never
	// overflows. rem!=0 rounds up so a partial never underpays.
	q, rem := mulDiv64(wholeBtc, takeAsset, wholeAsset)
	if rem != 0 {
		q++
	}
	return q
}

// ProportionalBtcFloor is the BTC (sats) owed for taking `takeAsset` atoms out of an offer
// of `wholeAsset` atoms priced at `wholeBtc` sats, rounded DOWN. It is the maker's-favour
// direction when the MAKER GIVES the BTC (a REVERSE cross BUY: the maker pays BTC to acquire
// the asset), so a partial fill never makes the maker pay MORE than its offer's exact ratio.
// The reverse serve loop then re-rests the remainder by SUBTRACTING the exact filled sats
// (remaining_btc = offer_btc - filled_btc), so a full sweep of partials commits at most the
// offer's original BTC and never over-commits the maker's capital (the ceil variant, summed
// over partials, would exceed it). Rounding down can leave the counterparty a sub-atom short,
// so the caller MUST reject a floor of 0 (dust / free-drain) — every value-move fails closed.
// Uses the same 128-bit mulDiv64 as ProportionalBtc, so realistic sizes never wrap uint64.
func ProportionalBtcFloor(wholeBtc, takeAsset, wholeAsset uint64) uint64 {
	if wholeAsset == 0 || takeAsset >= wholeAsset {
		return wholeBtc
	}
	q, _ := mulDiv64(wholeBtc, takeAsset, wholeAsset)
	return q
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
	if p.NewMakerOps == nil || p.Crypter == nil {
		return nil, fmt.Errorf("subasset maker: NewMakerOps and Crypter are required")
	}
	// MinBTCConf 0 is honored explicitly (0-conf: the maker fronts the Bitcoin
	// reorg risk, like the submarine's max-0conf LP-fronting); only a negative
	// value falls back to the safe 1-conf default.
	if p.MinBTCConf < 0 {
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

	// 1. Terms request -> advertise terms (the asset LN node id is advisory: the
	//    maker PAYS the taker, so the taker never dials it).
	if _, err := recvXcType(recv, p.Crypter, XcSubAsTermsRequest, p.Timing.TermsReqWait); err != nil {
		return res, err
	}
	if err := sendXc(&XcMsg{
		Type:             XcSubAsTerms,
		MakerBtcClaimPub: hex.EncodeToString(claimKey.PubKey()),
		BtcLocktime:      p.BtcLocktime,
		BtcAmount:        p.BtcAmount,
		SeqAmount:        p.AssetAmount,
		MakerLNNodeID:    p.AssetLNNodeID,
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
	// HODL BUY: the taker's device registered a hold on H at its OWN hosted node and
	// relays that node id instead of a bolt11 (the maker pays the bare hash to it).
	hodl := funded.TakerLNNodeID != ""
	if funded.Leg == nil || (funded.Bolt11 == "" && !hodl) {
		sendXcFail(p.Crypter, send, "bad_funded", "funded message missing leg or invoice/node-id")
		return res, fmt.Errorf("subasset maker: funded message missing leg and (bolt11 or taker node id)")
	}
	script, err := hex.DecodeString(funded.Leg.RedeemScript)
	if err != nil {
		sendXcFail(p.Crypter, send, "bad_script", "malformed redeem script")
		return res, fmt.Errorf("subasset maker: bad btc redeem_script hex: %w", err)
	}

	// Partial fills (T8): the taker MAY take less than the whole offer. funded.SeqAmount
	// carries how much asset it wants (0 = whole, for back-compat with older takers). The
	// BTC leg it funds must be the PROPORTIONAL amount at the offer's price; we pay exactly
	// what it asked, and the serve loop re-rests the remainder.
	takeAsset := funded.SeqAmount
	if takeAsset == 0 {
		takeAsset = p.AssetAmount
	}
	if takeAsset > p.AssetAmount {
		sendXcFail(p.Crypter, send, "amount_too_large", "requested more asset than the offer")
		return res, fmt.Errorf("subasset maker: take %d > offer %d", takeAsset, p.AssetAmount)
	}
	wantBtc := ProportionalBtc(p.BtcAmount, takeAsset, p.AssetAmount)

	// Now that the taker's H is known, bind the settlement engine: the BTC-leg
	// swap embeds H so VerifyBTCLeg/ClaimBTCLeg recompute and match it.
	ops := p.NewMakerOps(hashH)

	// The T_btc verified is the one ENCODED IN THE TAKER'S HTLC (funded.Leg.Locktime),
	// not the maker's advertised value: with an externally-funded HTLC the wallet picks
	// T_btc (from the offer's advisory OnchainCltv + the live tip), so the maker accepts
	// the taker's choice within a sanity window (checked in step 4). For the self-funded
	// taker this equals the maker's advertised BtcLocktime, so nothing changes.
	tBtc := funded.Leg.Locktime

	// 3. Verify the on-chain BTC HTLC (H, claim=maker, refund=taker, amount, confs).
	//    The taker announces the instant ITS node sees MinBTCConf; ours can lag by
	//    propagation, so poll: only a proven-INVALID leg is terminal.
	var verified *xchain.VerifiedBTCLeg
	verifyDeadline := time.Now().Add(p.Timing.SeqLockWait)
	for {
		verified, err = ops.VerifyBTCLeg(hashH, claimKey.PubKey(), takerRefundPub, script,
			tBtc, funded.Leg.Txid, funded.Leg.Vout, funded.Leg.Amount, p.MinBTCConf)
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
		p.logf("subasset maker: BTC leg %s not yet verifiable, retrying: %v", funded.Leg.Txid, err)
		time.Sleep(p.Timing.Poll)
	}
	if funded.Leg.Amount != wantBtc {
		sendXcFail(p.Crypter, send, "amount_mismatch", "btc leg amount != proportional quote")
		return res, fmt.Errorf("subasset maker: btc leg %d != required %d (take %d of %d)", funded.Leg.Amount, wantBtc, takeAsset, p.AssetAmount)
	}

	// 4. Claim-window guard: never pay the asset unless T_btc is far enough out to
	//    still claim the BTC after we learn P. On the anchor chain (Bitcoin) confs
	//    are final, so a comfortable block margin is the whole safety condition.
	tip, err := ops.BtcTip()
	if err != nil {
		sendXcFail(p.Crypter, send, "btc_tip", err.Error())
		return res, fmt.Errorf("subasset maker: btc tip: %w", err)
	}
	if tBtc <= uint32(tip) || tBtc-uint32(tip) < p.MinClaimWindow {
		sendXcFail(p.Crypter, send, "claim_window", "T_btc too close to tip to safely claim")
		return res, fmt.Errorf("subasset maker: T_btc %d within %d of tip %d; not paying", tBtc, p.MinClaimWindow, tip)
	}
	// Upper bound: refuse an absurdly far T_btc (the maker's BTC would be locked too
	// long against a Bitcoin reorg / griefing). 0 = no upper bound.
	if p.MaxClaimWindow > 0 && tBtc-uint32(tip) > p.MaxClaimWindow {
		sendXcFail(p.Crypter, send, "claim_window", "T_btc too far in the future")
		return res, fmt.Errorf("subasset maker: T_btc %d exceeds tip %d + max %d; not paying", tBtc, tip, p.MaxClaimWindow)
	}
	p.logf("subasset maker: BTC HTLC %s verified (%d sats, T_btc=%d, tip=%d); paying asset over LN", funded.Leg.Txid, p.BtcAmount, tBtc, tip)

	if err := sendXc(&XcMsg{Type: XcSubAsBtcVerified, HashH: funded.HashH}, p.Crypter, send); err != nil {
		return res, err
	}

	// 5. Pay the taker's asset hold invoice. This BLOCKS until the taker settles it
	//    with P; on settle we learn P. The payment is a HELD LN HTLC until then, so
	//    a taker that never settles simply times it out (nothing delivered).
	// The asset payment's timelock is capped against T_btc: a taker whose asset
	// hold outlasts its own BTC refund could refund the BTC and settle afterwards.
	maxDelay := subAssetPayCap(tBtc - uint32(tip))
	if maxDelay == 0 {
		sendXcFail(p.Crypter, send, "claim_window", "T_btc leaves no room for the asset payment's timelock")
		return res, fmt.Errorf("subasset maker: T_btc %d vs tip %d leaves no timelock room", tBtc, tip)
	}
	var preimage []byte
	if hodl {
		if maxDelay < 18 {
			sendXcFail(p.Crypter, send, "claim_window", "T_btc too close for an 18-block hold payment")
			return res, fmt.Errorf("subasset maker: HODL pay needs 18 blocks, T_btc allows %d", maxDelay)
		}
		p.logf("subasset maker: paying %d asset atoms by bare hash to taker node %s (HODL; device settles, no bolt11)", takeAsset, funded.TakerLNNodeID)
		preimage, err = ops.PayAssetHashHold(funded.TakerLNNodeID, hashH, takeAsset*1000)
	} else {
		preimage, err = ops.PayAssetHold(funded.Bolt11, hashH, takeAsset*1000, maxDelay)
	}
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
	if err := ops.InjectSecret(preimage); err != nil {
		return res, fmt.Errorf("subasset maker: inject secret (RETRYABLE, maker holds P): %w", err)
	}
	claimTxid, err := ops.ClaimBTCLeg(verified.Leg, claimKey, xcSafeFee(p.SpendFeeSats, wantBtc))
	if err != nil {
		// We hold P; this is retryable out of band (the leg is confirmed and ours
		// to claim until T_btc). Surface it, but the value is recoverable.
		return res, fmt.Errorf("subasset maker: claim BTC HTLC after paying asset (RETRYABLE, maker holds P): %w", err)
	}
	res.BtcClaimTxid = claimTxid
	res.Settled = true
	res.FilledAsset = takeAsset
	res.FilledBtc = wantBtc
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
	Preimage     []byte      // 32-byte secret P (minted if nil). IGNORED in external-invoice mode.
	// DEVICE-PREIMAGE (external-invoice) mode: when ExternalBolt11 is set, the asset
	// invoice was created OUT-OF-BAND by the device (the wallet) on the receiving
	// node with the device's OWN preimage. The driver then NEVER mints P, creates an
	// invoice, or settles: it funds the BTC HTLC on the device-supplied H, announces
	// the device's bolt11 to the maker, and waits for the invoice to be PAID (the
	// device's node auto-settles, revealing P to the maker, who claims the BTC). The
	// preimage stays device-held; this taker never learns it. Non-custodial receive.
	ExternalHashH  []byte // the device invoice's payment_hash H (H = SHA256(device P))
	ExternalBolt11 string // the device-created asset invoice on H (the maker pays this)
	// DEVICE-HODL mode: the device registered a HOLD on ExternalHashH at its OWN hosted
	// node and holds P; this taker relays H + this hosted node id (NOT a bolt11), the
	// maker PayHash-es the bare hash, and the DEVICE settles out-of-band. This taker
	// waits for HELD and never learns P.
	TakerLNNodeID string
	// EXTERNAL BTC HTLC mode: the wallet/device funded + signed + broadcast the BTC HTLC
	// from the USER's OWN BTC (device-signed), so this taker does NOT fund it (the LSP
	// never fronts the BTC). It only relays the pre-funded leg to the maker. The user
	// holds the refund key (device); this taker only carries the refund PUBKEY for the
	// announcement and never refunds — the wallet reclaims the HTLC at CLTV.
	ExternalBtcLeg       *xchain.LegLock // the user's pre-funded HTLC (Script + Funded{TxID,Vout,Amount} + Locktime)
	ExternalBtcRefundPub []byte          // the user's BTC refund pubkey (encoded in the HTLC script)
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
	Held        bool // DEVICE-HODL: the maker's payment is HELD; the DEVICE settles.
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
	// MinBTCConf 0 is honored explicitly (0-conf: the maker fronts the Bitcoin
	// reorg risk, like the submarine's max-0conf LP-fronting); only a negative
	// value falls back to the safe 1-conf default.
	if p.MinBTCConf < 0 {
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
	// DEVICE-PREIMAGE (external-invoice) mode: the device created the asset invoice
	// with its own preimage; this taker only gets H + the bolt11 and NEVER the
	// preimage. Otherwise (self-driven mode) it mints P and creates the invoice.
	// DEVICE-HODL mode: the device registered a hold on H at its OWN hosted node and
	// holds P; this taker relays H + that node id (no bolt11) and waits for HELD.
	hodl := p.TakerLNNodeID != ""
	external := p.ExternalBolt11 != "" || hodl
	if external && len(p.ExternalHashH) != 32 {
		return nil, fmt.Errorf("subasset taker: external-invoice mode needs a 32-byte ExternalHashH")
	}
	var secret []byte
	if !external {
		secret = p.Preimage
		if len(secret) == 0 {
			secret = make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return nil, err
			}
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
	// Partial fills (T8): the maker advertises the WHOLE offer in terms; we may take a
	// smaller p.AssetAmount. Never over-ask, and our BTC leg must be EXACTLY the proportional
	// price of what we take (the maker requires the same value, rounded up, so a mismatch is
	// rejected before anything is funded). A whole-offer lift reduces to the old exact match.
	if terms.SeqAmount != 0 {
		if p.AssetAmount > terms.SeqAmount {
			return res, fmt.Errorf("%w: asked %d asset but the offer is only %d", ErrXcBadTerms, p.AssetAmount, terms.SeqAmount)
		}
		wantBtc := ProportionalBtc(terms.BtcAmount, p.AssetAmount, terms.SeqAmount)
		if terms.BtcAmount != 0 && p.BtcAmount != wantBtc {
			return res, fmt.Errorf("%w: BTC %d != proportional %d for taking %d of %d", ErrXcBadTerms, p.BtcAmount, wantBtc, p.AssetAmount, terms.SeqAmount)
		}
	} else if terms.BtcAmount != 0 && terms.BtcAmount != p.BtcAmount {
		return res, fmt.Errorf("%w: maker BTC amount %d != expected %d", ErrXcBadTerms, terms.BtcAmount, p.BtcAmount)
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

	// 2. Fund the BTC HTLC (claim=maker with P, refund=us after T_btc). H is the
	//    device's invoice hash in external mode, else SHA256(our minted P).
	var hashH []byte
	if external {
		hashH = p.ExternalHashH
	} else {
		hashArr := sha256.Sum256(secret)
		hashH = hashArr[:]
	}
	res.HashH = hashH
	// EXTERNAL BTC: the wallet already funded + broadcast the HTLC from the USER's own
	// BTC; we relay it (the LSP fronts nothing). Else we fund it ourselves.
	externalBtc := p.ExternalBtcLeg != nil
	var btcLeg *xchain.LegLock
	var announceRefundPub []byte
	if externalBtc {
		btcLeg = p.ExternalBtcLeg
		announceRefundPub = p.ExternalBtcRefundPub
		res.BtcLeg = btcLeg
		res.BtcLocktime = btcLeg.Locktime
		p.logf("subasset taker: relaying user-funded BTC HTLC %s (T_btc=%d); the LSP funds nothing", btcLeg.Funded.TxID, btcLeg.Locktime)
		// The maker verifies confirmations on-chain; this taker does not hold the tx in
		// its own wallet, so it does not poll BtcConfirmations.
	} else {
		p.logf("subasset taker: locking BTC HTLC: %d sats, T_btc=%d", p.BtcAmount, terms.BtcLocktime)
		var hp int64
		btcLeg, hp, err = p.Ops.LockBTCLeg(makerClaimPub, refundKey.PubKey(), atomsToCoins(p.BtcAmount), terms.BtcLocktime)
		if err != nil {
			sendXcFail(p.Crypter, send, "btc_lock_failed", err.Error())
			return res, fmt.Errorf("subasset taker: lock BTC leg: %w", err)
		}
		announceRefundPub = refundKey.PubKey()
		res.BtcLeg = btcLeg
		if p.OnBtcLegFunded != nil {
			p.OnBtcLegFunded(res)
		}
		// 3. Wait out our own confirmation on a live parent (broadcast-only -> hp==0).
		p.logf("subasset taker: BTC HTLC broadcast (hp=%d), waiting for %d conf(s)", hp, p.MinBTCConf)
		if hp <= 0 {
			confDeadline := time.Now().Add(p.Timing.BtcConfWait)
			for {
				confs, cerr := p.Ops.BtcConfirmations(btcLeg.Funded.TxID)
				if cerr == nil && confs >= p.MinBTCConf {
					break
				}
				if cerr != nil {
					p.logf("subasset taker: conf poll error: %v", cerr)
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
	}

	// 4. The asset invoice on H. External mode: the DEVICE already created it (we
	//    only forward its bolt11; we never mint P). Self-driven: we create it.
	var invoice string
	switch {
	case hodl:
		// the device created the hold on H at its hosted node; relay only H + node id
	case external:
		invoice = p.ExternalBolt11
	default:
		var invH []byte
		invoice, invH, err = p.Ops.PrepareAssetHold(secret, p.AssetAmount*1000)
		if err != nil {
			sendXcFail(p.Crypter, send, "asset_invoice", err.Error())
			return res, fmt.Errorf("subasset taker: prepare asset invoice (refund BTC at T_btc): %w", err)
		}
		if hex.EncodeToString(invH) != hex.EncodeToString(hashH) {
			return res, fmt.Errorf("subasset taker: invoice hash != H (internal)")
		}
	}

	// 5. Announce the funded leg + invoice. The refund pubkey + T_btc are the ones
	//    actually embedded in the HTLC (the user's, in external-BTC mode).
	msg := &XcMsg{
		Type:              XcSubAsBtcFunded,
		HashH:             hex.EncodeToString(hashH),
		TakerBtcRefundPub: hex.EncodeToString(announceRefundPub),
		SeqAmount:         p.AssetAmount, // T8: how much of the offer we're taking (maker pays exactly this)
		BtcAmount:         p.BtcAmount,   // the proportional BTC we locked (== funded leg amount)
		Leg: &XcLeg{
			Txid:         btcLeg.Funded.TxID,
			Vout:         btcLeg.Funded.Vout,
			Amount:       btcLeg.Funded.Amount,
			RedeemScript: hex.EncodeToString(btcLeg.Script),
			Locktime:     btcLeg.Locktime,
		},
	}
	if hodl {
		// DEVICE-HODL: relay our HOSTED node id, no bolt11 (the maker pays the bare hash).
		msg.TakerLNNodeID = p.TakerLNNodeID
	} else {
		msg.Bolt11 = invoice
	}
	if err := sendXc(msg, p.Crypter, send); err != nil {
		return res, err
	}

	// 6. Await the maker's "verified, about to pay" ack (best-effort; a failing
	//    maker instead couriers XcFail, which recvXcType surfaces).
	if _, err := recvXcType(recv, p.Crypter, XcSubAsBtcVerified, p.Timing.SeqLockWait); err != nil {
		if !external {
			_ = p.Ops.CancelAssetHold(hashH)
		}
		return res, fmt.Errorf("subasset taker: maker did not verify the BTC leg (refund BTC at T_btc): %w", err)
	}

	if hodl {
		// 7-hodl (DEVICE-HODL): the maker pays the bare hash to our HOSTED node, where
		// the device registered a hold on H and holds P. We wait for the payment to be
		// HELD, then STOP — the DEVICE settles out-of-band (revealing P to the maker,
		// who claims the BTC). We never learn P and never settle.
		p.logf("subasset taker: awaiting the maker's HELD asset payment on H (device settles out-of-band)")
		if err := p.Ops.WaitAssetHeld(hashH, p.Timing.SeqLockWait); err != nil {
			return res, fmt.Errorf("subasset taker: maker never held the asset (refund BTC at T_btc): %w", err)
		}
		res.Held = true
		p.logf("subasset taker: asset payment HELD; the DEVICE settles now (maker claims BTC with the device-revealed P)")
		return res, nil
	}

	if external {
		// 7-external (DEVICE-PREIMAGE): the device's node holds P and auto-settles
		// when the maker pays. We NEVER learn P; we only wait for the invoice to be
		// PAID (= the maker paid, the device revealed P to the maker, the maker
		// claims the BTC). The received asset lands in the device's own node.
		p.logf("subasset taker: awaiting the maker's asset payment (device settles; preimage device-held)")
		if err := p.Ops.WaitAssetPaid(hashH, p.Timing.SeqLockWait); err != nil {
			return res, fmt.Errorf("subasset taker: maker never paid the asset (refund BTC at T_btc): %w", err)
		}
		res.Received = true // res.Preimage stays nil: the device holds it, not us
		p.logf("subasset taker: asset invoice PAID (device received the asset; maker claims BTC with the device-revealed P)")
		return res, nil
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

// --- mixed same-chain ops (rails 7/8): the on-chain leg is an ISSUED asset --
//
// The sub-asset construction with the QUOTE asset standing in BTC's structural
// place (Principle 3: no privileged unit): the base asset moves over Lightning
// exactly as in the sub-asset swap, and the "BTC leg" is an on-chain HTLC on
// the quote ASSET, on the SAME Sequentia chain, verified/claimed/refunded in
// Elements format (xchain.NewSwapAsset). Everything else — the courier
// protocol, the hold-invoice discipline, the drivers (RunMakerSubAsset /
// RunTakerSubAsset) — is IDENTICAL, so these ops are the live ops with the
// chain-facing methods re-pointed at the Sequentia chain + quote asset.

// LiveSubAssetMakerOpsSeq is LiveSubAssetMakerOps for the mixed same-chain
// shape. "BtcTip"/locktimes are SEQUENTIA heights; VerifyBTCLeg requires the
// funded leg to pay the quote asset.
type LiveSubAssetMakerOpsSeq struct {
	LiveSubAssetMakerOps
	Seq        *xchain.Chain
	QuoteAsset string
}

// NewLiveSubAssetMakerOpsSeq builds the maker's mixed same-chain ops for a swap
// whose taker H is hashH: the quote-asset HTLC swap embeds hashH so
// VerifyBTCLeg/ClaimBTCLeg recompute and match it.
func NewLiveSubAssetMakerOpsSeq(seq *xchain.Chain, quoteAsset string, assetLN xchain.LNLeg, hashH []byte) *LiveSubAssetMakerOpsSeq {
	return &LiveSubAssetMakerOpsSeq{
		LiveSubAssetMakerOps: LiveSubAssetMakerOps{
			Swap:    xchain.NewSwapAsset(seq, quoteAsset, xchain.NewHashLockFromHash(hashH)),
			AssetLN: assetLN,
		},
		Seq:        seq,
		QuoteAsset: quoteAsset,
	}
}

func (o *LiveSubAssetMakerOpsSeq) BtcTip() (int64, error) { return o.Seq.BlockCount() }
func (o *LiveSubAssetMakerOpsSeq) VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	return o.Swap.VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript, btcLocktime, txid, vout, amount, o.QuoteAsset, minConf)
}

// LiveSubAssetTakerOpsSeq is LiveSubAssetTakerOps for the mixed same-chain
// shape: the taker funds/refunds the on-chain HTLC on the QUOTE asset via the
// Sequentia chain (LockBTCLeg routes through the asset-aware Swap; amountCoins
// is denominated in the quote asset's coins).
type LiveSubAssetTakerOpsSeq struct {
	LiveSubAssetTakerOps
	Seq *xchain.Chain
}

// NewLiveSubAssetTakerOpsSeq builds the taker's mixed same-chain ops. lock is
// the HTLC hashlock (NewHashLock(secret) or NewHashLockFromHash for an
// external/device H), matching the CLI's construction of the live ops.
func NewLiveSubAssetTakerOpsSeq(seq *xchain.Chain, quoteAsset string, assetLN xchain.LNLeg, lock *xchain.HashLock, plain bool) *LiveSubAssetTakerOpsSeq {
	return &LiveSubAssetTakerOpsSeq{
		LiveSubAssetTakerOps: LiveSubAssetTakerOps{
			Swap:    xchain.NewSwapAsset(seq, quoteAsset, lock),
			AssetLN: assetLN,
			Plain:   plain,
		},
		Seq: seq,
	}
}

func (o *LiveSubAssetTakerOpsSeq) BtcTip() (int64, error) { return o.Seq.BlockCount() }
func (o *LiveSubAssetTakerOpsSeq) BtcConfirmations(txid string) (int, error) {
	return o.Seq.TxConfirmations(txid)
}
