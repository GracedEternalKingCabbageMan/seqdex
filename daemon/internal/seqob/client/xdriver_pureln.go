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
// drivers: the maker holds its incoming leg and pays its outgoing leg (learning
// P); the taker generates P, invoices its incoming leg, and pays the maker's hold
// by bare hash. v1 = the BUY direction (taker buys the asset with BTC; maker holds
// BTC, pays the asset — ln_direction LnBTCLNForAssetLN).

// PlnMakerOps is the narrow settlement seam the maker driver runs against.
type PlnMakerOps interface {
	// BtcNodeID returns the maker's incoming-leg node id (advertised so the taker
	// can pay the hold by bare hash).
	BtcNodeID() (string, error)
	// RegisterHold registers the incoming BTC hold on h (btcAmtMsat).
	RegisterHold(h []byte, btcAmtMsat uint64) error
	// Fulfill waits for the held BTC, pays the taker's asset invoice (learning P),
	// and settles the hold. Cancels the hold on failure. Returns the preimage.
	Fulfill(h []byte, assetInvoice string, assetAmtMsat uint64, holdTimeout time.Duration) ([]byte, error)
}

// PlnTakerOps is the settlement seam the taker driver runs against.
type PlnTakerOps interface {
	// PrepareBuy issues the asset invoice on the taker's preimage p; returns the
	// invoice and h = SHA256(p).
	PrepareBuy(p []byte, assetAmtMsat uint64) (assetInvoice string, h []byte, err error)
	// PayHold pays the maker's BTC hold by bare hash and blocks until the maker
	// settles, returning the revealed preimage.
	PayHold(h []byte, makerBtcNodeID string, btcAmtMsat uint64, finalCltv uint32, paymentSecret []byte) ([]byte, error)
}

// Live implementations over a real *xchain.PureLNSwap (M3).
type LivePlnMakerOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnMakerOps) BtcNodeID() (string, error) { return o.Swap.BtcLegNodeID() }
func (o *LivePlnMakerOps) RegisterHold(h []byte, amt uint64) error {
	return o.Swap.MakerRegisterHold(h, amt)
}
func (o *LivePlnMakerOps) Fulfill(h []byte, inv string, amt uint64, to time.Duration) ([]byte, error) {
	return o.Swap.MakerFulfill(h, inv, amt, to)
}

type LivePlnTakerOps struct{ Swap *xchain.PureLNSwap }

func (o *LivePlnTakerOps) PrepareBuy(p []byte, amt uint64) (string, []byte, error) {
	return o.Swap.PrepareTakerBuy(p, amt)
}
func (o *LivePlnTakerOps) PayHold(h []byte, id string, amt uint64, cltv uint32, secret []byte) ([]byte, error) {
	return o.Swap.RunTakerBuy(h, id, amt, cltv, secret)
}

// --- BUY maker --------------------------------------------------------------

// MakerPlnParams configures RunMakerPureLNBuy. Amounts come from the SIGNED offer.
type MakerPlnParams struct {
	NewMakerOps  func() PlnMakerOps // binds the settlement engine
	Crypter      *Crypter
	AssetAmtMsat uint64        // asset the maker pays (the offer's asset side)
	BtcAmtMsat   uint64        // BTC the maker receives (the offer's BTC side)
	HoldTimeout  time.Duration // wait for the taker's BTC hold + fulfill (default 2m)
	Timing       XcTiming
	Log          func(format string, args ...interface{})
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

// RunMakerPureLNBuy executes the BUY pure-LN handshake as the maker: advertise
// terms (incl. its BTC-leg node id), receive the taker's asset invoice on H,
// register the incoming BTC hold, and fulfill (wait held -> pay asset -> settle).
func RunMakerPureLNBuy(p MakerPlnParams, in <-chan []byte, send XcSend) (*MakerPlnResult, error) {
	p.Timing.setDefaults()
	if p.NewMakerOps == nil || p.Crypter == nil {
		return nil, fmt.Errorf("pureln maker: NewMakerOps and Crypter are required")
	}
	if p.HoldTimeout <= 0 {
		p.HoldTimeout = 2 * time.Minute
	}
	recv := chanRecv(in)
	res := &MakerPlnResult{}

	if _, err := recvXcType(recv, p.Crypter, XcPlnTermsRequest, p.Timing.TermsReqWait); err != nil {
		return res, err
	}
	ops := p.NewMakerOps()
	btcID, err := ops.BtcNodeID()
	if err != nil {
		sendXcFail(p.Crypter, send, "maker_node", err.Error())
		return res, fmt.Errorf("maker btc node id: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnTerms, MakerLNNodeID: btcID, BtcAmount: p.BtcAmtMsat, SeqAmount: p.AssetAmtMsat}, p.Crypter, send); err != nil {
		return res, err
	}

	inv, err := recvXcType(recv, p.Crypter, XcPlnAssetInvoice, p.Timing.BtcFundWait)
	if err != nil {
		return res, err
	}
	hashH, err := hex.DecodeString(inv.HashH)
	if err != nil || len(hashH) != 32 {
		sendXcFail(p.Crypter, send, "bad_hash", "asset invoice carried a malformed hash")
		return res, fmt.Errorf("pureln maker: bad hash_h %q", inv.HashH)
	}
	res.HashH = hashH
	p.logf("pureln maker: registering BTC hold on H=%s", inv.HashH[:12])
	if err := ops.RegisterHold(hashH, p.BtcAmtMsat); err != nil {
		sendXcFail(p.Crypter, send, "hold", err.Error())
		return res, fmt.Errorf("register hold: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnHoldReady, HashH: inv.HashH}, p.Crypter, send); err != nil {
		return res, err
	}

	// Wait for the taker to lock the BTC, pay the asset invoice (learn P), settle.
	pre, err := ops.Fulfill(hashH, inv.Bolt11, p.AssetAmtMsat, p.HoldTimeout)
	if err != nil {
		sendXcFail(p.Crypter, send, "fulfill", err.Error())
		return res, fmt.Errorf("fulfill: %w", err)
	}
	res.Preimage = pre
	res.Settled = true
	_ = sendXc(&XcMsg{Type: XcPlnSettled}, p.Crypter, send)
	p.logf("pureln maker: settled, took the BTC")
	return res, nil
}

// --- BUY taker --------------------------------------------------------------

type TakerPlnParams struct {
	Ops           PlnTakerOps
	Crypter       *Crypter
	AssetAmtMsat  uint64
	BtcAmtMsat    uint64
	FinalCltv     uint32 // final-hop cltv delta for paying the hold (default 18)
	PaymentSecret []byte // onion TLV secret; random if nil
	Timing        XcTiming
	Log           func(format string, args ...interface{})
}

type TakerPlnResult struct {
	HashH    []byte
	Preimage []byte
}

// RunTakerPureLNBuy executes the BUY pure-LN handshake as the taker: request
// terms, issue an asset invoice on a fresh P, hand the maker H + the invoice,
// then pay the maker's BTC hold by bare hash (blocks until the maker settles).
func RunTakerPureLNBuy(p TakerPlnParams, send XcSend, recv XcRecv) (*TakerPlnResult, error) {
	p.Timing.setDefaults()
	if p.Ops == nil || p.Crypter == nil {
		return nil, fmt.Errorf("pureln taker: Ops and Crypter are required")
	}
	if p.FinalCltv == 0 {
		p.FinalCltv = 18
	}
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
		return nil, fmt.Errorf("pureln taker: maker advertised no BTC node id")
	}
	if terms.BtcAmount != 0 && terms.BtcAmount != p.BtcAmtMsat {
		return nil, fmt.Errorf("%w: maker BTC amount %d != expected %d", ErrXcBadTerms, terms.BtcAmount, p.BtcAmtMsat)
	}
	if terms.SeqAmount != 0 && terms.SeqAmount != p.AssetAmtMsat {
		return nil, fmt.Errorf("%w: maker asset amount %d != expected %d", ErrXcBadTerms, terms.SeqAmount, p.AssetAmtMsat)
	}

	// Our secret P, and the asset invoice bound to it.
	pre := make([]byte, 32)
	if _, err := rand.Read(pre); err != nil {
		return nil, err
	}
	assetInvoice, hashH, err := p.Ops.PrepareBuy(pre, p.AssetAmtMsat)
	if err != nil {
		sendXcFail(p.Crypter, send, "invoice", err.Error())
		return nil, fmt.Errorf("prepare asset invoice: %w", err)
	}
	if err := sendXc(&XcMsg{Type: XcPlnAssetInvoice, HashH: hex.EncodeToString(hashH), Bolt11: assetInvoice}, p.Crypter, send); err != nil {
		return nil, err
	}
	if _, err := recvXcType(recv, p.Crypter, XcPlnHoldReady, p.Timing.SeqLockWait); err != nil {
		return nil, err
	}

	// Pay the maker's BTC hold by bare hash; blocks until the maker settles.
	revealed, err := p.Ops.PayHold(hashH, terms.MakerLNNodeID, p.BtcAmtMsat, p.FinalCltv, secret)
	if err != nil {
		sendXcFail(p.Crypter, send, "pay_hold", err.Error())
		return nil, fmt.Errorf("pay BTC hold: %w", err)
	}
	// The maker settled with our own P; sanity-check the reveal.
	want := sha256.Sum256(pre)
	if hex.EncodeToString(revealed) != hex.EncodeToString(pre) || hex.EncodeToString(hashH) != hex.EncodeToString(want[:]) {
		return nil, fmt.Errorf("%w: settle revealed a preimage that is not ours", ErrLNPeer)
	}
	return &TakerPlnResult{HashH: hashH, Preimage: pre}, nil
}

// ErrLNPeer flags a counterparty protocol violation in a pure-LN swap.
var ErrLNPeer = fmt.Errorf("pure-LN peer protocol violation")
