package matcher

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

const (
	assetA = "aaaa" // base
	assetB = "bbbb" // quote
)

func newKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sell = maker sells `base` of A wanting `quote` of B (ask = quote/base).
func sell(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, partial bool) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId: id, SchemaVersion: 1,
		Pair:       &seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB},
		TradeDir:   seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount: base, OfferAmount: base, OfferAsset: assetA,
		WantAmount: quote, WantAsset: assetB,
		AllowPartial: partial, CreatedAtUnix: 1750000000, ExpiresAtUnix: 1799999999,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// buy = maker buys `base` of A giving `quote` of B (bid = quote/base).
func buy(t *testing.T, k *btcec.PrivateKey, id string, base, quote uint64, partial bool) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId: id, SchemaVersion: 1,
		Pair:       &seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB},
		TradeDir:   seqobv1.TradeDir_TRADE_DIR_BUY,
		BaseAmount: base, WantAmount: base, WantAsset: assetA,
		OfferAmount: quote, OfferAsset: assetB,
		AllowPartial: partial, CreatedAtUnix: 1750000000, ExpiresAtUnix: 1799999999,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

func mustSubmit(t *testing.T, s *offerstore.Store, o *seqobv1.Offer) {
	t.Helper()
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit %s: %v", o.GetOfferId(), err)
	}
}

func TestCrossFullFill(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting SELL: 100 A for 45 B (ask 0.45).
	mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true))
	// Incoming BUY: 100 A for 50 B (bid 0.50 >= 0.45) -> crosses.
	in := buy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if got[0].FillBase != 100 {
		t.Fatalf("fill base = %d, want 100", got[0].FillBase)
	}
	// Quote owed = ceil(100 * 45/100) = 45 (maker's ask, not the taker's bid).
	if got[0].FillQuote != 45 {
		t.Fatalf("fill quote = %d, want 45", got[0].FillQuote)
	}
	// Both fully filled -> removed from the book.
	if _, ok := s.Get(offerstore.Key{MakerPubkey: in.GetMakerPubkey(), OfferID: "b1"}); ok {
		t.Fatal("incoming should be fully filled and removed")
	}
	if activeOf(s, "s1") != 0 {
		t.Fatal("resting SELL should be fully filled and removed")
	}
	_ = mk
}

func TestNonCrossingLeavesResting(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting SELL asking 0.60 (100 A for 60 B).
	mustSubmit(t, s, sell(t, mk, "s1", 100, 60, true))
	// Incoming BUY bidding only 0.50 -> does NOT cross.
	in := buy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)

	if got := m.Cross(in); len(got) != 0 {
		t.Fatalf("expected no match, got %d", len(got))
	}
	// Both remain resting at full size.
	e, ok := s.Get(offerstore.Key{MakerPubkey: in.GetMakerPubkey(), OfferID: "b1"})
	if !ok || e.ActiveAmount != 100 {
		t.Fatalf("incoming should rest at 100, got ok=%v amt=%v", ok, e)
	}
}

func TestPartialFillReRests(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting SELL of 100 A (ask 0.45), partial allowed.
	mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true))
	// Incoming BUY for only 30 A at bid 0.50.
	in := buy(t, tk, "b1", 30, 15, true)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 || got[0].FillBase != 30 {
		t.Fatalf("want 1 match of 30, got %+v", got)
	}
	// Resting SELL remainder re-rests at 70; incoming fully filled.
	if rem := activeOf(s, "s1"); rem != 70 {
		t.Fatalf("resting remainder = %d, want 70", rem)
	}
	if activeOf(s, "b1") != 0 {
		t.Fatalf("incoming should be fully filled")
	}
	_ = mk
}

func TestPriceTimePriority(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	a, b, c, tk := newKey(t), newKey(t), newKey(t), newKey(t)
	// Three resting SELLs at asks 0.50, 0.45, 0.45 (b older than c).
	s5 := sell(t, a, "ask50", 100, 50, true)
	s45old := sell(t, b, "ask45old", 100, 45, true)
	s45new := sell(t, c, "ask45new", 100, 45, true)
	s45old.CreatedAtUnix = 1750000000
	s45new.CreatedAtUnix = 1750000500
	// Re-sign after touching created_at (it is inside the signed bytes).
	_ = offer.SignOffer(s5, a)
	_ = offer.SignOffer(s45old, b)
	_ = offer.SignOffer(s45new, c)
	mustSubmit(t, s, s5)
	mustSubmit(t, s, s45old)
	mustSubmit(t, s, s45new)

	// Incoming BUY of 150 A bidding 0.50: fills best price first (both 0.45s),
	// oldest 0.45 before newer, then the 0.50.
	in := buy(t, tk, "big", 150, 75, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 2 {
		t.Fatalf("want 2 matches, got %d", len(got))
	}
	if got[0].RestingKey.OfferID != "ask45old" {
		t.Fatalf("first fill should be the oldest 0.45, got %s", got[0].RestingKey.OfferID)
	}
	if got[1].RestingKey.OfferID != "ask45new" || got[1].FillBase != 50 {
		t.Fatalf("second fill should be 50 of ask45new, got %s/%d", got[1].RestingKey.OfferID, got[1].FillBase)
	}
	if activeOf(s, "ask50") != 100 {
		t.Fatal("the 0.50 ask should be untouched")
	}
}

func TestAllOrNothingIncomingNoPartial(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Only 40 A resting; incoming wants 100 all-or-nothing.
	mustSubmit(t, s, sell(t, mk, "s1", 40, 18, true))
	in := buy(t, tk, "b1", 100, 50, false) // allow_partial = false
	mustSubmit(t, s, in)
	if got := m.Cross(in); len(got) != 0 {
		t.Fatalf("all-or-nothing incoming must not partially fill, got %d", len(got))
	}
	if activeOf(s, "s1") != 40 || activeOf(s, "b1") != 100 {
		t.Fatal("nothing should have been filled")
	}
}

func TestCovenantMatchCarriesTerms(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL: 90 A for 30 B (rate 1/3), min_lot 5.
	o := sell(t, mk, "cov1", 90, 30, true)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "deadbeef", CovenantVout: 0,
		AssetA: "00aa", AssetB: "00bb", RateNum: 1, RateDen: 3,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	if err := offer.SignOffer(o, mk); err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, s, o)

	in := buy(t, tk, "b1", 90, 40, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if got[0].RestingCovenant == nil {
		t.Fatal("covenant terms not carried on the match")
	}
	if got[0].RestingLocked != 90 {
		t.Fatalf("covenant locked = %d, want 90", got[0].RestingLocked)
	}
	// Quote owed uses the on-chain rate: ceil(90 * 1/3) = 30.
	if got[0].FillQuote != 30 {
		t.Fatalf("fill quote = %d, want 30", got[0].FillQuote)
	}
}

func TestBothCovenantMatch(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL: 30 A wanting 90 B (sells A at 3 B/A).
	rest := sell(t, mk, "covA", 30, 90, true)
	rest.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "aa", CovenantVout: 0,
		AssetA: "00aa", AssetB: "00bb", RateNum: 3, RateDen: 1,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	_ = offer.SignOffer(rest, mk)
	mustSubmit(t, s, rest)

	// Incoming covenant BUY: buys 30 A giving 90 B (its own covenant sells B).
	in := buy(t, tk, "covB", 30, 90, true)
	in.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "bb", CovenantVout: 1,
		AssetA: "00bb", AssetB: "00aa", RateNum: 1, RateDen: 3,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	_ = offer.SignOffer(in, tk)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if !got[0].BothCovenant() {
		t.Fatal("both-covenant case not detected (settler path)")
	}
	if got[0].RestingCovenant == nil || got[0].IncomingCovenant == nil {
		t.Fatal("both covenant terms must be carried for the settler")
	}
	if got[0].IncomingCovenant.GetCovenantTxid() != "bb" || got[0].IncomingCovenant.GetCovenantVout() != 1 {
		t.Fatal("incoming covenant outpoint not carried")
	}
}

func TestCovenantMinLotRemainderTrim(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Covenant of 90 A, min_lot 5. Incoming would take 88, leaving a 2-atom
	// remainder < min_lot; the matcher trims the fill to 85 so the remainder is 5.
	o := sell(t, mk, "cov1", 90, 30, true)
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		AssetA: "00aa", AssetB: "00bb", RateNum: 1, RateDen: 3, MinLot: 5, MakerProgVer: 1,
	}}
	_ = offer.SignOffer(o, mk)
	mustSubmit(t, s, o)
	in := buy(t, tk, "b1", 88, 40, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 1 || got[0].FillBase != 85 {
		t.Fatalf("want a trimmed fill of 85, got %+v", got)
	}
	if activeOf(s, "cov1") != 5 {
		t.Fatalf("covenant remainder = %d, want 5 (>= min_lot)", activeOf(s, "cov1"))
	}
}

func TestCrossRailBridgeMatch(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Resting covenant SELL of X (asset A) wanting Y (asset B), 30 X for 90 B.
	rest := sell(t, mk, "covA", 30, 90, true)
	rest.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid: "aa", CovenantVout: 0,
		AssetA: "00aa", AssetB: "00bb", RateNum: 3, RateDen: 1,
		MinLot: 5, MakerProgVer: 1, ExpiryLocktime: 400,
	}}
	_ = offer.SignOffer(rest, mk)
	mustSubmit(t, s, rest)

	// Incoming LIGHTNING BUY of 30 X giving 100 B (its owner chose the LN rail).
	in := buy(t, tk, "lnB", 30, 100, true)
	in.Settlement = &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{
		LnDirection: 1, Max_0ConfAmount: 50,
	}}
	_ = offer.SignOffer(in, tk)
	mustSubmit(t, s, in)

	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if !got[0].CrossRail() {
		t.Fatal("covenant vs Lightning cross not classified CrossRail (bridge path)")
	}
	if got[0].BothCovenant() {
		t.Fatal("a cross-rail match must NOT be BothCovenant")
	}
	terms, resting := got[0].BridgeCovenant()
	if terms == nil || !resting || terms.GetCovenantTxid() != "aa" {
		t.Fatalf("bridge covenant side wrong: terms=%v resting=%v", terms, resting)
	}
	ln := got[0].BridgeLightning()
	if ln == nil || ln.GetMax_0ConfAmount() != 50 {
		t.Fatalf("bridge Lightning side not carried: %v", ln)
	}
}

func TestSameRailNotCrossRail(t *testing.T) {
	s := offerstore.New(nil)
	m := New(s)
	mk, tk := newKey(t), newKey(t)
	// Two plain same-chain orders: a same-rail cross, never a bridge job.
	mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true))
	in := buy(t, tk, "b1", 100, 50, true)
	mustSubmit(t, s, in)
	got := m.Cross(in)
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	if got[0].CrossRail() {
		t.Fatal("a same-chain/same-chain cross must not be CrossRail")
	}
	if terms, _ := got[0].BridgeCovenant(); terms != nil || got[0].BridgeLightning() != nil {
		t.Fatal("same-rail match must expose no bridge sides")
	}
}

// confidential re-signs o in the blinded-book namespace (confidential=true). The
// namespace is part of the canonical signed bytes, so it must be set before signing.
func confidential(t *testing.T, k *btcec.PrivateKey, o *seqobv1.Offer) *seqobv1.Offer {
	t.Helper()
	o.Confidential = true
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestConfidentialNeverCrossesTransparent asserts the separate blinded book: a
// confidential offer must NEVER cross a transparent offer on the same pair, even
// when their prices cross. Both legs of a confidential swap must blind on-chain, so
// a confidential order can only fill against another confidential order.
func TestConfidentialNeverCrossesTransparent(t *testing.T) {
	// Case 1: a confidential incoming must not cross a transparent resting order.
	{
		s := offerstore.New(nil)
		m := New(s)
		mk, tk := newKey(t), newKey(t)
		mustSubmit(t, s, sell(t, mk, "s1", 100, 45, true)) // transparent SELL
		in := confidential(t, tk, buy(t, tk, "b1", 100, 50, true))
		mustSubmit(t, s, in)
		if got := m.Cross(in); len(got) != 0 {
			t.Fatalf("confidential incoming must not cross a transparent resting order, got %d matches", len(got))
		}
		if activeOf(s, "s1") != 100 {
			t.Fatal("transparent resting SELL must be untouched by a confidential incoming")
		}
	}

	// Case 2: a transparent incoming must not cross a resting confidential order.
	{
		s := offerstore.New(nil)
		m := New(s)
		mk, tk := newKey(t), newKey(t)
		mustSubmit(t, s, confidential(t, mk, sell(t, mk, "s2", 100, 45, true)))
		in := buy(t, tk, "b2", 100, 50, true)
		mustSubmit(t, s, in)
		if got := m.Cross(in); len(got) != 0 {
			t.Fatalf("transparent incoming must not cross a confidential resting order, got %d matches", len(got))
		}
	}

	// Case 3 (sanity): a confidential incoming DOES cross a confidential resting order.
	{
		s := offerstore.New(nil)
		m := New(s)
		mk, tk := newKey(t), newKey(t)
		mustSubmit(t, s, confidential(t, mk, sell(t, mk, "s3", 100, 45, true)))
		in := confidential(t, tk, buy(t, tk, "b3", 100, 50, true))
		mustSubmit(t, s, in)
		if got := m.Cross(in); len(got) != 1 {
			t.Fatalf("confidential-vs-confidential must cross, got %d matches", len(got))
		}
	}
}

// activeOf finds a resting order's active amount by scanning the pair.
func activeOf(s *offerstore.Store, offerID string) uint64 {
	for _, e := range s.SnapshotPairEntries(&seqobv1.AssetPair{BaseAsset: assetA, QuoteAsset: assetB}, false) {
		if e.Offer.GetOfferId() == offerID {
			return e.ActiveAmount
		}
	}
	return 0
}

var _ = time.Now
