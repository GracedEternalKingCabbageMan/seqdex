package main

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

func match(numA, denA, lotA, numB, denB, lotB uint64) matcher.Match {
	return matcher.Match{
		RestingCovenant:  &seqobv1.CovenantTerms{RateNum: numA, RateDen: denA, MinLot: lotA},
		IncomingCovenant: &seqobv1.CovenantTerms{RateNum: numB, RateDen: denB, MinLot: lotB},
	}
}

// Both leaves ceil in their own maker's favour, so reciprocal rates that do not
// divide evenly miss a floor by an atom on the first pick; the most common cross,
// equal prices, must still settle.
func TestComputeFillsConvergesOnReciprocalRounding(t *testing.T) {
	// A sells X at 1 Y per 3 X; B sells Y at 3 X per 1 Y. lockedA=10, lockedB=10.
	m := match(1, 3, 1, 3, 1, 1)
	fillA, fillB, err := computeFills(m, 10, 10)
	if err != nil {
		t.Fatalf("equal-price cross rejected: %v", err)
	}
	if fillB < covenant.CeilPrice(fillA, 1, 3) || fillA < covenant.CeilPrice(fillB, 3, 1) {
		t.Fatalf("floors missed: fillA=%d fillB=%d", fillA, fillB)
	}
	if fillA == 0 || fillB == 0 {
		t.Fatal("empty fill")
	}
}

// A remainder below min_lot on either side is shrunk away, and the result still
// clears both floors.
func TestComputeFillsRespectsMinLot(t *testing.T) {
	m := match(1, 1, 5, 1, 1, 5) // 1:1, min_lot 5 both sides
	fillA, fillB, err := computeFills(m, 14, 9)
	if err != nil {
		t.Fatal(err)
	}
	if r := 14 - fillA; r != 0 && r < 5 {
		t.Fatalf("A remainder %d below min_lot", r)
	}
	if r := 9 - fillB; r != 0 && r < 5 {
		t.Fatalf("B remainder %d below min_lot", r)
	}
	if fillB < covenant.CeilPrice(fillA, 1, 1) || fillA < covenant.CeilPrice(fillB, 1, 1) {
		t.Fatalf("floors missed: fillA=%d fillB=%d", fillA, fillB)
	}
}
