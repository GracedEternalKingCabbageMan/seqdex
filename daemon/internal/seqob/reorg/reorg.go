// Package reorg implements the interactive-lift REORG-UNDO watcher (P3.9). It
// watches each interactive (same-chain co-sign) settlement tx and, when a
// Bitcoin-driven reorg orphans the tx's anchor (Principle 1), fires the session's
// re-open hook so the relay re-inserts the resting offer and retracts the phantom
// trade. It mirrors the covenant chain-watcher's tip-driven reconciliation but for
// the INTERACTIVE rail, whose fills the covenant watcher does not see.
//
// It adds NO confirmation wait anywhere (spec §5b): min_anchor_depth stays the
// maker's dial and the happy path settles as fast as the offer allows. This is book
// and trade-feed TRUTH-TELLING after a reorg, not protection — the relay simply
// stops lying once a fill has un-happened.
package reorg

import (
	"sync"
	"time"
)

// ChainProbe reports whether a settling tx is still known to the node. It is the
// small, total surface the reconciliation needs, so it can be driven by a fake in
// tests. Every call reads the LIVE tip; there is no cache.
// ReorgForgetDepth is the Sequentia confirmation depth past which a watched
// settlement is dropped: about 2.4 hours of 60-second slots, some fourteen Bitcoin
// blocks of anchoring, beyond any reorg the anchor-following design is built for.
const ReorgForgetDepth = 144

// confirmationsProber is the optional probe capability that lets the watcher expire
// deeply buried settlements.
type confirmationsProber interface {
	TxConfirmations(txid string) (int64, error)
}

type ChainProbe interface {
	// TxPresent reports whether txid is still known to the node at the current tip —
	// in the best chain OR the mempool. A tx whose anchoring block was orphaned by a
	// Bitcoin reorg and was not re-mined (dropped) returns present=false. A transient
	// node error returns (_, err) and is NEVER treated as an orphan (we do not re-open
	// an order on a node hiccup).
	TxPresent(txid string) (present bool, err error)
}

// Logger is the minimal logging surface (satisfied by *log.Logger).
type Logger interface {
	Printf(format string, v ...interface{})
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}

type watch struct {
	txid       string
	onOrphaned func()
	seen       bool // TxPresent returned true at least once (the tx was anchored/known)
}

// Watcher watches interactive settlement txs and fires each session's re-open hook
// on a reorg that orphans its tx. It satisfies session.ReorgWatcher structurally, so
// seqobd wires it as the router's ReorgWatcher.
type Watcher struct {
	probe ChainProbe
	log   Logger

	mu sync.Mutex
	w  map[string]*watch // sessionID -> watch
}

// New builds a Watcher over a chain probe.
func New(probe ChainProbe, log Logger) *Watcher {
	if log == nil {
		log = nopLogger{}
	}
	return &Watcher{probe: probe, log: log, w: make(map[string]*watch)}
}

// WatchSettlement registers a settling tx to watch (session.ReorgWatcher). A
// txid-less settlement (a pure-LN fill with no on-chain tx) is not watched.
func (w *Watcher) WatchSettlement(sessionID, txid string, onOrphaned func()) {
	if txid == "" || onOrphaned == nil {
		return
	}
	w.mu.Lock()
	w.w[sessionID] = &watch{txid: txid, onOrphaned: onOrphaned}
	w.mu.Unlock()
}

// Forget drops a watch (e.g. the settlement was finalized beyond reorg risk). Safe
// to call for an unknown session.
func (w *Watcher) Forget(sessionID string) {
	w.mu.Lock()
	delete(w.w, sessionID)
	w.mu.Unlock()
}

// Run reconciles every interval until stop closes. Errors from a pass are logged,
// not fatal (a transient node hiccup must not tear down the watcher).
func (w *Watcher) Run(interval time.Duration, stop <-chan struct{}) {
	w.ReconcileOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			w.ReconcileOnce()
		}
	}
}

// ReconcileOnce probes every watched tx once. A tx SEEN present then ABSENT was
// orphaned by a Bitcoin reorg: fire its onOrphaned hook (which re-opens the order
// and retracts the phantom trade) and forget it. A tx not yet seen present is still
// propagating and is left alone (we never re-open a not-yet-anchored tx). A
// transient probe error skips that tx (no fire). Returns the number of orphans fired.
func (w *Watcher) ReconcileOnce() int {
	// Snapshot the session ids so the probe I/O runs without the lock held.
	w.mu.Lock()
	ids := make([]string, 0, len(w.w))
	for id := range w.w {
		ids = append(ids, id)
	}
	w.mu.Unlock()

	fired := 0
	for _, id := range ids {
		w.mu.Lock()
		wt, ok := w.w[id]
		w.mu.Unlock()
		if !ok {
			continue
		}
		present, err := w.probe.TxPresent(wt.txid)
		if err != nil {
			w.log.Printf("reorg: probe %s (session %s): %v; skipping this pass", short(wt.txid), id, err)
			continue
		}
		if present {
			w.mu.Lock()
			wt.seen = true
			w.mu.Unlock()
			// A settlement buried past any Bitcoin reorg worth modelling no longer needs
			// watching; without this every settled lift cost one RPC per pass forever.
			if cp, ok := w.probe.(confirmationsProber); ok {
				if confs, err := cp.TxConfirmations(wt.txid); err == nil && confs >= ReorgForgetDepth {
					w.Forget(id)
				}
			}
			continue
		}
		if wt.seen {
			// Seen anchored/known, now gone: a Bitcoin reorg orphaned it (anchoring
			// supremacy). Un-happen the fill: re-open the order + retract the trade.
			w.log.Printf("reorg: session %s tx %s orphaned; re-opening order + retracting trade", id, short(wt.txid))
			w.Forget(id)
			wt.onOrphaned()
			fired++
		}
	}
	return fired
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
