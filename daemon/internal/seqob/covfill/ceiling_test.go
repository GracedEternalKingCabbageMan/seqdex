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
