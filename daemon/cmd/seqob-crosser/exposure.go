package main

// exposure.go — the per-asset inventory-drift ledger.
//
// A crossing that completes both legs is inventory-neutral in base and positive
// in quote (the spread). Drift enters ONLY when leg 2 fails after leg 1 settled;
// the ledger records that residue per asset (signed atoms, + = long, - = short)
// and the planner refuses any crossing whose worst case would push an asset's
// |drift| past -max-exposure-atoms. Reservations model the in-flight worst case
// (leg 1 settles, leg 2 fails) so concurrent pairs cannot jointly blow the cap.

import (
	"fmt"
	"sync"
)

type Ledger struct {
	mu       sync.Mutex
	drift    map[string]int64 // settled residue per asset
	reserved map[string]int64 // in-flight worst-case deltas
	maxAbs   int64            // 0 = uncapped
}

func NewLedger(maxAbs uint64, seed map[string]int64) *Ledger {
	l := &Ledger{drift: map[string]int64{}, reserved: map[string]int64{}, maxAbs: int64(maxAbs)}
	for k, v := range seed {
		l.drift[k] = v
	}
	return l
}

// worstCase returns the base/quote deltas of the plan's worst failure: leg 1
// settled, leg 2 failed.
//
//	first == ask (buy first):  long base +take, quote -cost
//	first == bid (sell first): short base -take, quote +revenue
func worstCase(p *CrossPlan) (baseDelta, quoteDelta int64) {
	if p.First == p.Ask {
		return int64(p.Take), -int64(p.Cost)
	}
	return -int64(p.Take), int64(p.Revenue)
}

// Reserve admits a plan against the cap and holds its worst case until
// Settle/Fail. Returns an error naming the violated asset when over cap.
func (l *Ledger) Reserve(p *CrossPlan) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	bd, qd := worstCase(p)
	for _, c := range []struct {
		asset string
		d     int64
	}{{p.Bid.Base, bd}, {p.Bid.Quote, qd}} {
		if l.maxAbs > 0 && abs64(l.drift[c.asset]+l.reserved[c.asset]+c.d) > l.maxAbs {
			return fmt.Errorf("exposure cap: asset %s drift %d + reserved %d + worst-case %d exceeds ±%d",
				short(c.asset), l.drift[c.asset], l.reserved[c.asset], c.d, l.maxAbs)
		}
	}
	l.reserved[p.Bid.Base] += bd
	l.reserved[p.Bid.Quote] += qd
	return nil
}

// Release drops the reservation (call on ANY terminal outcome, before Record*).
func (l *Ledger) Release(p *CrossPlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bd, qd := worstCase(p)
	l.reserved[p.Bid.Base] -= bd
	l.reserved[p.Bid.Quote] -= qd
}

// RecordResidue books the drift of a leg-2 failure after leg 1 settled.
func (l *Ledger) RecordResidue(p *CrossPlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bd, qd := worstCase(p)
	l.drift[p.Bid.Base] += bd
	l.drift[p.Bid.Quote] += qd
}

// Drift snapshots the settled residue (for the journal + logs).
func (l *Ledger) Drift() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.drift))
	for k, v := range l.drift {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:6] + ".." + s[len(s)-6:]
}
