package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// PureLNSwap is a pure-Lightning atomic swap between two Lightning legs — a
// Sequentia issued-asset leg (e.g. GOLD) and a "BTC" leg (the policy asset, or a
// real Bitcoin-LN node) — stitched by ONE shared secret. Unlike a submarine
// swap it has NO on-chain leg and therefore NO anchor-depth gate: both legs are
// off-chain LN payments, so the happy path settles in seconds.
//
// Atomicity for "taker buys the asset with BTC" (RunMakerBuy / RunTakerBuy):
// the maker's INCOMING BTC leg is a hold, released only by the preimage P; the
// maker learns P solely by paying the taker's asset invoice (the taker generated
// P and put its hash on both the asset invoice and the BTC hold). So the maker
// cannot take the BTC without delivering the asset, and the taker cannot take
// the asset without releasing the BTC. Worst case is a hold timeout -> refund.
type PureLNSwap struct {
	assetLeg LNLeg // pays/receives the Sequentia issued asset (e.g. GOLD)
	btcLeg   LNLeg // pays/receives "BTC" (policy asset / real BTC-LN)
}

// NewPureLNSwap builds a swap over an asset leg and a BTC leg. For a maker both
// legs are the maker's two nodes; for a taker, the taker's two nodes.
func NewPureLNSwap(assetLeg, btcLeg LNLeg) *PureLNSwap {
	return &PureLNSwap{assetLeg: assetLeg, btcLeg: btcLeg}
}

// PrepareTakerBuy (taker) issues the asset invoice the maker will pay, on the
// taker-chosen preimage p. Returns the asset invoice (BOLT11) and h = SHA256(p),
// which the taker hands to the maker (over the courier) to register the BTC hold.
func (s *PureLNSwap) PrepareTakerBuy(p []byte, assetAmtMsat uint64) (assetInvoice string, h []byte, err error) {
	sum := sha256.Sum256(p)
	label := "pureln-buy-" + hex.EncodeToString(sum[:6])
	inv, err := s.assetLeg.CreateInvoice(p, assetAmtMsat, 0, label, "pure-ln buy: asset leg")
	if err != nil {
		return "", nil, fmt.Errorf("create asset invoice: %w", err)
	}
	return inv, sum[:], nil
}

// RunTakerBuy (taker) pays the maker's BTC hold by bare hash and blocks until
// the maker settles it — which happens only after the maker has paid the taker's
// asset invoice. Returns the preimage the settle revealed (must equal the p the
// taker chose). makerBTCNodeID is the maker's BTC-leg node id.
func (s *PureLNSwap) RunTakerBuy(h []byte, makerBTCNodeID string, btcAmtMsat uint64, finalCltv uint32, paymentSecret []byte) (preimage []byte, err error) {
	pre, err := s.btcLeg.PayHash(makerBTCNodeID, h, btcAmtMsat, finalCltv, paymentSecret)
	if err != nil {
		return nil, fmt.Errorf("taker pay BTC hold: %w", err)
	}
	return pre, nil
}

// MakerRegisterHold (maker, step 1) registers the incoming BTC hold on h. The
// maker does NOT know P yet. Call this BEFORE telling the taker to pay (over the
// courier), so the taker's HTLC is held rather than failed as unknown.
func (s *PureLNSwap) MakerRegisterHold(h []byte, btcAmtMsat uint64) error {
	label := "pureln-buy-" + hex.EncodeToString(h[:6])
	if _, err := s.btcLeg.CreateHoldInvoice(h, btcAmtMsat, 0, label, "pure-ln buy: BTC hold"); err != nil {
		return fmt.Errorf("register BTC hold: %w", err)
	}
	return nil
}

// MakerFulfill (maker, steps 2-4) waits for the taker's BTC HTLC to be held,
// pays the taker's asset invoice (LEARNING P and delivering the asset), then
// settles the held BTC with P. On any failure before the settle it cancels the
// hold, so the taker's BTC is refunded and neither leg completes. Returns the
// preimage learned by paying the asset leg.
func (s *PureLNSwap) MakerFulfill(h []byte, assetInvoice string, assetAmtMsat uint64, holdTimeout time.Duration) (preimage []byte, err error) {
	// 2. Wait for the taker to lock the BTC.
	if _, err = s.btcLeg.WaitHeld(h, holdTimeout); err != nil {
		_ = s.btcLeg.CancelHold(h)
		return nil, fmt.Errorf("wait BTC held: %w", err)
	}

	// 3. Pay the taker's asset invoice; this reveals P (and delivers the asset).
	p, err := s.assetLeg.Pay(assetInvoice, h, assetAmtMsat)
	if err != nil {
		_ = s.btcLeg.CancelHold(h) // refund the taker's BTC: nothing was delivered
		return nil, fmt.Errorf("pay asset leg: %w", err)
	}

	// 4. Settle the held BTC with the now-known P (take the incoming BTC).
	if err := s.btcLeg.SettleHold(h, p); err != nil {
		return nil, fmt.Errorf("settle BTC hold (asset already paid!): %w", err)
	}
	return p, nil
}
