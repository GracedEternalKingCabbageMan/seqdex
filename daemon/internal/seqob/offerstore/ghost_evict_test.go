package offerstore

import "testing"

// A DEPARTED MAKER'S OFFERS MUST NOT KEEP RESTING.
//
// RemoveByMaker deliberately spares cross-chain and Lightning offers: they are durable
// across a maker RECONNECT, and evicting on every transient WS blip emptied the cross
// book and vanished BTC from the DEX (TestCrossSurvivesMakerDisconnect pins that, and it
// stays true).
//
// But durable-across-a-reconnect is not the same as liftable-while-the-maker-is-GONE.
// Those offers kept resting, kept being served at top of book, and answered every taker
// with silence or a refusal until their TTL. A fleet that restarts its makers fills the
// book with them: a taker walks the whole book collecting "maker is not connected",
// "offer not found or not open" and "another lift is in flight" from offers that will
// never settle.
//
// RemoveInteractiveByMaker is the after-the-grace form, used only once the maker has
// failed to come back.
func TestRemoveInteractiveByMakerClearsWhatRemoveByMakerSpares(t *testing.T) {
	s := New(nil)
	k := key(t)
	cross := mkCrossOffer(t, k, "x1", 1799999999)
	same := mkOffer(t, k, "s1", 1799999999)
	if _, err := s.Submit(cross); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(same); err != nil {
		t.Fatal(err)
	}

	// The immediate path still spares the cross offer — unchanged behaviour on a blip.
	if n := s.RemoveByMaker(cross.GetMakerPubkey()); n != 1 {
		t.Fatalf("RemoveByMaker evicted %d; want 1 (same-chain only)", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: cross.GetMakerPubkey(), OfferID: "x1"}); !ok {
		t.Fatal("a transient drop must not empty the cross book")
	}

	// After the grace, the maker is gone and the cross offer goes too.
	if n := s.RemoveInteractiveByMaker(cross.GetMakerPubkey()); n != 1 {
		t.Fatalf("RemoveInteractiveByMaker evicted %d; want the 1 remaining cross offer", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: cross.GetMakerPubkey(), OfferID: "x1"}); ok {
		t.Fatal("a departed maker's cross offer must not keep resting: every taker that " +
			"lifts it waits for a maker that no longer exists")
	}
}

// A covenant offer is genuinely offline-liftable — the taker settles the fill spend
// itself — so it survives its maker's departure by design.
func TestCovenantOfferSurvivesADepartedMaker(t *testing.T) {
	s := New(nil)
	k := key(t)
	cov := mkCovOffer(t, k, "c1", "covtx", 0, 100)
	if _, err := s.Submit(cov); err != nil {
		t.Fatal(err)
	}
	if n := s.RemoveInteractiveByMaker(cov.GetMakerPubkey()); n != 0 {
		t.Fatalf("evicted %d covenant offer(s); a covenant needs no maker present", n)
	}
	if _, ok := s.Get(Key{MakerPubkey: cov.GetMakerPubkey(), OfferID: "c1"}); !ok {
		t.Fatal("a covenant offer must stay liftable with no maker connected")
	}
}
