package reorg

import (
	"errors"
	"testing"
)

// fakeProbe is a controllable ChainProbe.
type fakeProbe struct {
	present map[string]bool
	err     error
}

func (f *fakeProbe) TxPresent(txid string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.present[txid], nil
}

// TestWatcherFiresOnOrphan: a tx SEEN present then ABSENT is an orphaned anchor; the
// watcher fires onOrphaned exactly once and forgets it.
func TestWatcherFiresOnOrphan(t *testing.T) {
	fp := &fakeProbe{present: map[string]bool{"tx1": true}}
	w := New(fp, nil)
	fired := 0
	w.WatchSettlement("sess1", "tx1", func() { fired++ })

	if n := w.ReconcileOnce(); n != 0 { // present -> seen, no fire
		t.Fatalf("first pass fired %d, want 0", n)
	}
	fp.present["tx1"] = false // a Bitcoin reorg orphans + drops the settling tx
	if n := w.ReconcileOnce(); n != 1 {
		t.Fatalf("orphan pass fired %d, want 1", n)
	}
	if fired != 1 {
		t.Fatalf("onOrphaned called %d times, want 1", fired)
	}
	if n := w.ReconcileOnce(); n != 0 { // forgotten after firing
		t.Fatalf("post-fire pass fired %d, want 0 (idempotent)", n)
	}
}

// TestWatcherNoFireIfNeverSeen: a tx that is not yet known (still propagating) must
// not fire — we never re-open a not-yet-anchored fill.
func TestWatcherNoFireIfNeverSeen(t *testing.T) {
	fp := &fakeProbe{present: map[string]bool{}}
	w := New(fp, nil)
	w.WatchSettlement("s", "txX", func() { t.Fatal("must not fire for a never-seen tx") })
	if n := w.ReconcileOnce(); n != 0 {
		t.Fatalf("fired %d, want 0", n)
	}
	fp.present["txX"] = true // it confirms later
	if n := w.ReconcileOnce(); n != 0 {
		t.Fatalf("fired %d after confirm, want 0", n)
	}
}

// TestWatcherErrorNeverFires: a transient probe error must not be treated as an
// orphan (never re-open on a node hiccup).
func TestWatcherErrorNeverFires(t *testing.T) {
	fp := &fakeProbe{present: map[string]bool{"t": true}}
	w := New(fp, nil)
	w.WatchSettlement("s", "t", func() { t.Fatal("must not fire on a probe error") })
	w.ReconcileOnce() // seen present
	fp.err = errors.New("node down")
	if n := w.ReconcileOnce(); n != 0 {
		t.Fatalf("fired %d on error, want 0", n)
	}
}

// TestWatcherIgnoresTxidlessAndForget: a txid-less settlement is not watched, and
// Forget cancels a watch before it can fire.
func TestWatcherIgnoresTxidlessAndForget(t *testing.T) {
	fp := &fakeProbe{present: map[string]bool{"tx": true}}
	w := New(fp, nil)
	w.WatchSettlement("noln", "", func() { t.Fatal("txid-less settlement must not be watched") })
	w.WatchSettlement("s", "tx", func() { t.Fatal("forgotten watch must not fire") })
	w.ReconcileOnce() // seen
	w.Forget("s")
	fp.present["tx"] = false
	if n := w.ReconcileOnce(); n != 0 {
		t.Fatalf("fired %d after Forget, want 0", n)
	}
}
