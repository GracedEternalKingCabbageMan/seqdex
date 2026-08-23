package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

// A signed offer is public: the orderbook serves it verbatim. Replaying it over a
// WebSocket proves nothing about who holds the socket, so it must not register that
// socket as the maker's endpoint — otherwise any client could take over the maker's
// lift routing and, by disconnecting, have the relay evict the maker's book. Only a
// re-signed copy (which needs the key) registers.
func TestReplayOverWSDoesNotRegisterMaker(t *testing.T) {
	store := offerstore.New(nil)
	srv := New(store, validator.New(validator.DefaultConfig(), nil), session.NewRouter(session.Options{Deadline: time.Minute}), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa")
	if resp := postProto(t, ts.URL+"/v1/offers", o); resp.StatusCode != http.StatusOK {
		t.Fatalf("maker REST submit: status %d", resp.StatusCode)
	}
	k := offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: "aaaa"}

	attacker := dialWS(t, ts)
	sendTo(t, attacker, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if st := readFrom(t, attacker); st.GetOrderStatus() == nil {
		t.Fatalf("replay should be re-acked with the live status, got %+v", st)
	}
	if _, ok := srv.makerConns.get(o.GetMakerPubkey()); ok {
		t.Fatal("a connection that merely replayed the public offer became the maker's endpoint")
	}
	attacker.Close()
	time.Sleep(200 * time.Millisecond)
	if _, ok := store.Get(k); !ok {
		t.Fatal("the maker's offer was evicted when the replaying connection dropped")
	}

	// The real maker reconnects with a re-signed copy and is registered.
	fresh := proto.Clone(o).(*seqobv1.Offer)
	fresh.CreatedAtUnix++
	fresh.MakerSig = nil
	if err := offer.SignOffer(fresh, mk); err != nil {
		t.Fatal(err)
	}
	maker := dialWS(t, ts)
	defer maker.Close()
	sendTo(t, maker, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: fresh}})
	if st := readFrom(t, maker); st.GetOrderStatus() == nil {
		t.Fatalf("re-signed submit should be acked, got %+v", st)
	}
	makerConn, ok := srv.makerConns.get(o.GetMakerPubkey())
	if !ok {
		t.Fatal("the maker's own re-signed submit did not register its connection")
	}

	// A maker-signed variant from the SAME second (a captured earlier edit) edits the
	// book but does not move the registration: only a strictly newer copy does.
	variant := proto.Clone(fresh).(*seqobv1.Offer)
	variant.ExpiresAtUnix++
	variant.MakerSig = nil
	if err := offer.SignOffer(variant, mk); err != nil {
		t.Fatal(err)
	}
	attacker2 := dialWS(t, ts)
	defer attacker2.Close()
	sendTo(t, attacker2, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: variant}})
	if st := readFrom(t, attacker2); st.GetOrderStatus() == nil {
		t.Fatalf("same-second variant should still be acked, got %+v", st)
	}
	if cur, _ := srv.makerConns.get(o.GetMakerPubkey()); cur != makerConn {
		t.Fatal("a same-second variant moved the maker registration to another socket")
	}
}

// A lift opened over REST binds to no connection, so the abandoned-lift release that
// protects WebSocket lifts never fires for it. Without an attach deadline one POST per
// offer would hold every interactive offer's single lift slot until the session deadline.
func TestRESTLiftWithoutTakerAttachIsReleased(t *testing.T) {
	prev := restLiftAttachGrace
	restLiftAttachGrace = 50 * time.Millisecond
	t.Cleanup(func() { restLiftAttachGrace = prev })

	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa")
	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if st := readFrom(t, makerConn); st.GetOrderStatus() == nil {
		t.Fatalf("expected order_status, got %+v", st)
	}

	lift := func() int {
		resp := postProto(t, ts.URL+"/v1/lift", &seqobv1.StartLift{
			OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 10,
			TakerSessionPubkey: key(t).PubKey().SerializeCompressed(),
		})
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := lift(); code != http.StatusOK {
		t.Fatalf("first REST lift: status %d", code)
	}
	if lr := readFrom(t, makerConn); lr.GetLiftRequested() == nil {
		t.Fatalf("maker should see lift_requested, got %+v", lr)
	}
	if code := lift(); code == http.StatusOK {
		t.Fatal("second lift accepted while the first holds the slot")
	}
	// The unattached lift is released after the grace, and the maker is told so it
	// frees its own slot rather than waiting out the session deadline.
	if e := readFrom(t, makerConn); e.GetError() == nil {
		t.Fatalf("maker should be told the taker is gone, got %+v", e)
	}
	if code := lift(); code != http.StatusOK {
		t.Fatalf("lift after the unattached one was released: status %d", code)
	}
}
