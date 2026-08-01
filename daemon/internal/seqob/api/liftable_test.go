package api

import (
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A BOOK MUST ONLY ADVERTISE PRICES A TAKER CAN ACTUALLY TAKE.
//
// An interactive same-chain offer needs its maker ONLINE to co-sign the lift. Offers whose maker had
// gone away were being served at the TOP of book -- better-priced than the real ones behind them --
// so every market order walked straight into them and came back "maker is not connected; this offer
// cannot be lifted right now". On one live pair that was 40 unfillable offers in front of 2 real
// ones: the DEX looked deep and could not trade.
//
// RemoveInteractiveByMaker already evicts these, but only when a maker DISCONNECTS. An offer posted
// by something that never held a connection -- a seeding script, a maker that died before its socket
// registered -- was never evicted by anything, because no disconnect ever happened.
func TestNeedsLiveMaker(t *testing.T) {
	t.Run("a plain interactive same-chain offer needs its maker", func(t *testing.T) {
		if !needsLiveMaker(&seqobv1.Offer{}) {
			t.Fatal("nothing else can co-sign this lift")
		}
	})

	// The durable settlement types are deliberately kept, exactly as the eviction path keeps them.
	t.Run("a COVENANT offer does not — that is the whole point of the passive CLOB", func(t *testing.T) {
		if needsLiveMaker(&seqobv1.Offer{Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{}}}) {
			t.Fatal("a covenant fills permissionlessly with the maker offline")
		}
	})
	t.Run("a LIGHTNING/sub-asset offer does not — an always-online agent serves it", func(t *testing.T) {
		if needsLiveMaker(&seqobv1.Offer{Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{}}}) {
			t.Fatal("hiding these on a transient blip is what emptied a book once already")
		}
	})
	t.Run("a CROSS-CHAIN offer does not — same reason", func(t *testing.T) {
		if needsLiveMaker(&seqobv1.Offer{Settlement: &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{}}}) {
			t.Fatal("cross offers are durable across a maker reconnect")
		}
	})
}

func TestLiftableOffersHidesGhostsButNotYoungOrDurableOnes(t *testing.T) {
	s := &Server{makerConns: newConnRegistry()}
	now := time.Now().Unix()
	old := uint64(now - ghostGraceSecs - 10)
	fresh := uint64(now - 5)

	offers := []*seqobv1.Offer{
		{OfferId: "ghost", MakerPubkey: "dead", CreatedAtUnix: old},
		{OfferId: "young", MakerPubkey: "dead", CreatedAtUnix: fresh},
		{OfferId: "covenant", MakerPubkey: "dead", CreatedAtUnix: old, Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{}}},
		{OfferId: "cross", MakerPubkey: "dead", CreatedAtUnix: old, Settlement: &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{}}},
		{OfferId: "ln", MakerPubkey: "dead", CreatedAtUnix: old, Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{}}},
	}
	got := map[string]bool{}
	for _, o := range s.liftableOffers(offers) {
		got[o.GetOfferId()] = true
	}

	if got["ghost"] {
		t.Error("an old interactive offer with no connected maker is unfillable and must not be quoted")
	}
	// Posting an offer and then connecting to serve it is a legitimate order, so a young one stays.
	if !got["young"] {
		t.Error("a just-posted offer must survive the submit-then-connect gap")
	}
	for _, id := range []string{"covenant", "cross", "ln"} {
		if !got[id] {
			t.Errorf("%s offer was hidden; it does not need a live maker to fill", id)
		}
	}
}

func TestLiftableOffersKeepsEverythingWhenTheRegistryIsAbsent(t *testing.T) {
	// Fail OPEN: a server built without a registry (tests, embedded uses) must not silently serve an
	// empty book -- that would look exactly like "no liquidity" while the store is full.
	s := &Server{}
	offers := []*seqobv1.Offer{{OfferId: "a"}, {OfferId: "b"}}
	if n := len(s.liftableOffers(offers)); n != 2 {
		t.Fatalf("got %d offers, want 2 — an absent registry must not empty the book", n)
	}
}
