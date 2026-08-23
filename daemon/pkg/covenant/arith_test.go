package covenant

import (
	"math"
	"testing"
)

// The FILL leaf multiplies with OP_MUL64, which is signed and pushes false on
// overflow: an order whose locked*rate_num exceeds 2^63-1 can never be filled and
// its asset is stuck until REFUND. The planner must refuse it, and CeilPrice must
// never hand back a wrapped product.
func TestPlanFillRefusesLeafOverflow(t *testing.T) {
	o := fixedOrder()
	o.RateNum, o.RateDen = 4e11+1, 1e10 // 4000.00000001 B per A, coprime with a 1e10 lock
	o.MinLot = 1
	locked := uint64(1e10)
	if err := o.CheckArithmetic(locked); err == nil {
		t.Fatal("locked*rate_num = 4e21 must be refused")
	}
	if _, err := o.PlanFill(locked, locked, 0); err == nil {
		t.Fatal("PlanFill accepted an order the leaf cannot evaluate")
	}
	o.RateNum, o.RateDen = 4000, 1 // the same price, reduced
	if err := o.CheckArithmetic(locked); err != nil {
		t.Fatalf("reduced rate must pass: %v", err)
	}
	if _, err := o.PlanFill(locked, locked, 0); err != nil {
		t.Fatalf("reduced rate must plan: %v", err)
	}
}

func TestCeilPriceDoesNotWrap(t *testing.T) {
	if got := CeilPrice(1e10, 4e11+1, 1e10); got != 4e11+1 {
		t.Fatalf("128-bit product: got %d, want %d", got, uint64(4e11+1))
	}
	if got := CeilPrice(math.MaxUint64, 2, 1); got != math.MaxUint64 {
		t.Fatalf("an unrepresentable quotient must saturate, got %d", got)
	}
	if got := CeilPrice(90*1e8, 3, 7); got != (90*1e8*3+6)/7 {
		t.Fatalf("small case: got %d", got)
	}
}
