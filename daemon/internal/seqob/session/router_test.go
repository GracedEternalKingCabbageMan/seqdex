package session

import (
	"testing"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// manualReorg is a ReorgWatcher whose orphan callback the test fires by hand.
type manualReorg struct{ fire func() }

func (m *manualReorg) WatchSettlement(_ string, _ string, onOrphaned func()) { m.fire = onOrphaned }

func newTestRouter(reopen func(*Session), reorg ReorgWatcher, now func() time.Time) *Router {
	return NewRouter(Options{
		Deadline: time.Minute,
		Reorg:    reorg,
		OnReopen: reopen,
		Now:      now,
	})
}

func TestStartLiftAndCourier(t *testing.T) {
	r := newTestRouter(nil, nil, nil)
	s, err := r.StartLift(OpenReq{
		OfferID:            "aaaa",
		MakerPubkey:        "02pub",
		TakeAmount:         50,
		TakerSessionPubkey: []byte{1, 2, 3},
		MakerSessionPubkey: []byte{4, 5, 6},
	})
	if err != nil {
		t.Fatalf("StartLift: %v", err)
	}
	if _, ok := r.Get(s.ID); !ok {
		t.Fatalf("session not registered")
	}

	// Taker -> Maker.
	if err := r.Send(s.ID, RoleTaker, []byte("req-cipher")); err != nil {
		t.Fatalf("send taker->maker: %v", err)
	}
	msg := <-s.Inbox(RoleMaker)
	if string(msg.GetCiphertext()) != "req-cipher" || msg.GetSessionId() != s.ID {
		t.Fatalf("maker inbox got %+v", msg)
	}

	// Maker -> Taker.
	if err := r.Send(s.ID, RoleMaker, []byte("accept-cipher")); err != nil {
		t.Fatalf("send maker->taker: %v", err)
	}
	msg = <-s.Inbox(RoleTaker)
	if string(msg.GetCiphertext()) != "accept-cipher" {
		t.Fatalf("taker inbox got %+v", msg)
	}
}

func TestNotifyMakerHookFires(t *testing.T) {
	r := newTestRouter(nil, nil, nil)
	notified := make(chan *Session, 1)
	r.SetNotifyMaker(func(s *Session) { notified <- s })
	s, err := r.StartLift(OpenReq{
		OfferID: "aaaa", MakerPubkey: "02pub", TakeAmount: 50,
		TakerSessionPubkey: []byte{7, 7, 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-notified:
		if got.ID != s.ID || got.OfferID != "aaaa" || got.TakeAmount != 50 {
			t.Fatalf("notify carried wrong session: %+v", got)
		}
		if string(got.TakerSessionPubkey) != string([]byte{7, 7, 7}) {
			t.Fatalf("notify missing taker session pubkey")
		}
	default:
		t.Fatalf("expected notifyMaker to fire on StartLift")
	}
}

func TestSendUnknownSession(t *testing.T) {
	r := newTestRouter(nil, nil, nil)
	if err := r.Send("nope", RoleTaker, []byte("x")); err == nil {
		t.Fatalf("expected error for unknown session")
	}
}

func TestDeadlineElapsedRejectsSend(t *testing.T) {
	now := time.Unix(1750000000, 0)
	r := newTestRouter(nil, nil, func() time.Time { return now })
	s, _ := r.StartLift(OpenReq{OfferID: "aaaa", MakerPubkey: "02pub"})
	now = now.Add(2 * time.Minute) // past the 1-minute deadline
	if err := r.Send(s.ID, RoleTaker, []byte("late")); err == nil {
		t.Fatalf("expected send after deadline to fail")
	}
}

func TestSweepExpiredReopens(t *testing.T) {
	now := time.Unix(1750000000, 0)
	reopened := make(chan *Session, 1)
	r := newTestRouter(func(s *Session) { reopened <- s }, nil, func() time.Time { return now })
	s, _ := r.StartLift(OpenReq{OfferID: "aaaa", MakerPubkey: "02pub"})
	now = now.Add(2 * time.Minute)
	if n := r.SweepExpired(); n != 1 {
		t.Fatalf("expected 1 swept, got %d", n)
	}
	select {
	case got := <-reopened:
		if got.ID != s.ID {
			t.Fatalf("reopened wrong session")
		}
	default:
		t.Fatalf("expected OnReopen to fire for expired session")
	}
	if _, ok := r.Get(s.ID); ok {
		t.Fatalf("expired session should be removed")
	}
}

// fireImmediately invokes onOrphaned synchronously, simulating an anchor orphan.
type fireImmediately struct{}

func (fireImmediately) WatchSettlement(_ string, _ string, onOrphaned func()) { onOrphaned() }

func TestReorgOrphanReopens(t *testing.T) {
	reopened := make(chan *Session, 1)
	r := newTestRouter(func(s *Session) { reopened <- s }, fireImmediately{}, nil)
	s, _ := r.StartLift(OpenReq{OfferID: "aaaa", MakerPubkey: "02pub"})
	if err := r.SetSettleTxid(s.ID, "deadbeef"); err != nil {
		t.Fatalf("SetSettleTxid: %v", err)
	}
	select {
	case got := <-reopened:
		if got.ID != s.ID {
			t.Fatalf("reopened wrong session")
		}
	default:
		t.Fatalf("expected anchor orphan to re-open the order")
	}
}

// TestArmReorgReopenAfterClose covers P3.9: a settlement caches the offer + fill and
// Close()s the session, yet a reorg firing LATER must still re-open from the captured
// session (the live-session map no longer holds it).
func TestArmReorgReopenAfterClose(t *testing.T) {
	rw := &manualReorg{}
	reopened := make(chan *Session, 1)
	r := NewRouter(Options{Deadline: time.Minute, Reorg: rw, OnReopen: func(s *Session) { reopened <- s }})
	s, _ := r.StartLift(OpenReq{OfferID: "o1", MakerPubkey: "pk"})

	o := &seqobv1.Offer{OfferId: "o1", BaseAmount: 100}
	if err := r.ArmReorgReopen(s.ID, "txid1", o, 40); err != nil {
		t.Fatalf("ArmReorgReopen: %v", err)
	}
	// Settlement closes the session (removed from the live map).
	r.Close(s.ID)
	if _, ok := r.Get(s.ID); ok {
		t.Fatal("session should be closed/removed")
	}
	// The reorg fires later: onReopen must still run with the cached offer + fill.
	if rw.fire == nil {
		t.Fatal("reorg watch was not armed")
	}
	rw.fire()
	select {
	case got := <-reopened:
		if got.SettledOffer() != o || got.SettledFill() != 40 || got.SettleTxid() != "txid1" {
			t.Fatalf("cached settlement not carried: offer=%v fill=%d txid=%q",
				got.SettledOffer(), got.SettledFill(), got.SettleTxid())
		}
	default:
		t.Fatal("onReopen not called on a reorg after Close")
	}
}

func TestCloseDoesNotReopen(t *testing.T) {
	reopened := make(chan *Session, 1)
	r := newTestRouter(func(s *Session) { reopened <- s }, nil, nil)
	s, _ := r.StartLift(OpenReq{OfferID: "aaaa", MakerPubkey: "02pub"})
	r.Close(s.ID)
	select {
	case <-reopened:
		t.Fatalf("normal Close must NOT re-open the order")
	default:
	}
	if _, ok := r.Get(s.ID); ok {
		t.Fatalf("closed session should be removed")
	}
}
