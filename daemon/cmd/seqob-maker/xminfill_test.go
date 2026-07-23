package main

// xminfill_test.go: the maker's minimum-slice wiring — buildCrossOffer advertises
// a dust+fee-floor min_fill, and the serve loop's re-rest never leaves a sub-
// min_fill dust remainder (it drops to a full fill instead).

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// TestCrossReRestRemainder: the covenant-mirrored 'remainder == 0 OR remainder >=
// min_fill' decision. A whole fill and a sub-min_fill dust remainder both drop to
// a full fill (reRest=false); a healthy remainder re-rests.
func TestCrossReRestRemainder(t *testing.T) {
	// Whole fill / zero fill: nothing to re-rest.
	if _, r := crossReRestRemainder(100, 100, 10); r {
		t.Fatal("whole fill must not re-rest")
	}
	if _, r := crossReRestRemainder(100, 0, 10); r {
		t.Fatal("zero fill must not re-rest")
	}
	// Healthy remainder (> min_fill): re-rest the remainder.
	if rem, r := crossReRestRemainder(100, 40, 10); !r || rem != 60 {
		t.Fatalf("want re-rest of 60, got %d %v", rem, r)
	}
	// Remainder exactly == min_fill: re-rest (>= floor).
	if rem, r := crossReRestRemainder(100, 90, 10); !r || rem != 10 {
		t.Fatalf("remainder == min_fill must re-rest 10, got %d %v", rem, r)
	}
	// Sub-min_fill dust remainder: drop to a full fill.
	if rem, r := crossReRestRemainder(100, 95, 10); r || rem != 5 {
		t.Fatalf("dust remainder must not re-rest; got %d %v", rem, r)
	}
	// No min_fill floor (0): any non-empty remainder re-rests.
	if rem, r := crossReRestRemainder(100, 99, 0); !r || rem != 1 {
		t.Fatalf("want re-rest of 1 with no floor, got %d %v", rem, r)
	}
}

// TestBuildCrossOfferMinFill: a cross offer advertises min_fill = the dust+fee
// floor slice for its price, in (0, base_amount], for both SELL and BUY, and the
// signed offer still verifies.
func TestBuildCrossOfferMinFill(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	want := client.MinFillBase(25_000, 5_000_000, 1000)

	sell := buildCrossOffer("gold", "sell", 5_000_000, 25_000, 1000, "", time.Hour, 0, "seqaddr", pubHex(key), "sell-oid")
	if sell.GetMinFill() != want {
		t.Fatalf("sell min_fill = %d, want %d", sell.GetMinFill(), want)
	}
	if sell.GetMinFill() == 0 || sell.GetMinFill() > sell.GetBaseAmount() {
		t.Fatalf("sell min_fill %d must be in (0, base=%d]", sell.GetMinFill(), sell.GetBaseAmount())
	}
	if err := offer.SignOffer(sell, key); err != nil {
		t.Fatal(err)
	}
	if err := offer.VerifyOffer(sell); err != nil {
		t.Fatalf("sell offer with min_fill fails verify: %v", err)
	}

	buy := buildCrossOffer("gold", "buy", 5_000_000, 25_000, 1000, "", time.Hour, 0, "seqaddr", pubHex(key), "buy-oid")
	if buy.GetMinFill() != want {
		t.Fatalf("buy min_fill = %d, want %d", buy.GetMinFill(), want)
	}
	if buy.GetMinFill() == 0 || buy.GetMinFill() > buy.GetBaseAmount() {
		t.Fatalf("buy min_fill %d must be in (0, base=%d]", buy.GetMinFill(), buy.GetBaseAmount())
	}

	// The smallest safe slice prices to a BTC leg at/above the floor on both the
	// ceil (SELL) and floor (BUY) pricing paths, so the advertised min_fill is
	// genuinely settleable.
	if leg := client.ProportionalBtc(25_000, want, 5_000_000); leg < client.MinSafeBtcLegSats(1000) {
		t.Fatalf("SELL BTC leg at min_fill = %d, below floor %d", leg, client.MinSafeBtcLegSats(1000))
	}
	if leg := client.ProportionalBtcFloor(25_000, want, 5_000_000); leg < client.MinSafeBtcLegSats(1000) {
		t.Fatalf("BUY BTC leg at min_fill = %d, below floor %d", leg, client.MinSafeBtcLegSats(1000))
	}
}
