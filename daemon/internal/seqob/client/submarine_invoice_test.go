package client

import "testing"

// A SUBMARINE INVOICE IS A WHOLE NUMBER OF SATOSHIS.
//
// The offer's price is quoted in sats, and the taker sizes its side in sats and then
// multiplies by 1000. The maker rounded in MSAT instead, which for a partial slice
// produces a sub-satoshi invoice the taker's exact-match check rejects:
//
//   reverse submarine: the invoice demands 6197920 msat != the offer's 6198000 msat
//
// Both sides round UP and both round in SATS, so they agree bit-for-bit.
func TestSubmarineInvoiceRoundsToWholeSats(t *testing.T) {
	// The live case: an 8% slice of a 77474-sat offer.
	const wholeMsat = 77_474 * 1000
	const whole = 5_000_000_000
	const take = 400_000_000

	got := proportionalInvoiceMsat(wholeMsat, take, whole)
	if got%1000 != 0 {
		t.Fatalf("invoice %d msat is not a whole number of sats", got)
	}
	if got != 6_198_000 {
		t.Fatalf("invoice = %d msat, want 6198000 (what the taker computes)", got)
	}
	// The msat-rounded value is what used to be sent, and what broke the swap.
	if old := ProportionalBtc(wholeMsat, take, whole); old == got {
		t.Skip("this offer does not exercise the sub-sat case")
	} else if old != 6_197_920 {
		t.Fatalf("sanity: the old msat rounding gave %d, expected 6197920", old)
	}
}

// The taker's own arithmetic, restated: size in sats, then convert.
func TestMakerInvoiceMatchesTakerExpectation(t *testing.T) {
	const whole = 5_000_000_000
	for _, c := range []struct{ wholeSats, take uint64 }{
		{77_474, 400_000_000}, {78_219, 400_000_000}, {77_567, 123_456_789},
		{80_000, 1}, {80_000, whole},
	} {
		maker := proportionalInvoiceMsat(c.wholeSats*1000, c.take, whole)
		takerSats := ProportionalBtc(c.wholeSats, c.take, whole) // taker sizes in sats
		if maker != takerSats*1000 {
			t.Fatalf("whole=%d take=%d: maker %d msat vs taker %d msat",
				c.wholeSats, c.take, maker, takerSats*1000)
		}
	}
}

// A whole-offer take is byte-identical to before the change.
func TestWholeTakeInvoiceUnchanged(t *testing.T) {
	const whole = 5_000_000_000
	const wholeMsat = 78_219 * 1000
	if got := proportionalInvoiceMsat(wholeMsat, whole, whole); got != wholeMsat {
		t.Fatalf("whole take = %d, want %d", got, wholeMsat)
	}
}
