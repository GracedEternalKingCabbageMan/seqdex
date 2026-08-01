package covenant

import "testing"

func TestPlanFillFullAndPartial(t *testing.T) {
	o := fixedOrder() // rate 3/7, min_lot 5e8
	// Full fill of a 90e8 covenant at input index 1 -> credit 2, remainder 3.
	locked := uint64(90 * 1e8)
	full, err := o.PlanFill(locked, locked, 1)
	if err != nil {
		t.Fatal(err)
	}
	if full.CreditIndex != 2 || full.RemainderIndex != 3 {
		t.Fatalf("index map wrong: credit %d rem %d", full.CreditIndex, full.RemainderIndex)
	}
	if full.Partial || full.Remainder != 0 {
		t.Fatal("full fill should have no remainder")
	}
	if full.RequiredB != CeilPrice(locked, 3, 7) {
		t.Fatalf("required_b = %d, want %d", full.RequiredB, CeilPrice(locked, 3, 7))
	}

	// Partial fill leaves a >= min_lot remainder.
	part, err := o.PlanFill(locked, 30*1e8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !part.Partial || part.Remainder != 60*1e8 {
		t.Fatalf("partial remainder = %d, want %de8", part.Remainder, 60)
	}
	if part.CreditIndex != 0 || part.RemainderIndex != 1 {
		t.Fatal("k=0 index map wrong")
	}
}

func TestPlanFillRejectsBelowMinLot(t *testing.T) {
	o := fixedOrder() // min_lot 5e8
	locked := uint64(90 * 1e8)
	// filled below min_lot.
	if _, err := o.PlanFill(locked, 1e8, 0); err == nil {
		t.Fatal("expected below-min_lot fill to be rejected")
	}
	// remainder below min_lot (locked-filled = 2e8 < 5e8).
	if _, err := o.PlanFill(locked, locked-2*1e8, 0); err == nil {
		t.Fatal("expected below-min_lot remainder to be rejected")
	}
}

func TestPlanJointFillIndexMap(t *testing.T) {
	a := fixedOrder()
	b := fixedOrder() // a second covenant standing in for the counterparty
	jp, err := PlanJointFill(a, 90*1e8, 90*1e8, b, 30*1e8, 30*1e8)
	if err != nil {
		t.Fatal(err)
	}
	// A-covenant is input 0: its maker credit at output 0.
	if jp.A.InputIndex != 0 || jp.A.CreditIndex != 0 {
		t.Fatalf("A input/credit = %d/%d, want 0/0", jp.A.InputIndex, jp.A.CreditIndex)
	}
	// B-covenant is input 1: its maker credit at output 2 (never colliding with A's).
	if jp.B.InputIndex != 1 || jp.B.CreditIndex != 2 {
		t.Fatalf("B input/credit = %d/%d, want 1/2", jp.B.InputIndex, jp.B.CreditIndex)
	}
	if jp.A.CreditIndex == jp.B.CreditIndex {
		t.Fatal("credit outputs must not alias across covenant inputs")
	}
	// Both witnesses are the introspection-only [leaf, control_block].
	if len(jp.A.Witness()) != 2 || len(jp.B.Witness()) != 2 {
		t.Fatal("covenant witnesses must be [leaf, control_block]")
	}
}
