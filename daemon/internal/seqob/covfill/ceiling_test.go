package covfill

import "testing"

// The covenant's rate is the maker's; the price the taker agreed to is the offer's.
// A fill whose terms demand more asset B than the taker's ceiling must be refused
// before anything is funded or broadcast.
func TestRefusesAboveTakerCeiling(t *testing.T) {
	o := mkOrder(5) // 3 B per A
	node := okNode(t, o, 100, fundRow(displayB(), 1000, 1), fundRow(feeDisplay, 1000, 2))
	p := params(o, node, 0, 500)
	p.MaxPayB = 299 // a full fill of 100 A costs 300 B
	refuse(t, p, node, "above the 299 ceiling")

	node = okNode(t, o, 100, fundRow(displayB(), 1000, 1), fundRow(feeDisplay, 1000, 2))
	p = params(o, node, 0, 500)
	p.MaxPayB = 300
	if _, err := FillCovenant(p); err != nil {
		t.Fatalf("a fill exactly at the ceiling must go through: %v", err)
	}
}

// Selected coins are locked in the wallet while the fill is assembled and released
// on every path that does not broadcast them, so concurrent fills from one wallet
// cannot pick the same coins.
func TestFundingCoinsLockedAndReleased(t *testing.T) {
	o := mkOrder(5)
	node := okNode(t, o, 100, fundRow(displayB(), 1000, 1), fundRow(feeDisplay, 1000, 2))
	if _, err := FillCovenant(params(o, node, 0, 500)); err != nil {
		t.Fatal(err)
	}
	if node.locked != 1 || node.unlocked != 0 {
		t.Fatalf("broadcast path: locked=%d unlocked=%d (want 1/0: coins stay locked until spent)", node.locked, node.unlocked)
	}
	node = okNode(t, o, 100, fundRow(displayB(), 1000, 1), fundRow(feeDisplay, 1000, 2))
	node.mempoolAllowed = false
	node.rejectReason = "scripted"
	if _, err := FillCovenant(params(o, node, 0, 500)); err == nil {
		t.Fatal("mempool rejection must refuse")
	}
	if node.locked != 1 || node.unlocked != 1 {
		t.Fatalf("refusal path: locked=%d unlocked=%d (want 1/1: coins released)", node.locked, node.unlocked)
	}
}
