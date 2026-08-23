package main

import "testing"

func TestNormalizeAskAndBid(t *testing.T) {
	ask := mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45)
	n, why := Normalize(ask, "r", tBase, tQuote)
	if n == nil {
		t.Fatalf("ask did not normalize: %s", why)
	}
	if n.Side != SideAsk || n.Family != FamPureLN {
		t.Fatalf("got side=%v fam=%v", n.Side, n.Family)
	}
	if n.BaseSize != 100 || n.QuoteNum != 45 || n.BaseDen != 100 {
		t.Fatalf("price fields wrong: %+v", n)
	}
	if n.CostFor(50) != 23 { // ceil(50*45/100)=22.5 -> 23 (maker-favoured)
		t.Fatalf("CostFor(50)=%d want 23", n.CostFor(50))
	}

	bid := mkOffer(FamCross, SideBid, tBase, tQuote, 100, 50)
	nb, why := Normalize(bid, "r", tBase, tQuote)
	if nb == nil {
		t.Fatalf("bid did not normalize: %s", why)
	}
	if nb.Side != SideBid || nb.BaseSize != 100 || nb.QuoteNum != 50 {
		t.Fatalf("bid fields wrong: %+v", nb)
	}
	if nb.RevenueFor(33) != 16 { // floor(33*50/100)=16.5 -> 16 (maker-favoured)
		t.Fatalf("RevenueFor(33)=%d want 16", nb.RevenueFor(33))
	}
}

func TestNormalizeFlippedOrientation(t *testing.T) {
	// An offer posted on the REVERSED pair (its base is our quote): maker sells
	// 50 quote for 100 base = canonically a BID for 100 base at 0.5 quote/base.
	o := mkOffer(FamSameChain, SideAsk, tQuote, tBase, 50, 100, withMinFill(10))
	n, why := Normalize(o, "r", tBase, tQuote)
	if n == nil {
		t.Fatalf("flipped offer did not normalize: %s", why)
	}
	if !n.Flipped || n.Side != SideBid {
		t.Fatalf("want flipped bid, got flipped=%v side=%v", n.Flipped, n.Side)
	}
	if n.BaseSize != 100 || n.QuoteNum != 50 {
		t.Fatalf("flipped sizing wrong: %+v", n)
	}
	// min_fill was 10 of ITS base (= our quote): converted to canonical base
	// atoms with a conservative ceil: ceil(10*100/50)=20.
	if n.MinFill != 20 {
		t.Fatalf("flipped min_fill=%d want 20", n.MinFill)
	}
}

func TestNormalizeRejectsInconsistentTradeDir(t *testing.T) {
	o := mkOffer(FamPureLN, SideAsk, tBase, tQuote, 100, 45)
	o.TradeDir = 2 // BUY, but offer_asset says the maker gives base (a SELL)
	if n, why := Normalize(o, "r", tBase, tQuote); n != nil {
		t.Fatalf("inconsistent trade_dir normalized (%s)", why)
	}
}

func TestNormalizeRejectsBaseAmountMismatch(t *testing.T) {
	o := mkOffer(FamCross, SideAsk, tBase, tQuote, 100, 45)
	o.BaseAmount = 90 // partials would be priced off a different whole
	if n, _ := Normalize(o, "r", tBase, tQuote); n != nil {
		t.Fatal("base_amount mismatch normalized")
	}
}

func TestNormalizeWrongPair(t *testing.T) {
	o := mkOffer(FamCross, SideAsk, "cc33", tQuote, 100, 45)
	if n, why := Normalize(o, "r", tBase, tQuote); n != nil || why != "not this pair" {
		t.Fatalf("wrong-pair offer: n=%v why=%q", n, why)
	}
}

func TestFamilyClassificationAndSliceability(t *testing.T) {
	cases := []struct {
		fam       Family
		sliceable bool
	}{
		{FamSameChain, true},
		{FamCovenant, true},
		{FamCross, true},
		{FamSubmarine, false}, // xsublift/xsubbuy have no partial flag
		{FamPureLN, true},
		{FamSubAsset, true},
	}
	for _, c := range cases {
		n := norm(mkOffer(c.fam, SideAsk, tBase, tQuote, 100, 45), tBase, tQuote)
		if n.Family != c.fam {
			t.Fatalf("family %v classified as %v", c.fam, n.Family)
		}
		if n.Sliceable != c.sliceable {
			t.Fatalf("family %v sliceable=%v want %v", c.fam, n.Sliceable, c.sliceable)
		}
	}
}

func TestPriceGEBigNumbers(t *testing.T) {
	// Values that would overflow a uint64 cross-multiplication.
	a := &NormOrder{QuoteNum: 1 << 62, BaseDen: 3, Side: SideBid}
	b := &NormOrder{QuoteNum: (1 << 62) - 1, BaseDen: 3, Side: SideAsk}
	if !a.PriceGE(b) || b.PriceGE(a) {
		t.Fatal("big.Int price comparison wrong")
	}
}
