package offerstore

import (
	"fmt"
	"sync"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

func mkOfferNS(t *testing.T, id string, confidential bool) *seqobv1.Offer {
	t.Helper()
	k := key(t)
	o := mkOffer(t, k, id, 1750003600)
	o.Confidential = confidential
	o.MakerSig = nil
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

// The transparent and blinded books of one pair are disjoint namespaces, so the
// market summary lists them separately and flags the pair with the namespace; a
// client selecting the blinded book by pair.confidential would otherwise never
// find it, because makers leave the flag on the offer and not on the pair struct.
func TestMarketsSplitNamespacesAndFilter(t *testing.T) {
	s := New(nil)
	plain := mkOfferNS(t, "p1", false)
	blind := mkOfferNS(t, "b1", true)
	for _, o := range []*seqobv1.Offer{plain, blind} {
		if _, err := s.Submit(o); err != nil {
			t.Fatal(err)
		}
	}
	ms := s.Markets()
	if len(ms) != 2 {
		t.Fatalf("want 2 markets (one per namespace), got %d: %+v", len(ms), ms)
	}
	seen := map[bool]uint64{}
	for _, m := range ms {
		seen[m.GetPair().GetConfidential()] = m.GetNOrders()
	}
	if seen[false] != 1 || seen[true] != 1 {
		t.Fatalf("namespace split wrong: %+v", seen)
	}

	only := s.MarketsFiltered(func(o *seqobv1.Offer) bool { return !o.GetConfidential() })
	if len(only) != 1 || only[0].GetPair().GetConfidential() {
		t.Fatalf("filter should leave the transparent market alone, got %+v", only)
	}
}

// A delta broadcast must never send on a channel Unsubscribe has closed; that
// panics the goroutine that broadcast (the expiry sweeper, the chain watcher).
func TestBroadcastRacesUnsubscribe(t *testing.T) {
	s := New(nil)
	k := key(t)
	const n = 300
	offers := make([]*seqobv1.Offer, n)
	cancels := make([]*seqobv1.OfferCancel, n)
	for i := range offers {
		o := mkOffer(t, k, fmt.Sprintf("o%03d", i), 1750003600)
		offers[i] = o
		c := &seqobv1.OfferCancel{OfferId: o.OfferId, Nonce: uint64(i + 1)}
		if err := offer.SignCancel(c, k); err != nil {
			t.Fatal(err)
		}
		cancels[i] = c
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			_, id, _ := s.Subscribe(nil, false)
			s.Unsubscribe(id)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range offers {
			_, _ = s.Submit(offers[i])
			_ = s.Cancel(cancels[i])
		}
	}()
	wg.Wait()
}

// Un-happening a fill must not resurrect an order its maker cancelled after posting.
func TestReopenRefusesCancelledOrder(t *testing.T) {
	s := New(nil)
	k := key(t)
	o := mkOffer(t, k, "aaaa", 1750003600)
	kk, err := s.Submit(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPartialFill(kk, 40, "txid", 0); err != nil {
		t.Fatal(err)
	}
	// The Go maker's cancel nonce is a unix-nanosecond clock, here one hour after the post.
	c := &seqobv1.OfferCancel{OfferId: "aaaa", Nonce: (o.GetCreatedAtUnix() + 3600) * 1e9}
	if err := offer.SignCancel(c, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(c); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen(o, 100); err == nil {
		t.Fatal("reorg-reopen resurrected an order the maker had cancelled")
	}
	if _, ok := s.Get(kk); ok {
		t.Fatal("cancelled order is back in the book")
	}

	// A cancel that predates the post (an earlier life of a re-used offer_id) does not block.
	o2 := mkOffer(t, k, "bbbb", 1750003600)
	if _, err := s.Submit(o2); err != nil {
		t.Fatal(err)
	}
	old := &seqobv1.OfferCancel{OfferId: "bbbb", Nonce: (o2.GetCreatedAtUnix() - 3600) * 1e9}
	if err := offer.SignCancel(old, k); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(old); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen(o2, 100); err != nil {
		t.Fatalf("a pre-post cancel must not block the reopen: %v", err)
	}
}
