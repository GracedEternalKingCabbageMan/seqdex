package main

// xpartial_test.go: the CLI-side minimum-slice guard. A partial -amount below the
// offer's advertised min_fill, or one that prices to a sub-dust BTC leg, must be
// rejected before any coins move; a whole take and the smallest safe slice pass.

import "testing"

func TestGuardPartialBtcLeg(t *testing.T) {
	const wholeSeq = uint64(5_000_000)
	const minFill = uint64(509_200) // MinFillBase(25000, 5M, 1000)
	const floor = uint64(2546)      // MinSafeBtcLegSats(1000)

	// Whole take (takeSeq >= wholeSeq) always passes, whatever the leg size.
	if err := guardPartialBtcLeg(wholeSeq, wholeSeq, 25_000, minFill, 1000); err != nil {
		t.Fatalf("whole take must pass: %v", err)
	}

	// A partial below the advertised min_fill is rejected (with a min_fill message).
	if err := guardPartialBtcLeg(minFill-1, wholeSeq, floor, minFill, 1000); err == nil {
		t.Fatal("slice below min_fill must be rejected")
	}

	// A partial at min_fill whose BTC leg clears the floor passes.
	if err := guardPartialBtcLeg(minFill, wholeSeq, floor, minFill, 1000); err != nil {
		t.Fatalf("smallest safe slice must pass: %v", err)
	}

	// With no advertised min_fill (0), a sub-dust BTC leg is still rejected...
	if err := guardPartialBtcLeg(1, wholeSeq, 1, 0, 1000); err == nil {
		t.Fatal("sub-dust BTC leg must be rejected even without a min_fill")
	}
	// ...and a slice whose BTC leg clears the floor still passes.
	if err := guardPartialBtcLeg(minFill, wholeSeq, floor, 0, 1000); err != nil {
		t.Fatalf("safe slice must pass without a min_fill: %v", err)
	}

	// A larger spend fee raises the floor: a leg that cleared 2546 no longer clears
	// 546 + 2*5000 = 10546.
	if err := guardPartialBtcLeg(minFill, wholeSeq, floor, 0, 5000); err == nil {
		t.Fatal("a leg below the raised floor must be rejected at a higher spend fee")
	}
}
