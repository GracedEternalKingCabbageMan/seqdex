package main

import (
	"strings"
	"testing"
)

func gateOff() GateConfig { return GateConfig{MinProfitBps: 0, SpendFeeAtoms: 0} }

func TestClampOverlapWholeTakes(t *testing.T) {
	bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 100, 50), tBase, tQuote)
	ask := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
	take, why := ClampOverlap(bid, ask)
	if take != 100 || why != "" {
		t.Fatalf("equal sizes: take=%d why=%q", take, why)
	}
}

func TestClampOverlapPartialRules(t *testing.T) {
	t.Run("partial leg clamps to the smaller side", func(t *testing.T) {
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 60, 30), tBase, tQuote)
		ask := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
		take, _ := ClampOverlap(bid, ask)
		if take != 60 {
			t.Fatalf("take=%d want 60", take)
		}
	})
	t.Run("non-sliceable family refuses a partial", func(t *testing.T) {
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 60, 30), tBase, tQuote)
		ask := norm(mkOffer(FamSubmarine, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 0 || !strings.Contains(why, "whole-take only") {
			t.Fatalf("take=%d why=%q", take, why)
		}
	})
	t.Run("allow_partial=false refuses a partial", func(t *testing.T) {
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 60, 30), tBase, tQuote)
		ask := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45, withNoPartial()), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 0 || !strings.Contains(why, "forbids partial") {
			t.Fatalf("take=%d why=%q", take, why)
		}
	})
	t.Run("min_fill floors the overlap", func(t *testing.T) {
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 20, 10), tBase, tQuote)
		ask := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45, withMinFill(30)), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 0 || !strings.Contains(why, "min_fill") {
			t.Fatalf("take=%d why=%q", take, why)
		}
	})
	t.Run("submarine whole take is fine", func(t *testing.T) {
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 150, 75), tBase, tQuote)
		ask := norm(mkOffer(FamSubmarine, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 100 || why != "" {
			t.Fatalf("take=%d why=%q", take, why)
		}
	})
}

func TestClampOverlapCovenantMinLot(t *testing.T) {
	t.Run("dust remainder shrinks the take to size-min_lot", func(t *testing.T) {
		// Covenant ask of 100 with min_lot 20; bid wants 90 -> remainder 10 < 20,
		// so the take reduces to 80 (largest fill leaving a valid remainder).
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 90, 45), tBase, tQuote)
		ask := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 100, 45, withMinLot(20)), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 80 || why != "" {
			t.Fatalf("take=%d why=%q, want 80", take, why)
		}
	})
	t.Run("fill below min_lot refuses", func(t *testing.T) {
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 10, 5), tBase, tQuote)
		ask := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 100, 45, withMinLot(20)), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 0 || !strings.Contains(why, "min_lot") {
			t.Fatalf("take=%d why=%q", take, why)
		}
	})
	t.Run("covenant reduction re-checks the other leg's min_fill", func(t *testing.T) {
		// Reduction to 80 would drop the bid to a partial below its min_fill 85.
		bid := norm(mkOffer(FamCross, SideBid, tBase, tQuote, 90, 45, withMinFill(85)), tBase, tQuote)
		ask := norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 100, 45, withMinLot(20)), tBase, tQuote)
		take, why := ClampOverlap(bid, ask)
		if take != 0 || !strings.Contains(why, "min_fill") {
			t.Fatalf("take=%d why=%q", take, why)
		}
	})
}

func TestProfitabilityGate(t *testing.T) {
	bid := norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, 100, 50), tBase, tQuote) // pays 0.50/base
	ask := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote) // asks 0.45/base
	askAt50 := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 50), tBase, tQuote)

	t.Run("never at a loss: equal prices refuse", func(t *testing.T) {
		p := &CrossPlan{Bid: bid, Ask: askAt50, Take: 100, Cost: askAt50.CostFor(100), Revenue: bid.RevenueFor(100)}
		if ok, _ := Profitable(p, gateOff()); ok {
			t.Fatal("zero-spread crossing passed the gate")
		}
	})
	t.Run("gross must exceed estimated leg costs", func(t *testing.T) {
		p := &CrossPlan{Bid: bid, Ask: ask, Take: 100, Cost: 45, Revenue: 50}
		// pureln+pureln = 0 on-chain spends: passes even with a spend fee set.
		if ok, why := Profitable(p, GateConfig{SpendFeeAtoms: 100}); !ok {
			t.Fatalf("LN-only crossing failed cost gate: %s", why)
		}
		// cross ask leg = 2 spends; 2*3 = 6 >= gross 5 -> refuse.
		p2 := &CrossPlan{Bid: bid, Ask: norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote),
			Take: 100, Cost: 45, Revenue: 50}
		if ok, _ := Profitable(p2, GateConfig{SpendFeeAtoms: 3}); ok {
			t.Fatal("crossing that cannot cover leg costs passed")
		}
	})
	t.Run("bps floor on deployed quote", func(t *testing.T) {
		// net 5 on cost 45 = ~1111 bps: passes 1000, fails 1200.
		p := &CrossPlan{Bid: bid, Ask: ask, Take: 100, Cost: 45, Revenue: 50}
		if ok, why := Profitable(p, GateConfig{MinProfitBps: 1000}); !ok {
			t.Fatalf("1111bps crossing failed a 1000bps floor: %s", why)
		}
		p = &CrossPlan{Bid: bid, Ask: ask, Take: 100, Cost: 45, Revenue: 50}
		if ok, _ := Profitable(p, GateConfig{MinProfitBps: 1200}); ok {
			t.Fatal("1111bps crossing passed a 1200bps floor")
		}
	})
}

func TestDetectFindsBestExecutableCross(t *testing.T) {
	orders := []*NormOrder{
		norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote),   // best ask 0.45
		norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 48), tBase, tQuote),    // worse ask
		norm(mkOffer(FamSubAsset, SideBid, tBase, tQuote, 100, 50), tBase, tQuote), // best bid 0.50
		norm(mkOffer(FamCross, SideBid, tBase, tQuote, 100, 40), tBase, tQuote),    // worse bid
	}
	d := Detect(orders, execAll, gateOff())
	if d.Plan == nil {
		t.Fatalf("no plan: %s", d.Skip)
	}
	p := d.Plan
	if p.Ask.QuoteNum != 45 || p.Bid.QuoteNum != 50 || p.Take != 100 {
		t.Fatalf("picked wrong levels: ask=%d bid=%d take=%d", p.Ask.QuoteNum, p.Bid.QuoteNum, p.Take)
	}
	if p.Cost != 45 || p.Revenue != 50 || p.Gross != 5 {
		t.Fatalf("economics wrong: %+v", p)
	}
}

func TestDetectExecutabilityFiltersBestLevel(t *testing.T) {
	// The best ask is a covenant (inexecutable); detection must fall through to
	// the next executable ask, not execute the covenant and not give up.
	orders := []*NormOrder{
		norm(mkOffer(FamCovenant, SideAsk, tBase, tQuote, 100, 40), tBase, tQuote),
		norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote),
		norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, 100, 50), tBase, tQuote),
	}
	noCov := func(n *NormOrder) (bool, string) {
		if n.Family == FamCovenant {
			return false, "no single-covenant taker driver"
		}
		return true, ""
	}
	d := Detect(orders, noCov, gateOff())
	if d.Plan == nil {
		t.Fatalf("no plan: %s", d.Skip)
	}
	if d.Plan.Ask.Family != FamPureLN || d.Plan.Ask.QuoteNum != 45 {
		t.Fatalf("did not fall through past the inexecutable covenant: %+v", d.Plan.Ask)
	}
}

func TestDetectNoCross(t *testing.T) {
	orders := []*NormOrder{
		norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 50), tBase, tQuote),
		norm(mkOffer(FamPureLN, SideBid, tBase, tQuote, 100, 45), tBase, tQuote),
	}
	d := Detect(orders, execAll, gateOff())
	if d.Plan != nil || !strings.Contains(d.Skip, "no cross") {
		t.Fatalf("plan=%v skip=%q", d.Plan, d.Skip)
	}
}

func TestDetectRefusesSelfCross(t *testing.T) {
	ask := mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45)
	bid := mkOffer(FamPureLN, SideBid, tBase, tQuote, 100, 50)
	bid.MakerPubkey = ask.MakerPubkey // same maker on both sides
	d := Detect([]*NormOrder{norm(ask, tBase, tQuote), norm(bid, tBase, tQuote)}, execAll, gateOff())
	if d.Plan != nil || !strings.Contains(d.Skip, "same maker") {
		t.Fatalf("same-maker cross executed: plan=%v skip=%q", d.Plan, d.Skip)
	}
}

func TestSortBookPriceTimePriority(t *testing.T) {
	a := norm(mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45, withCreatedAt(200)), tBase, tQuote)
	b := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45, withCreatedAt(100)), tBase, tQuote) // same price, older
	c := norm(mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 44), tBase, tQuote)                     // better price
	asks := []*NormOrder{a, b, c}
	SortBook(asks)
	if asks[0] != c || asks[1] != b || asks[2] != a {
		t.Fatalf("ask order wrong: %v", []uint64{asks[0].QuoteNum, asks[1].QuoteNum, asks[2].QuoteNum})
	}
}
