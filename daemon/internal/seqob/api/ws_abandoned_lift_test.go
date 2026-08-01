package api

import (
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

// AN ABANDONED LIFT MUST FREE THE OFFER, NOT PIN IT FOR HOURS.
//
// An offer has ONE lift slot, held for as long as its session is live, and a cross
// session's co-sign deadline is measured in HOURS because it spans real parent-chain
// confirmations. Nothing released that slot when the taker simply went away — a failed
// take, a closed tab, a page reload — so one abandoned attempt made the offer answer
// every subsequent taker with "offer has a lift in progress; retry when it frees" for
// the rest of that deadline.
//
// In a book being probed by real users that poisons offers faster than the makers
// re-quote them: the book looks full while becoming unusable, and each retry down the
// book burns the next offer the same way.
func TestAbandonedLiftFreesTheOfferSlot(t *testing.T) {
	orig := takerReattachGrace
	takerReattachGrace = 50 * time.Millisecond
	defer func() { takerReattachGrace = orig }()

	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa")

	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	readFrom(t, makerConn) // order_status

	// Taker 1 lifts, then vanishes without settling or aborting.
	tk1 := key(t)
	t1 := dialWS(t, ts)
	sendTo(t, t1, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk1.PubKey().SerializeCompressed(),
	}}})
	readFrom(t, makerConn) // lift_requested
	if la := readFrom(t, t1).GetLiftAccepted(); la == nil {
		t.Fatal("taker1 lift must be accepted")
	}
	t1.Close() // the taker is gone: no settle, no abort, just gone

	// Taker 2 must get the offer back once the re-attach grace elapses — WITHOUT the
	// maker settling anything, which is the whole point.
	tk2 := key(t)
	t2 := dialWS(t, ts)
	defer t2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sendTo(t, t2, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
			OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
			TakerSessionPubkey: tk2.PubKey().SerializeCompressed(),
		}}})
		f := readFrom(t, t2)
		if f.GetLiftAccepted() != nil {
			return // the slot was released
		}
		if e := f.GetError(); e != nil && e.GetCode() != 409 {
			t.Fatalf("unexpected error while waiting for the slot: %+v", e)
		}
		if time.Now().After(deadline) {
			t.Fatal("the offer stayed pinned by an abandoned lift; one dead take poisons it " +
				"for the whole co-sign deadline (hours on the cross rail)")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A taker that RECONNECTS must not be aborted out from under: P3.8 re-attach exists
// because a taker mid-swap with funds committed has to be able to come back.
func TestReattachedTakerIsNotAborted(t *testing.T) {
	orig := takerReattachGrace
	takerReattachGrace = 50 * time.Millisecond
	defer func() { takerReattachGrace = orig }()

	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "bbbb")

	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	readFrom(t, makerConn)

	tk := key(t)
	t1 := dialWS(t, ts)
	sendTo(t, t1, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "bbbb", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
	}}})
	readFrom(t, makerConn)
	sid := readFrom(t, t1).GetLiftAccepted().GetSessionId()
	t1.Close()

	// Come back on a fresh connection and re-attach before the grace elapses.
	t2 := dialWS(t, ts)
	defer t2.Close()
	sendTo(t, t2, &seqobv1.To{Msg: &seqobv1.To_SessionReattach{SessionReattach: &seqobv1.SessionReattach{
		SessionId: sid, Role: "taker", Sig: offer.SignReattach(sid, "taker", tk),
	}}})
	readFrom(t, t2)

	time.Sleep(300 * time.Millisecond) // well past the grace

	// Behavioural proof the session is still live: the offer's single lift slot is still
	// held, so another taker is refused. If the re-attached taker had been aborted, this
	// lift would be accepted — and a swap whose funds are already committed would have
	// been abandoned underneath its owner.
	other := key(t)
	t3 := dialWS(t, ts)
	defer t3.Close()
	sendTo(t, t3, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "bbbb", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: other.PubKey().SerializeCompressed(),
	}}})
	if e := readFrom(t, t3).GetError(); e == nil || e.GetCode() != 409 {
		t.Fatalf("a re-attached taker's session must survive the grace; got %+v", e)
	}
}
