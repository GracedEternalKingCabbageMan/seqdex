package offerstore

import "testing"

// Pubkey-scoped eviction cannot tell two maker processes sharing one identity key
// apart: while any sibling stayed connected, a dead sibling's offers ghosted at top of
// book and won price-time ties with prices no live process would honor (seen live on a
// requote overlap — the relay routed the lift to the survivor, which refused it with
// "btc leg X != required Y" after the taker had already funded). The per-connection
// remover takes exactly the KEYS a departed connection posted.
func TestRemoveInteractiveByKeysRemovesOnlyTheNamedOffers(t *testing.T) {
	s := New(nil)
	k := key(t)
	dead := mkCrossOffer(t, k, "dead1", 1799999999)
	live := mkCrossOffer(t, k, "live1", 1799999999)
	if _, err := s.Submit(dead); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(live); err != nil {
		t.Fatal(err)
	}

	// Same maker pubkey on both; only the dead connection's key is named.
	n := s.RemoveInteractiveByKeys([]Key{{MakerPubkey: dead.GetMakerPubkey(), OfferID: "dead1"}})
	if n != 1 {
		t.Fatalf("evicted %d; want exactly the dead connection's offer", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: dead.GetMakerPubkey(), OfferID: "dead1"}); ok {
		t.Fatal("the departed connection's offer must go: its process can never co-sign the lift")
	}
	if _, ok := s.Get(Key{MakerPubkey: live.GetMakerPubkey(), OfferID: "live1"}); !ok {
		t.Fatal("the live sibling's offer must keep resting: sharing a pubkey is not a crime")
	}
}

// A covenant offer is offline-liftable by design; even a named key must spare it.
func TestRemoveInteractiveByKeysSparesCovenants(t *testing.T) {
	s := New(nil)
	k := key(t)
	cov := mkCovOffer(t, k, "c1", "covtx", 0, 100)
	if _, err := s.Submit(cov); err != nil {
		t.Fatal(err)
	}
	if n := s.RemoveInteractiveByKeys([]Key{{MakerPubkey: cov.GetMakerPubkey(), OfferID: "c1"}}); n != 0 {
		t.Fatalf("evicted %d covenant offer(s); a covenant needs no maker present", n)
	}
}
