package api

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// A GHOST OFFER MUST REFUSE THE LIFT, NOT GO QUIET.
//
// A cross-chain or Lightning offer deliberately survives its maker's WS drop, because
// the settlement agent reconnects after a blip and evicting on every drop emptied the
// cross book (TestCrossSurvivesMakerDisconnect pins that).
//
// But surviving a reconnect is not the same as being liftable while the maker is GONE.
// Those rails need the maker live to send terms. A lift against a departed maker used
// to be ACCEPTED and then simply go silent: the taker waited out its terms timeout with
// no error to act on, so the trade sat in flight while blocks went by and there was
// nothing for the retry-down-the-book path to trigger on.
//
// An instant, explicit refusal is retryable. Silence is not.
func TestLiftAgainstDepartedMakerIsRefusedImmediately(t *testing.T) {
	if !liftGuardExempts(nil) {
		t.Fatal("an offer we cannot find must not be blocked by the guard (openLift reports it)")
	}
	if !liftGuardExempts(&seqobv1.Offer{
		Settlement: &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{}},
	}) {
		t.Fatal("a covenant offer is offline-liftable and must stay liftable with no maker present")
	}
	for name, o := range map[string]*seqobv1.Offer{
		"cross": {Settlement: &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{}}},
		"ln":    {Settlement: &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{}}},
		"same":  {Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{}}},
	} {
		if liftGuardExempts(o) {
			t.Fatalf("%s settlement needs a live maker; a lift with none present must be refused", name)
		}
	}
}
