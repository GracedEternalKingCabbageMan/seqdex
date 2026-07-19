package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// xdriver_pureln.go runs the PURE-LN swap handshake (xcourier_pureln.go) over the
// opaque relay courier, settling with the proven pkg/xchain PureLNSwap engine.
// BOTH legs are off-chain Lightning, stitched by one shared secret — there is NO
// on-chain leg and NO anchor gate, so this is the simplest of the Lightning
// drivers. It is direction-parameterized because BUY and SELL are genuinely
// symmetric: the TAKER always originates the secret P, mints an invoice on its
// INCOMING leg (the leg it receives value on), and pays into the maker's hold on
// its OUTGOING leg; the MAKER always holds its incoming leg, then pays the taker's
// invoice on its outgoing leg (learning P) and settles the hold.
//
//	BUY  (LnBTCLNForAssetLN): taker receives asset, pays BTC -> maker holds BTC, pays asset
//	SELL (LnAssetLNForBTCLN): taker receives BTC, pays asset -> maker holds asset, pays BTC

// PlnDirection selects which leg the maker holds. The neutral driver derives the
// hold/pay amounts and picks the live ops from this.
type PlnDirection int

const (
	PlnBuy  PlnDirection = iota // taker buys the asset with BTC; maker holds BTC, pays the asset
	PlnSell                     // taker sells the asset for BTC; maker holds the asset, pays BTC
)

// PlnMakerOps is the narrow settlement seam the maker driver runs against. The
// method shapes are direction-neutral; the live ops differ per direction.
type PlnMakerOps interface {
	// HoldNodeID returns the maker's incoming (held) leg node id — advertised so
	// the taker can pay the hold by bare hash.
	HoldNodeID() (string, error)
	// RegisterHold registers the incoming hold on h for holdAmtMsat.
	RegisterHold(h []byte, holdAmtMsat uint64) error
	// Fulfill waits for the held incoming leg, pays the taker's invoice for
	// payAmtMsat (learning P), and settles the hold. Cancels on failure. Returns P.
	Fulfill(h []byte, takerInvoice string, payAmtMsat uint64, holdTimeout time.Duration) ([]byte, error)
}

// PlnTakerOps is the settlement seam the taker driver runs against.
type PlnTakerOps interface {
	// PrepareInvoice issues the invoice on the taker's INCOMING leg on preimage p
	// for invoiceAmtMsat; returns the invoice and h = SHA256(p).
	PrepareInvoice(p []byte, invoiceAmtMsat uint64) (invoice string, h []byte, err error)
	// PayHold pays the maker's hold by bare hash for holdAmtMsat and blocks until
	// the maker settles, returning the revealed preimage.
	PayHold(h []byte, makerHoldNodeID string, holdAmtMsat uint64, finalCltv uint32, paymentSecret []byte) ([]byte, error)
}

// --- live ops over a real *xchain.PureLNSwap (M3) ---------------------------

// BUY: maker holds the BTC leg and pays the asset leg.
type LivePlnMakerBuyOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnMakerBuyOps) HoldNodeID() (string, error) { return o.Swap.BtcLegNodeID() }
func (o *LivePlnMakerBuyOps) RegisterHold(h []byte, amt uint64) error {
	return o.Swap.MakerRegisterHold(h, amt)
}
func (o *LivePlnMakerBuyOps) Fulfill(h []byte, inv string, amt uint64, to time.Duration) ([]byte, error) {
	return o.Swap.MakerFulfill(h, inv, amt, to)
}

type LivePlnTakerBuyOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnTakerBuyOps) PrepareInvoice(p []byte, amt uint64) (string, []byte, error) {
	return o.Swap.PrepareTakerBuy(p, amt)
}
func (o *LivePlnTakerBuyOps) PayHold(h []byte, id string, amt uint64, cltv uint32, secret []byte) ([]byte, error) {
	return o.Swap.RunTakerBuy(h, id, amt, cltv, secret)
}

// SELL: maker holds the asset leg and pays the BTC leg.
type LivePlnMakerSellOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnMakerSellOps) HoldNodeID() (string, error) { return o.Swap.AssetLegNodeID() }
func (o *LivePlnMakerSellOps) RegisterHold(h []byte, amt uint64) error {
	return o.Swap.MakerRegisterHoldSell(h, amt)
}
func (o *LivePlnMakerSellOps) Fulfill(h []byte, inv string, amt uint64, to time.Duration) ([]byte, error) {
	return o.Swap.MakerFulfillSell(h, inv, amt, to)
}

type LivePlnTakerSellOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnTakerSellOps) PrepareInvoice(p []byte, amt uint64) (string, []byte, error) {
	return o.Swap.PrepareTakerSell(p, amt)
}
func (o *LivePlnTakerSellOps) PayHold(h []byte, id string, amt uint64, cltv uint32, secret []byte) ([]byte, error) {
	return o.Swap.RunTakerSell(h, id, amt, cltv, secret)
}

// holdPayAmts splits the offer's BTC/asset amounts into (holdAmt, payAmt) for a
// direction: the maker holds what the taker pays in, and pays the taker's invoice.
func holdPayAmts(dir PlnDirection, btcMsat, seqMsat uint64) (holdAmt, payAmt uint64) {
	if dir == PlnSell {
		return seqMsat, btcMsat // maker holds the asset, pays BTC
	}
	return btcMsat, seqMsat // BUY: maker holds BTC, pays the asset
}

// --- maker -----------------------------------------------------------------

// MakerPlnParams configures RunMakerPureLN. Amounts come from the SIGNED offer.
type MakerPlnParams struct {
	Direction   PlnDirection
	NewMakerOps func() PlnMakerOps // binds the settlement engine for this direction
	Crypter     *Crypter
	BtcAmtMsat  uint64        // the offer's BTC side (advertised in Terms + bound)
	SeqAmtMsat  uint64        // the offer's asset side (advertised in Terms + bound)
	HoldTimeout time.Duration // wait for the taker's hold + fulfill (default 2m)
	Timing      XcTiming
	Log         func(format string, args ...interface{})
}

type MakerPlnResult struct {
	HashH    []byte
	Preimage []byte
	Settled  bool
}

func (p *MakerPlnParams) logf(f string, a ...interface{}) {
	if p.Log != nil {
		p.Log(f, a...)
	}
}

// RunMakerPureLN executes the pure-LN handshake as the maker: advertise terms
// (incl. its held-leg node id), receive the taker's invoice on H, register the
// incoming hold, and fulfill (wait held -> pay the taker's invoice -> settle).
func RunMakerPureLN(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Timing.setDefaults()
	if p.NewMakerOps == nil || p.Crypter == nil {
		return nil, fmt.Errorf("pureln maker: NewMakerOps and Crypter are required")
	}
	if p.HoldTimeout <= 0 {
		p.HoldTimeout = 2 * time.Minute
	}
	holdAmt, payAmt := holdPayAmts(p.Direction, p.BtcAmtMsat, p.SeqAmtMsat)
	recv := chanRecv(in)
	res := &MakerPlnResult{}

	if _, err := recvXcType(recv, p.Crypter, XcPlnTermsRequest, p.Timing.TermsReqWait); err != nil {
		return res, err
	}
	ops := p.NewMakerOps()
	holdID, err := ops.HoldNodeID()
	if err != nil {
		sendXcFail(p.Crypter, send, "maker_node", err.Error())
		return res, fmt.Errorf("maker hold node id: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnTerms, MakerLNNodeID: holdID, BtcAmount: p.BtcAmtMsat, SeqAmount: p.SeqAmtMsat}, p.Crypter, send); err != nil {
		return res, err
	}

	inv, err := recvXcType(recv, p.Crypter, XcPlnAssetInvoice, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	hashH, err := hex.DecodeString(inv.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "bad_hash", "invoice carried a malformed hash")
		return res, fmt.Errorf("pureln maker: bad hash_h %q", inv.HashH)
	}
	res.HashH = hashH
	p.logf("pureln maker: registering hold on H=%s", inv.HashH[:12])
	if err := ops.RegisterHold(hashH, holdAmt); err != nil {
		sendXcFail(p.Crypter, send, "hold", err.Error())
		return res, fmt.Errorf("register hold: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnHoldReady, HashH: inv.HashH}, p.Crypter, send); err != nil {
		return res, err
	}

	// Wait for the taker to lock the incoming leg, pay its invoice (learn P), settle.
	pre, err := ops.Fulfill(hashH, inv.Bolt11, payAmt, p.HoldTimeout)
	if err != nil {
		sendXcFail(p.Crypter, send, "fulfill", err.Error())
		return res, fmt.Errorf("fulfill: %w", err)
	}
	res.Preimage = pre
	res.Settled = true
	_ = sendXc(&XcMsg{Type: XcPlnSettled}, p.Crypter, send)
	p.logf("pureln maker: settled, took the incoming leg")
	return res, nil
}

// RunMakerPureLNBuy / RunMakerPureLNSell are the direction-explicit entry points.
func RunMakerPureLNBuy(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Direction = PlnBuy
	return RunMakerPureLN(p, in, send)
}
func RunMakerPureLNSell(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Direction = PlnSell
	return RunMakerPureLN(p, in, send)
}

// --- taker -----------------------------------------------------------------

type TakerPlnParams struct {
	Direction     PlnDirection
	Ops           PlnTakerOps
	Crypter       *Crypter
	BtcAmtMsat    uint64
	SeqAmtMsat    uint64
	FinalCltv     uint32 // final-hop cltv delta for paying the hold (default 18)
	PaymentSecret []byte // onion TLV secret; random if nil
	Timing        XcTiming
	Log           func(format string, args ...interface{})
}

type TakerPlnResult struct {
	HashH    []byte
	Preimage []byte
}

// RunTakerPureLN executes the pure-LN handshake as the taker: request terms, issue
// an invoice on a fresh P for its incoming leg, hand the maker H + the invoice,
// then pay the maker's hold by bare hash (blocks until the maker settles).
func RunTakerPureLN(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil {
		return nil, fmt.Errorf("pureln taker: Ops and Crypter are required")
	}
	if p.FinalCltv == 0 {
		p.FinalCltv = 18
	}
	// The taker mints an invoice for its INCOMING leg and pays HOLD into the
	// maker on its OUTGOING leg. For BUY it receives the asset (invoice=asset) and
	// pays BTC (hold=BTC); for SELL it is the mirror.
	holdAmt, invoiceAmt := holdPayAmts(p.Direction, p.BtcAmtMsat, p.SeqAmtMsat)
	secret := p.PaymentSecret
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}

	if err := sendXc(&XcMsg{Type: XcPlnTermsRequest}, p.Crypter, send); err != nil {
		return nil, err
	}
	terms, err := recvXcType(recv, p.Crypter, XcPlnTerms, p.Timing.TermsWait)
	if err != nil {
		return nil, err
	}
	if terms.MakerLNNodeID == "" {
		return nil, fmt.Errorf("pureln taker: maker advertised no hold-leg node id")
	}
	if terms.BtcAmount != 0 && terms.BtcAmount != p.BtcAmtMsat {
		return nil, fmt.Errorf("%w: maker BTC amount %d != expected %d", ErrXcBadTerms, terms.BtcAmount, p.BtcAmtMsat)
	}
	if terms.SeqAmount != 0 && terms.SeqAmount != p.SeqAmtMsat {
		return nil, fmt.Errorf("%w: maker asset amount %d != expected %d", ErrXcBadTerms, terms.SeqAmount, p.SeqAmtMsat)
	}

	// Our secret P, and the invoice on our incoming leg bound to it.
	pre := make([]byte, 32)
	if _, err := rand.Read(pre); err != nil {
		return nil, err
	}
	invoice, hashH, err := p.Ops.PrepareInvoice(pre, invoiceAmt)
	if err != nil {
		sendXcFail(p.Crypter, send, "invoice", err.Error())
		return nil, fmt.Errorf("prepare invoice: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnAssetInvoice, HashH: hex.EncodeToString(hashH), Bolt11: invoice}, p.Crypter, send); err != nil {
		return nil, err
	}
	if _, err := recvXcType(recv, p.Crypter, XcPlnHoldReady, p.Timing.SeqLockWait); err != nil {
		return nil, err
	}

	// Pay the maker's hold by bare hash; blocks until the maker settles.
	// Emit H at the moment we commit funds so a caller that is killed mid-pay (e.g. an LSP whose
	// exec timeout fires during PayHold) can reconcile: it greps this H and asks the paying node
	// `listsendpays H` before telling the user nothing was spent, instead of prompting a double-pay.
	if p.Log != nil {
		p.Log("pureln taker: paying maker hold on H=%s (funds now committed)", hex.EncodeToString(hashH))
	}
	revealed, err := p.Ops.PayHold(hashH, terms.MakerLNNodeID, holdAmt, p.FinalCltv, secret)
	if err != nil {
		sendXcFail(p.Crypter, send, "pay_hold", err.Error())
		return nil, fmt.Errorf("pay hold: %w", err)
	}
	// The maker settled with our own P; sanity-check the reveal.
	want := sha256.Sum256(pre)
	if hex.EncodeToString(revealed) != hex.EncodeToString(pre) || hex.EncodeToString(hashH) != hex.EncodeToString(want[:]) {
		return nil, fmt.Errorf("%w: settle revealed a preimage that is not ours", ErrLNPeer)
	}
	return &TakerPlnResult{HashH: hashH, Preimage: pre}, nil
}

// RunTakerPureLNBuy / RunTakerPureLNSell are the direction-explicit entry points.
func RunTakerPureLNBuy(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	p.Direction = PlnBuy
	return RunTakerPureLN(p, send, recv)
}
func RunTakerPureLNSell(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	p.Direction = PlnSell
	return RunTakerPureLN(p, send, recv)
}

// ErrLNPeer flags a counterparty protocol violation in a pure-LN swap.
var ErrLNPeer = fmt.Errorf("pure-LN peer protocol violation")
