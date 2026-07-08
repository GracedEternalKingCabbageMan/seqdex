package covenant

import (
	"bytes"
	"testing"
)

// counterparties builds orderA (sells X wants Y at 3 Y per X) and orderB (sells
// Y wants X at 1 X per 3 Y), a clean reciprocal cross.
func counterparties() (Order, Order) {
	var x, y, mx0, mx1 [32]byte
	for i := 0; i < 32; i++ {
		x[i] = byte(i)
		y[i] = byte(32 + i)
		mx0[i] = 0x22
		mx1[i] = 0x33
	}
	prog0 := bytes.Repeat([]byte{0x11}, 32)
	prog1 := bytes.Repeat([]byte{0x44}, 32)
	orderA := DefaultOrder(x, y, 3, 1, 5, prog0, 400, mx0) // required Y = fillA*3
	orderB := DefaultOrder(y, x, 1, 3, 5, prog1, 400, mx1) // required X = ceil(fillB/3)
	return orderA, orderB
}

func TestJointSettlementExactCross(t *testing.T) {
	a, b := counterparties()
	s, err := PlanJointSettlement(a, 30, 30, b, 90, 90)
	if err != nil {
		t.Fatal(err)
	}
	// Fixed index map: credits at 0 and 2, reserved slots at 1 and 3.
	if s.Leg0.CreditIndex != 0 || s.Leg0.RemainderIndex != 1 {
		t.Fatalf("leg0 indices %d/%d", s.Leg0.CreditIndex, s.Leg0.RemainderIndex)
	}
	if s.Leg1.CreditIndex != 2 || s.Leg1.RemainderIndex != 3 {
		t.Fatalf("leg1 indices %d/%d", s.Leg1.CreditIndex, s.Leg1.RemainderIndex)
	}
	// maker0 is paid all Y taken from covB (90); maker1 all X taken from covA (30).
	if s.Leg0.CreditValue != 90 || s.Leg0.CreditFloor != 90 {
		t.Fatalf("leg0 credit %d floor %d, want 90/90", s.Leg0.CreditValue, s.Leg0.CreditFloor)
	}
	if s.Leg1.CreditValue != 30 || s.Leg1.CreditFloor != 30 {
		t.Fatalf("leg1 credit %d floor %d, want 30/30", s.Leg1.CreditValue, s.Leg1.CreditFloor)
	}
	// maker0 receives Y (== orderA.AssetB); maker1 receives X (== orderB.AssetB).
	if !bytes.Equal(s.Leg0.CreditAsset[:], a.AssetB[:]) || !bytes.Equal(s.Leg1.CreditAsset[:], b.AssetB[:]) {
		t.Fatal("credit assets not the counterparty's wanted asset")
	}
	// Both full fills => both reserved slots need settler gap outputs.
	if len(s.GapSlots) != 2 {
		t.Fatalf("gap slots %v, want [1 3]", s.GapSlots)
	}
	if s.Leg0.Partial || s.Leg1.Partial {
		t.Fatal("exact cross should be a full fill on both legs")
	}
	// The gap outputs must not carry the sold asset, but bitcoin (zero id here) is fine.
	if err := s.Leg0.GapAsset(a.AssetA); err == nil {
		t.Fatal("gap must reject the sold asset X")
	}
	var btc [32]byte
	if err := s.Leg0.GapAsset(btc); err != nil {
		t.Fatalf("bitcoin gap should be allowed: %v", err)
	}
}

func TestJointSettlementPartialReRests(t *testing.T) {
	a, b := counterparties()
	// covB is larger: 180 Y locked, only 90 taken -> 90 Y re-rests as a covenant.
	s, err := PlanJointSettlement(a, 30, 30, b, 180, 90)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Leg1.Partial || s.Leg1.Remainder != 90 {
		t.Fatalf("leg1 remainder %d partial=%v, want 90/true", s.Leg1.Remainder, s.Leg1.Partial)
	}
	// leg1's remainder re-pays the SAME covenant spk in the sold asset Y.
	if !bytes.Equal(s.Leg1.OrderSPK, mustSPK(t, b)) {
		t.Fatal("remainder must self-replicate the covenant scriptPubKey")
	}
	if !bytes.Equal(s.Leg1.RemainderAsset[:], b.AssetA[:]) {
		t.Fatal("remainder asset must be the sold asset Y")
	}
	// Only leg0 (full fill) needs a gap output, at slot 1.
	if len(s.GapSlots) != 1 || s.GapSlots[0] != 1 {
		t.Fatalf("gap slots %v, want [1]", s.GapSlots)
	}
}

func TestJointSettlementRejectsNonCounterparties(t *testing.T) {
	a, _ := counterparties()
	// Two orders both selling X: not counterparties.
	bad := a
	if _, err := PlanJointSettlement(a, 30, 30, bad, 30, 30); err == nil {
		t.Fatal("expected non-counterparty rejection")
	}
}

func TestJointSettlementRejectsUnderpayingCross(t *testing.T) {
	a, b := counterparties()
	// fillB=80 pays maker0 only 80 Y but the covenant needs ceil(30*3)=90.
	if _, err := PlanJointSettlement(a, 30, 30, b, 90, 80); err == nil {
		t.Fatal("expected underpaid-maker0 rejection")
	}
	// fillA=20 pays maker1 only 20 X but covenant needs ceil(90/3)=30.
	if _, err := PlanJointSettlement(a, 30, 20, b, 90, 90); err == nil {
		t.Fatal("expected underpaid-maker1 rejection")
	}
}

func mustSPK(t *testing.T, o Order) []byte {
	t.Helper()
	tap, err := o.Derive()
	if err != nil {
		t.Fatal(err)
	}
	return tap.ScriptPubKey
}
