package offerstore

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

func mkOffer(t *testing.T, k *btcec.PrivateKey, id string, expires uint64) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		AllowPartial:  true,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: expires,
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

// TestSnapshotPairNamespaceDisjoint asserts the transparent and confidential books
// are disjoint views over the same pair: SnapshotPair(false) returns only
// transparent offers, SnapshotPair(true) only confidential ones.
func TestSnapshotPairNamespaceDisjoint(t *testing.T) {
	s := New(nil)
	k := key(t)

	transparent := mkOffer(t, k, "t1", 1799999999)
	conf := mkOffer(t, k, "c1", 1799999999)
	conf.Confidential = true
	conf.MakerSig = nil
	if err := offer.SignOffer(conf, k); err != nil {
		t.Fatalf("sign confidential: %v", err)
	}
	if _, err := s.Submit(transparent); err != nil {
		t.Fatalf("submit transparent: %v", err)
	}
	if _, err := s.Submit(conf); err != nil {
		t.Fatalf("submit confidential: %v", err)
	}

	pair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}
	pub := s.SnapshotPair(pair, false)
	if len(pub) != 1 || pub[0].GetOfferId() != "t1" {
		t.Fatalf("transparent book must contain only t1, got %+v", pub)
	}
	blinded := s.SnapshotPair(pair, true)
	if len(blinded) != 1 || blinded[0].GetOfferId() != "c1" {
		t.Fatalf("confidential book must contain only c1, got %+v", blinded)
	}
}

func key(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSubmitVerifiesAndSnapshot(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := s.Submit(o); err == nil {
		t.Fatalf("expected duplicate submit to fail")
	}
	snap := s.SnapshotPair(&seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}, false)
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}

	// Tampered offer must be rejected on submit.
	bad := mkOffer(t, k, "bbbb", 1750003600)
	bad.WantAmount = 1 // invalidates the signature
	if _, err := s.Submit(bad); err == nil {
		t.Fatalf("expected tampered offer to be rejected")
	}
}

func TestSnapshotPlusDeltas(t *testing.T) {
	s := New(nil)
	k := key(t)
	pair := &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}

	// Pre-existing offer appears in the snapshot.
	o1 := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o1); err != nil {
		t.Fatal(err)
	}
	snap, id, ch := s.Subscribe(pair, false)
	defer s.Unsubscribe(id)
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}

	// A subsequent submit arrives as a created delta.
	o2 := mkOffer(t, k, "bbbb", 1750003600)
	if _, err := s.Submit(o2); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, ch)
	if ev.Type != EventCreated || ev.Offer.GetOfferId() != "bbbb" {
		t.Fatalf("expected created delta for bbbb, got %+v", ev)
	}

	// Cancel arrives as a removed delta.
	c := &seqobv1.OfferCancel{OfferId: "bbbb", Nonce: 1}
	if err := offer.SignCancel(c, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	ev = recv(t, ch)
	if ev.Type != EventRemoved || ev.Ref.GetOfferId() != "bbbb" {
		t.Fatalf("expected removed delta for bbbb, got %+v", ev)
	}
}

func TestCancelNonceReplay(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	c := &seqobv1.OfferCancel{OfferId: "aaaa", Nonce: 5}
	if err := offer.SignCancel(c, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Re-post the same offer_id, then replay the old cancel: must be rejected.
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("re-submit: %v", err)
	}
	if err := s.Cancel(c); err == nil {
		t.Fatalf("expected replayed cancel (stale nonce) to be rejected")
	}
	// A fresh higher-nonce cancel works.
	c2 := &seqobv1.OfferCancel{OfferId: "aaaa", Nonce: 6}
	if err := offer.SignCancel(c2, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c2); err != nil {
		t.Fatalf("fresh cancel: %v", err)
	}
}

func TestCancelWrongSigner(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	// Cancel signed by a different key references the maker's offer_id+pubkey:
	// the signature will not verify against that maker_pubkey.
	other := key(t)
	c := &seqobv1.OfferCancel{OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), Nonce: 1}
	if err := offer.SignCancel(c, other); err == nil {
		t.Fatalf("expected SignCancel to reject mismatched maker_pubkey")
	}
}

func TestExpirySweeper(t *testing.T) {
	now := time.Unix(1750000000, 0)
	s := New(func() time.Time { return now })
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750000100) // expires 100s after now
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	if n := s.SweepExpired(); n != 0 {
		t.Fatalf("nothing should expire yet, swept %d", n)
	}
	now = time.Unix(1750000200, 0) // advance past expiry
	if n := s.SweepExpired(); n != 1 {
		t.Fatalf("expected 1 expired, swept %d", n)
	}
	if len(s.SnapshotPair(o.GetPair(), false)) != 0 {
		t.Fatalf("expired offer still in book")
	}
}

func TestPartialFillThenFilled(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600) // base 100, min_anchor_depth 0
	kk, err := s.Submit(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPartialFill(kk, 60, "txid1", 0); err != nil {
		t.Fatalf("partial fill: %v", err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 40 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_PARTIAL {
		t.Fatalf("after partial: %+v ok=%v", e, ok)
	}
	// Fill the rest: 0-conf tolerant (min_anchor_depth=0) so it reaches FILLED.
	if err := s.ApplyPartialFill(kk, 40, "txid2", 0); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("filled offer should be removed from the book")
	}
}

func TestReopenRestoresOrder(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	kk, err := s.Submit(o)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a fill, then an anchor orphan that re-opens the order.
	if err := s.ApplyPartialFill(kk, 100, "txid", 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("expected offer removed after fill")
	}
	if err := s.Reopen(o, 100); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 100 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_OPEN {
		t.Fatalf("after reopen: %+v ok=%v", e, ok)
	}
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delta")
		return Event{}
	}
}

// mkCovOffer builds a signed covenant SELL offer resting at outpoint txid:vout.
func mkCovOffer(t *testing.T, k *btcec.PrivateKey, id, txid string, vout uint32, base uint64) *seqobv1.Offer {
	t.Helper()
	b32 := func(f byte) []byte {
		o := make([]byte, 32)
		for i := range o {
			o[i] = f
		}
		return o
	}
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    base,
		OfferAmount:   base,
		OfferAsset:    "gold",
		WantAmount:    base / 3,
		WantAsset:     "usdx",
		AllowPartial:  true,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: 1750003600,
		Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
			CovenantTxid: txid, CovenantVout: vout,
			AssetA: hexOf(b32(0xa1)), AssetB: hexOf(b32(0xb2)),
			RateNum: 1, RateDen: 3, MakerProg: b32(0xc3), MakerProgVer: 1,
			MinLot: 5, ExpiryLocktime: 500, MakerX: b32(0xd4), InternalKey: b32(0xee),
		}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

func hexOf(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = h[c>>4]
		out[i*2+1] = h[c&0xf]
	}
	return string(out)
}

func TestCovenantReconcile(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// SnapshotCovenants sees it.
	covs := s.SnapshotCovenants()
	if len(covs) != 1 || covs[0].Terms.GetCovenantTxid() != "aabb" {
		t.Fatalf("snapshot covenants: %+v", covs)
	}
	kk := Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}

	// Optimistic decrement then LIVE reconcile back to on-chain value re-opens it.
	if err := s.ApplyPartialFill(kk, 30, "", 0); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(kk); e.ActiveAmount != 60 {
		t.Fatalf("after optimistic fill active=%d want 60", e.ActiveAmount)
	}
	if err := s.ReconcileCovenantActive(kk, 90); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(kk); e.ActiveAmount != 90 || e.Status != seqobv1.OfferStatus_OFFER_STATUS_OPEN {
		t.Fatalf("live reconcile: active=%d status=%v", e.ActiveAmount, e.Status)
	}

	// Hold for an unconfirmed spend: active drops to 0 (hidden from matcher).
	if err := s.HoldCovenantForSpend(kk, "spendtx"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(kk); e.ActiveAmount != 0 || e.SettleTxid != "spendtx" {
		t.Fatalf("hold: active=%d settle=%s", e.ActiveAmount, e.SettleTxid)
	}

	// Re-rest the remainder at a NEW outpoint with reduced size.
	if err := s.RerestCovenantRemainder(kk, "ccdd", 1, 60); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get(kk)
	if !ok || e.ActiveAmount != 60 || e.Offer.GetCovenant().GetCovenantTxid() != "ccdd" || e.Offer.GetCovenant().GetCovenantVout() != 1 {
		t.Fatalf("re-rest: %+v", e)
	}
	// Original submitted offer pointer must be untouched (clone-on-write).
	if o.GetCovenant().GetCovenantTxid() != "aabb" {
		t.Fatalf("re-rest mutated the original offer pointer")
	}

	// Full fill removes it.
	if err := s.RemoveCovenantFilled(kk, "filltx"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(kk); ok {
		t.Fatalf("filled covenant should be removed")
	}
}

// The watcher's covenant record path records a DELTA off the order's LIVE size on each confirmed fill,
// so the recorded trades sum to the base amount with the correct per-fill sizes — the heart of the
// on-confirm redesign (matcher no longer records at match). Drives mempool-hold -> partial re-rest
// (records the consumed 30) -> mempool-hold -> full fill (records the remaining 60).
func TestCovenantReconcileRecordsDeltaTrades(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatalf("submit: %v", err)
	}
	kk := Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}
	pair := o.GetPair()

	// Partial fill of 30: the mempool hold captures the live size (90), then the confirmed re-rest to a
	// 60 remainder records exactly the consumed 30 (90 - 60), NOT 0 and NOT the full 90.
	if err := s.HoldCovenantForSpend(kk, "spend1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RerestCovenantRemainder(kk, "rem1", 1, 60); err != nil {
		t.Fatal(err)
	}
	if trades := s.TradesFor(pair, 0); len(trades) != 1 || trades[0].Size != 30 {
		t.Fatalf("after partial: want exactly 1 trade of 30, got %+v", trades)
	}

	// Full fill of the 60 remainder records exactly 60. Total recorded == base (90), no loss, no double.
	if err := s.HoldCovenantForSpend(kk, "spend2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveCovenantFilled(kk, "fill2"); err != nil {
		t.Fatal(err)
	}
	trades := s.TradesFor(pair, 0)
	if len(trades) != 2 {
		t.Fatalf("after full fill: want 2 recorded trades, got %d", len(trades))
	}
	var sum uint64
	sizes := map[uint64]bool{}
	for _, tr := range trades {
		sum += tr.Size
		sizes[tr.Size] = true
	}
	if sum != 90 || !sizes[30] || !sizes[60] {
		t.Fatalf("recorded trades must be {30, 60} summing to base 90, got %+v", trades)
	}
}

func TestCovenantSurvivesMakerDisconnect(t *testing.T) {
	// RemoveByMaker must NOT evict a covenant (the watcher owns covenant removal).
	s := New(nil)
	k := key(t)
	o := mkCovOffer(t, k, "cov1", "aabb", 0, 90)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	n := s.RemoveByMaker(o.GetMakerPubkey())
	if n != 0 {
		t.Fatalf("RemoveByMaker evicted %d covenant offers; must be 0", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "cov1"}); !ok {
		t.Fatalf("covenant offer must survive maker disconnect")
	}
}

func mkCrossOffer(t *testing.T, k *btcec.PrivateKey, id string, expires uint64) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "BTC"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "BTC",
		AllowPartial:  true,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: expires,
		Settlement:    &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

func TestCrossSurvivesMakerDisconnect(t *testing.T) {
	// A BTC<->asset cross offer is serviced by the maker's always-online settlement agent, which
	// reconnects after a WS blip. RemoveByMaker must NOT evict it (evicting it on every transient drop
	// emptied the cross book and vanished BTC from the DEX). Only interactive same-chain co-sign offers
	// — which genuinely need the maker online to lift — are evicted.
	s := New(nil)
	k := key(t)
	cross := mkCrossOffer(t, k, "x1", 1799999999)
	same := mkOffer(t, k, "s1", 1799999999) // interactive same-chain: MUST still be evicted
	if _, err := s.Submit(cross); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(same); err != nil {
		t.Fatal(err)
	}
	if n := s.RemoveByMaker(cross.GetMakerPubkey()); n != 1 {
		t.Fatalf("RemoveByMaker evicted %d offers; want 1 (the same-chain one only)", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: cross.GetMakerPubkey(), OfferID: "x1"}); !ok {
		t.Fatalf("cross offer must survive maker disconnect")
	}
	if _, ok := s.Get(Key{MakerPubkey: same.GetMakerPubkey(), OfferID: "s1"}); ok {
		t.Fatalf("interactive same-chain offer must be evicted on disconnect")
	}
}

func TestSubmitDuplicateReturnsErrExists(t *testing.T) {
	// A reconnecting durable-offer agent re-posts the same (maker, offer_id) key; Submit must return
	// ErrExists so the WS layer refreshes it via Edit + re-register instead of 409ing the maker off.
	s := New(nil)
	k := key(t)
	o := mkCrossOffer(t, k, "x1", 1799999999)
	if _, err := s.Submit(o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(o); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Submit err = %v; want ErrExists", err)
	}
}
