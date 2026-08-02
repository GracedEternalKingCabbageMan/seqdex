package main

import "testing"

// TestOrderLegsTable pins the full family-pair ordering matrix from legorder.go:
// the slower/less-certain leg first; ties buy(ask)-first.
func TestOrderLegsTable(t *testing.T) {
	fams := []Family{FamCovenant, FamPureLN, FamSameChain, FamSubAsset, FamSubmarine, FamCross}
	for _, bidFam := range fams {
		for _, askFam := range fams {
			bid := norm(mkOffer(bidFam, SideBid, tBase, tQuote, 100, 50), tBase, tQuote)
			ask := norm(mkOffer(askFam, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
			first, second, reason := OrderLegs(bid, ask)
			if first == second {
				t.Fatalf("%v/%v: legs collapsed", bidFam, askFam)
			}
			var wantFirst *NormOrder
			switch {
			case famScore(askFam) < famScore(bidFam):
				wantFirst = ask
			case famScore(bidFam) < famScore(askFam):
				wantFirst = bid
			default:
				wantFirst = ask // tie: BUY first
			}
			if first != wantFirst {
				t.Errorf("bid=%v ask=%v: first=%v (%s), want %v",
					bidFam, askFam, first.Family, reason, wantFirst.Family)
			}
		}
	}
}

// Spot-check the table's marquee rows.
func TestOrderLegsExamples(t *testing.T) {
	cross := func(s Side) *NormOrder { return norm(mkOffer(FamCross, s, tBase, tQuote, 100, 47), tBase, tQuote) }
	pln := func(s Side) *NormOrder { return norm(mkOffer(FamPureLN, s, tBase, tQuote, 100, 47), tBase, tQuote) }

	// cross bid vs pureln ask: the cross leg (slow, two-chain) must run first.
	first, second, _ := OrderLegs(cross(SideBid), pln(SideAsk))
	if first.Family != FamCross || second.Family != FamPureLN {
		t.Fatalf("cross-vs-pureln: first=%v", first.Family)
	}
	// pureln vs pureln: tie -> the ask (buy) leg first.
	first, _, _ = OrderLegs(pln(SideBid), pln(SideAsk))
	if first.Side != SideAsk {
		t.Fatalf("tie should buy first, got %v", first.Side)
	}
}
