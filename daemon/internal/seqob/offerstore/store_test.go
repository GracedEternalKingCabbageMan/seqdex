package offerstore

import (
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
	snap := s.SnapshotPair(&seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"})
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
	snap, id, ch := s.Subscribe(pair)
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
	if len(s.SnapshotPair(o.GetPair())) != 0 {
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
