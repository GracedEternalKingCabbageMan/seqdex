package client

import "testing"

// THE PARTIAL QUOTE MUST PRICE THE SLICE, NOT THE WHOLE OFFER.
//
// A cross offer advertises allow_partial with a min_fill, the relay forwards
// take_amount, and the maker's post-fill path re-rests the remainder — but the
// HANDSHAKE quoted the offer's whole OfferAmount/WantAmount. So a taker asking for
// 8% of a resting offer was told the price of 100% of it, and the bridge's bind
// check killed the trade instantly with "maker wants 78219 BTC sats, above the
// offered 6248 — refuse".
//
// These pin the arithmetic the maker now quotes, using the exact live numbers.
func TestPartialQuotePricesTheSliceNotTheWholeOffer(t *testing.T) {
	const wholeAsset = 5_000_000_000 // 50 USDX
	const wholeBtc = 78_219          // sats for the whole offer
	const take = 399_500_000         // ~4 USDX, the slice the taker asked for

	got := ProportionalBtc(wholeBtc, take, wholeAsset)
	if got >= wholeBtc {
		t.Fatalf("a partial slice quoted %d sats, at or above the WHOLE offer's %d — "+
			"this is the refusal the user hit", got, wholeBtc)
	}
	// ceil(78219 * 399500000 / 5000000000) = ceil(6249.6...) = 6250
	if got != 6250 {
		t.Fatalf("slice price = %d, want 6250", got)
	}
}

// Rounding is the MAKER's favour on both sides, and the two directions round
// oppositely because the party paying the BTC flips.
func TestSliceRoundingFavoursTheMakerInBothDirections(t *testing.T) {
	const wholeAsset, wholeBtc, take = 3000, 1001, 1000

	// FORWARD: the TAKER pays BTC -> CEIL, so a partial never underpays the maker.
	fwd := ProportionalBtc(wholeBtc, take, wholeAsset)
	if fwd != 334 { // ceil(1001/3)
		t.Fatalf("forward slice = %d, want 334 (ceil)", fwd)
	}
	// REVERSE: the MAKER pays BTC -> FLOOR, so a partial never overpays the maker.
	rev := ProportionalBtcFloor(wholeBtc, take, wholeAsset)
	if rev != 333 { // floor(1001/3)
		t.Fatalf("reverse slice = %d, want 333 (floor)", rev)
	}
	if !(rev < fwd) {
		t.Fatal("the two directions must round oppositely; the maker is favoured in each")
	}
}

// A WHOLE take must be byte-identical to the pre-partial behaviour. This is what
// makes the change safe for every existing whole-offer lift.
func TestWholeTakeIsUnchanged(t *testing.T) {
	const wholeAsset, wholeBtc = 5_000_000_000, 78_219
	for _, take := range []uint64{wholeAsset, wholeAsset + 1, wholeAsset * 2} {
		if got := ProportionalBtc(wholeBtc, take, wholeAsset); got != wholeBtc {
			t.Fatalf("whole take %d quoted %d, want the offer's exact %d", take, got, wholeBtc)
		}
		if got := ProportionalBtcFloor(wholeBtc, take, wholeAsset); got != wholeBtc {
			t.Fatalf("whole take %d (floor) quoted %d, want %d", take, got, wholeBtc)
		}
	}
}

// The taker computes its expected BTC with the SAME ceil formula (xswap.js
// proportionalBtcCeil), and the bridge refuses on ANY excess — so the two must
// agree exactly, not merely closely. An off-by-one here is a refused trade.
func TestMakerQuoteMatchesTakerExpectationExactly(t *testing.T) {
	const wholeAsset, wholeBtc = 5_000_000_000, 78_219
	for _, take := range []uint64{1, 162_748_182, 399_500_000, 1_234_567_890, 4_999_999_999} {
		maker := ProportionalBtc(wholeBtc, take, wholeAsset)
		taker := takerCeilMirror(wholeBtc, take, wholeAsset) // the JS mirror, recomputed
		if maker != taker {
			t.Fatalf("take %d: maker quoted %d, taker expected %d — the bridge refuses on any excess",
				take, maker, taker)
		}
	}
}

// An independent restatement of xswap.js's proportionalBtcCeil, so this test fails
// if ProportionalBtc is ever "simplified" out of agreement with the taker.
func takerCeilMirror(wholeBtc, take, whole uint64) uint64 {
	if whole == 0 || take >= whole {
		return wholeBtc
	}
	prod := new(uint128).mul(wholeBtc, take)
	q, rem := prod.divMod(whole)
	if rem != 0 {
		q++
	}
	return q
}

// Minimal 128-bit product so the mirror does not itself overflow (1e8 sats * 2.1e15
// atoms overflows uint64), matching mulDiv64's reasoning.
type uint128 struct{ hi, lo uint64 }

func (u *uint128) mul(a, b uint64) *uint128 {
	const mask = 0xFFFFFFFF
	a0, a1 := a&mask, a>>32
	b0, b1 := b&mask, b>>32
	lo := a0 * b0
	mid1, mid2 := a1*b0, a0*b1
	hi := a1 * b1
	carry := (lo>>32 + mid1&mask + mid2&mask) >> 32
	u.lo = a * b
	u.hi = hi + mid1>>32 + mid2>>32 + carry
	return u
}

func (u *uint128) divMod(d uint64) (uint64, uint64) {
	// The callers guarantee the quotient fits in uint64 (take < whole), so a simple
	// long division over the two limbs is exact.
	rem := uint64(0)
	q := uint64(0)
	for i := 127; i >= 0; i-- {
		var bit uint64
		if i >= 64 {
			bit = (u.hi >> (i - 64)) & 1
		} else {
			bit = (u.lo >> i) & 1
		}
		rem = rem<<1 | bit
		if rem >= d {
			rem -= d
			if i < 64 {
				q |= 1 << i
			}
		}
	}
	return q, rem
}
