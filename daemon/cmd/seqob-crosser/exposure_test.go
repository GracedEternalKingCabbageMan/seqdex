package main

import (
	"strings"
	"testing"
)

func planFor(t *testing.T, take, cost, rev uint64, buyFirst bool) *CrossPlan {
	t.Helper()
	bid := norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, take, rev), tBase, tQuote)
	ask := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, take, cost), tBase, tQuote)
	p := &CrossPlan{Bid: bid, Ask: ask, Take: take, Cost: cost, Revenue: rev}
	if buyFirst {
		p.First, p.Second = ask, bid
	} else {
		p.First, p.Second = bid, ask
	}
	return p
}

func TestWorstCaseDeltas(t *testing.T) {
	p := planFor(t, 100, 45, 50, true)
	bd, qd := worstCase(p)
	if bd != 100 || qd != -45 {
		t.Fatalf("buy-first worst case = (%d,%d), want (100,-45)", bd, qd)
	}
	p = planFor(t, 100, 45, 50, false)
	bd, qd = worstCase(p)
	if bd != -100 || qd != 50 {
		t.Fatalf("sell-first worst case = (%d,%d), want (-100,50)", bd, qd)
	}
}

func TestLedgerCapBlocksOversizedPlan(t *testing.T) {
	l := NewLedger(150, nil)
	p1 := planFor(t, 100, 45, 50, true)
	if err := l.Reserve(p1); err != nil {
		t.Fatalf("first reserve refused: %v", err)
	}
	// A second concurrent 100-base worst case would put base drift at 200 > 150.
	p2 := planFor(t, 100, 45, 50, true)
	if err := l.Reserve(p2); err == nil || !strings.Contains(err.Error(), "exposure cap") {
		t.Fatalf("cap did not block: %v", err)
	}
	// Releasing p1 frees the headroom.
	l.Release(p1)
	if err := l.Reserve(p2); err != nil {
		t.Fatalf("reserve after release refused: %v", err)
	}
}

func TestLedgerResidueAccumulatesAndCaps(t *testing.T) {
	l := NewLedger(150, nil)
	p := planFor(t, 100, 45, 50, true)
	if err := l.Reserve(p); err != nil {
		t.Fatal(err)
	}
	l.Release(p)
	l.RecordResidue(p) // leg 2 failed: long 100 base, -45 quote
	d := l.Drift()
	if d[tBase] != 100 || d[tQuote] != -45 {
		t.Fatalf("drift=%v", d)
	}
	// Settled drift now counts against the cap: another 100-base plan busts it.
	if err := l.Reserve(planFor(t, 100, 45, 50, true)); err == nil {
		t.Fatal("cap ignored settled drift")
	}
	// A SELL-first plan reduces base exposure and is admitted.
	if err := l.Reserve(planFor(t, 100, 45, 50, false)); err != nil {
		t.Fatalf("offsetting plan refused: %v", err)
	}
}

func TestLedgerSeedAndUncapped(t *testing.T) {
	l := NewLedger(0, map[string]int64{tBase: -7}) // 0 = uncapped
	if err := l.Reserve(planFor(t, 1_000_000, 1, 2, true)); err != nil {
		t.Fatalf("uncapped ledger refused: %v", err)
	}
	if l.Drift()[tBase] != -7 {
		t.Fatalf("seed lost: %v", l.Drift())
	}
}
