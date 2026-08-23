package watcher

import (
	"context"
	"fmt"
	"strings"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// SubmitProbe is the relay validator's liveness probe backed by the chain view:
// a COVENANT offer is accepted only if its advertised outpoint exists unspent at
// the tip, pays the program derived from its own terms, carries asset A, and
// holds exactly the amount the offer advertises. Every other offer passes (the
// interactive and cross rails are proven at settlement, not at submit).
//
// Without this, a submitted covenant with a bogus outpoint rested in the book
// until the watcher's next pass and cost that pass a window of getblock scans;
// and a maker could advertise more than it had locked.
//
// A node that cannot be asked is not chain evidence: the probe passes on an RPC
// error and leaves the decision to the watcher, which reconciles within its
// interval. It fails closed only on what the chain actually says.
type SubmitProbe struct {
	Chain ChainView
}

// CheckOffer implements validator.LivenessProbe.
func (p SubmitProbe) CheckOffer(_ context.Context, o *seqobv1.Offer) error {
	return ProbeCovenant(p.Chain, o)
}

// ProbeCovenant is CheckOffer without the interface: nil for a non-covenant offer
// or when the chain could not be asked; an error when the chain contradicts the
// offer.
func ProbeCovenant(chain ChainView, o *seqobv1.Offer) error {
	t := o.GetCovenant()
	if t == nil || chain == nil {
		return nil
	}
	exp, err := expectFromTerms(t)
	if err != nil {
		return fmt.Errorf("covenant terms: %w", err)
	}
	st, err := chain.Inspect(t.GetCovenantTxid(), t.GetCovenantVout(), exp.OrderSPKHex, exp.AssetADisplay)
	if err != nil {
		return nil // node unavailable: not evidence; the watcher reconciles
	}
	if !st.Unspent {
		return fmt.Errorf("covenant outpoint %s:%d is not an unspent output at the tip", t.GetCovenantTxid(), t.GetCovenantVout())
	}
	if !strings.EqualFold(st.SPKHex, exp.OrderSPKHex) {
		return fmt.Errorf("covenant outpoint does not pay the program derived from its terms")
	}
	if !strings.EqualFold(st.AssetDisplay, exp.AssetADisplay) {
		return fmt.Errorf("covenant outpoint holds asset %s, not asset_a %s", st.AssetDisplay, exp.AssetADisplay)
	}
	if st.Value != o.GetOfferAmount() {
		return fmt.Errorf("covenant outpoint holds %d atoms, offer advertises %d", st.Value, o.GetOfferAmount())
	}
	return nil
}
