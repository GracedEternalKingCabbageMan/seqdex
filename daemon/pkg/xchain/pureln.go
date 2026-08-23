package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PureLNSwap is a pure-Lightning atomic swap between two Lightning legs — a
// Sequentia issued-asset leg (e.g. GOLD) and a "BTC" leg (the policy asset, or a
// real Bitcoin-LN node) — stitched by ONE shared secret. Unlike a submarine
// swap it has NO on-chain leg and therefore NO anchor-depth gate: both legs are
// off-chain LN payments, so the happy path settles in seconds.
//
// Both directions are the same mechanism with the legs swapped. In each, the
// maker's INCOMING leg is a hold released only by the preimage P, and the maker
// learns P solely by paying its OUTGOING leg (the taker generated P and put its
// hash on both its own invoice and the maker's hold). So the maker cannot take
// the incoming without delivering the outgoing, and the taker cannot take the
// outgoing without releasing the incoming. Worst case is a hold timeout -> the
// maker cancels and the taker is refunded; nothing partial.
//
//   - BUY  (taker buys the asset with BTC): maker holds BTC, pays the asset.
//   - SELL (taker sells the asset for BTC): maker holds the asset, pays BTC.
type PureLNSwap struct {
	assetLeg LNLeg // pays/receives the Sequentia issued asset (e.g. GOLD)
	btcLeg   LNLeg // pays/receives "BTC" (policy asset / real BTC-LN)
}

// NewPureLNSwap builds a swap over an asset leg and a BTC leg. For a maker both
// legs are the maker's two nodes; for a taker, the taker's two nodes.
func NewPureLNSwap(assetLeg, btcLeg LNLeg) *PureLNSwap {
	return &PureLNSwap{assetLeg: assetLeg, btcLeg: btcLeg}
}

// BtcLegNodeID / AssetLegNodeID expose each leg's Lightning node id (the party
// paying a hold by bare hash needs the holder's node id). Used by the order-book
// driver, which lives in another package.
func (s *PureLNSwap) BtcLegNodeID() (string, error)   { return s.btcLeg.NodeID() }
func (s *PureLNSwap) AssetLegNodeID() (string, error) { return s.assetLeg.NodeID() }

// --- direction-agnostic core (incoming = the maker's held leg) --------------

// takerPrepare issues the invoice on the taker's INCOMING leg on preimage p (the
// maker pays it, learning p). Returns the invoice + h = SHA256(p).
func takerPrepare(incoming LNLeg, p []byte, inAmtMsat uint64) (invoice string, h []byte, err error) {
	sum := sha256.Sum256(p)
	label := "pureln-" + hex.EncodeToString(sum[:6])
	// A just-booted node answers RPC before its channels finish re-establishing,
	// and an invoice with route hints fails until they do (CLN error 902, "None
	// of those hints were suitable local channels" — seen live 91 seconds after
	// a boot, killing the swap before any HTLC existed). The race is transient
	// by construction, so wait it out instead of aborting.
	var inv string
	var err2 error
	for deadline := time.Now().Add(2 * time.Minute); ; {
		inv, err2 = incoming.CreateInvoice(p, inAmtMsat, 0, label, "pure-ln swap: taker incoming")
		if err2 == nil {
			break
		}
		msg := err2.Error()
		if !(strings.Contains(msg, "hints were suitable") || strings.Contains(msg, "902")) || time.Now().After(deadline) {
			return "", nil, fmt.Errorf("taker create invoice: %w", err2)
		}
		time.Sleep(time.Second)
	}
	return inv, sum[:], nil
}

// makerRegisterHold registers the maker's INCOMING hold on h (the maker does not
// know P yet). Call BEFORE telling the taker to pay, so the taker's HTLC is held.
func makerRegisterHold(incoming LNLeg, h []byte, inAmtMsat uint64) error {
	label := "pureln-" + hex.EncodeToString(h[:6])
	if _, err := incoming.CreateHoldInvoice(h, inAmtMsat, 0, label, "pure-ln swap: maker hold"); err != nil {
		return fmt.Errorf("register hold: %w", err)
	}
	return nil
}

// makerFulfill: wait for the incoming leg to be held, pay the taker's outgoing
// invoice (LEARNING P and delivering value), then settle the held incoming with
// P. On any failure before the settle it cancels the hold (the taker's incoming
// is refunded and neither leg completes). Returns the preimage.
func makerFulfill(incoming, outgoing LNLeg, h []byte, outInvoice string, outAmtMsat uint64, holdTimeout time.Duration) (preimage []byte, err error) {
	// Maker-side stage stopwatch (stdout): splits the taker's opaque PayHold
	// wait into held / paid / settled so the latency is attributable.
	start := time.Now()
	mstage := func(name string) { fmt.Printf("MSTAGE %s +%dms\n", name, time.Since(start).Milliseconds()) }
	if _, err = incoming.WaitHeld(h, holdTimeout); err != nil {
		_ = incoming.CancelHold(h)
		return nil, fmt.Errorf("wait held: %w", err)
	}
	mstage("held")
	p, err := outgoing.Pay(outInvoice, h, outAmtMsat)
	if err != nil {
		if errors.Is(err, ErrLNPayUnresolved) {
			// The outgoing HTLC may still settle, and the incoming hold is the only thing
			// that pays for it. Cancelling the hold here is the one way to lose the asset;
			// leave it (it expires on its own CLTV) and let the operator settle from
			// listpays if the payment completes.
			return nil, fmt.Errorf("pay outgoing leg: %w (incoming hold LEFT IN PLACE)", err)
		}
		_ = incoming.CancelHold(h) // refund: nothing was delivered
		return nil, fmt.Errorf("pay outgoing leg: %w", err)
	}
	mstage("paid")
	if err := incoming.SettleHold(h, p); err != nil {
		return nil, fmt.Errorf("settle hold (outgoing already paid!): %w", err)
	}
	mstage("settled")
	return p, nil
}

// takerPayHold pays the maker's incoming hold by bare hash on the taker's
// OUTGOING leg and blocks until the maker settles, returning the revealed P.
func takerPayHold(outgoing LNLeg, makerNodeID string, h []byte, outAmtMsat uint64, finalCltv uint32, paymentSecret []byte) (preimage []byte, err error) {
	pre, err := outgoing.PayHash(makerNodeID, h, outAmtMsat, finalCltv, paymentSecret)
	if err != nil {
		return nil, fmt.Errorf("taker pay hold: %w", err)
	}
	return pre, nil
}

// --- BUY: taker buys the asset with BTC (maker holds BTC, pays the asset) ----

// PrepareTakerBuy (taker) issues the asset invoice the maker will pay.
func (s *PureLNSwap) PrepareTakerBuy(p []byte, assetAmtMsat uint64) (assetInvoice string, h []byte, err error) {
	return takerPrepare(s.assetLeg, p, assetAmtMsat)
}

// MakerRegisterHold (maker) registers the incoming BTC hold on h.
func (s *PureLNSwap) MakerRegisterHold(h []byte, btcAmtMsat uint64) error {
	return makerRegisterHold(s.btcLeg, h, btcAmtMsat)
}

// MakerFulfill (maker) waits for the held BTC, pays the taker's asset invoice
// (learning P), and settles the held BTC.
func (s *PureLNSwap) MakerFulfill(h []byte, assetInvoice string, assetAmtMsat uint64, holdTimeout time.Duration) (preimage []byte, err error) {
	return makerFulfill(s.btcLeg, s.assetLeg, h, assetInvoice, assetAmtMsat, holdTimeout)
}

// RunTakerBuy (taker) pays the maker's BTC hold by bare hash; blocks until settle.
func (s *PureLNSwap) RunTakerBuy(h []byte, makerBTCNodeID string, btcAmtMsat uint64, finalCltv uint32, paymentSecret []byte) (preimage []byte, err error) {
	return takerPayHold(s.btcLeg, makerBTCNodeID, h, btcAmtMsat, finalCltv, paymentSecret)
}

// --- SELL: taker sells the asset for BTC (maker holds the asset, pays BTC) ---

// PrepareTakerSell (taker) issues the BTC invoice the maker will pay.
func (s *PureLNSwap) PrepareTakerSell(p []byte, btcAmtMsat uint64) (btcInvoice string, h []byte, err error) {
	return takerPrepare(s.btcLeg, p, btcAmtMsat)
}

// MakerRegisterHoldSell (maker) registers the incoming ASSET hold on h.
func (s *PureLNSwap) MakerRegisterHoldSell(h []byte, assetAmtMsat uint64) error {
	return makerRegisterHold(s.assetLeg, h, assetAmtMsat)
}

// MakerFulfillSell (maker) waits for the held asset, pays the taker's BTC invoice
// (learning P), and settles the held asset.
func (s *PureLNSwap) MakerFulfillSell(h []byte, btcInvoice string, btcAmtMsat uint64, holdTimeout time.Duration) (preimage []byte, err error) {
	return makerFulfill(s.assetLeg, s.btcLeg, h, btcInvoice, btcAmtMsat, holdTimeout)
}

// RunTakerSell (taker) pays the maker's ASSET hold by bare hash; blocks until settle.
func (s *PureLNSwap) RunTakerSell(h []byte, makerAssetNodeID string, assetAmtMsat uint64, finalCltv uint32, paymentSecret []byte) (preimage []byte, err error) {
	return takerPayHold(s.assetLeg, makerAssetNodeID, h, assetAmtMsat, finalCltv, paymentSecret)
}
