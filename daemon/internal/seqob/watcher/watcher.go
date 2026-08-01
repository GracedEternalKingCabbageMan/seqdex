package watcher

import (
	"encoding/hex"
	"fmt"
	"time"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// ChainView is the read-only node surface the watcher needs. It is deliberately
// small and total so the reconciliation loop can be driven by a fake in tests.
// Every method reads the LIVE tip; there is no cache.
type ChainView interface {
	// Tip returns the current best block hash and height. Used for reorg
	// detection and to prove each pass reads the live tip.
	Tip() (hash string, height int64, err error)
	// Inspect reports the on-chain reality of a covenant outpoint at the current
	// tip. orderSPKHex is the covenant program scriptPubKey and assetADisplay the
	// display (reversed) asset-A id; both let Inspect recognise a re-created
	// remainder covenant output (asset == asset_a, spk == the program, at index
	// 2k+1 for the covenant input's index k in the spender).
	Inspect(txid string, vout uint32, orderSPKHex, assetADisplay string) (CovState, error)
}

// CovEntry is one resting covenant offer the watcher must reconcile.
type CovEntry struct {
	Key      offerstore.Key
	Terms    *seqobv1.CovenantTerms
	Active   uint64 // book's current active size (advisory; reconciled to chain)
	ReRested bool   // F3: current outpoint is a remainder from a prior partial fill, not the original funding
}

// BookController is the covenant-aware subset of the relay's book the watcher
// drives. offerstore.Store implements it (via the storeBook adapter). These are
// distinct from RemoveByMaker (which deliberately never evicts covenant offers on
// a maker disconnect); the watcher removes covenants only on CHAIN evidence.
type BookController interface {
	// SnapshotCovenants returns every resting covenant offer to reconcile.
	SnapshotCovenants() []CovEntry
	// ReconcileLive sets a resting covenant order's active size to its on-chain
	// UTXO value (idempotent; also re-opens a fill that never confirmed).
	ReconcileLive(k offerstore.Key, active uint64) error
	// RerestRemainder points a covenant order at the remainder outpoint a partial
	// fill re-created and sets the reduced active size, keeping it fillable.
	RerestRemainder(k offerstore.Key, txid string, vout uint32, size uint64) error
	// RemoveFilled removes a fully-filled covenant order (records the settling txid).
	RemoveFilled(k offerstore.Key, spenderTxid string) error
	// HoldForSpend hides a covenant order from matching while its spend is
	// unconfirmed (fill in mempool). A later pass re-opens it if the spend drops
	// or removes/re-rests it once the spend confirms.
	HoldForSpend(k offerstore.Key, spenderTxid string) error
	// RemoveGhost removes a covenant order whose funding UTXO is gone at the tip
	// (reorg / never-confirmed): maps to OFFER_STATUS_UTXO_INVALIDATED.
	RemoveGhost(k offerstore.Key, reason string) error
	// HoldGhost hides (does NOT remove) a RE-RESTED covenant order whose remainder
	// outpoint is gone: a Bitcoin reorg may have undone the fill, so keep the entry
	// so a later pass can re-open it if the fill re-confirms (F3, reversible).
	HoldGhost(k offerstore.Key, reason string) error
}

// Logger is the minimal logging surface (satisfied by *log.Logger).
type Logger interface {
	Printf(format string, v ...interface{})
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}

// Watcher reconciles the relay's covenant book to the node's current tip.
type Watcher struct {
	chain ChainView
	book  BookController
	log   Logger

	lastTipHash   string
	lastTipHeight int64
}

// New builds a Watcher over a chain view and a book controller.
func New(chain ChainView, book BookController, log Logger) *Watcher {
	if log == nil {
		log = nopLogger{}
	}
	return &Watcher{chain: chain, book: book, log: log}
}

// Stats summarises one reconciliation pass.
type Stats struct {
	Scanned     int
	Live        int
	PartialFill int
	Filled      int
	Pending     int
	Ghost       int
	Errors      int
	ReorgSeen   bool
}

// ReconcileOnce runs one full reconciliation pass: read the live tip, snapshot
// the resting covenant offers, classify each against the current UTXO set, and
// drive the book. It is safe to call repeatedly on a ticker.
func (w *Watcher) ReconcileOnce() (Stats, error) {
	var st Stats

	tipHash, tipHeight, err := w.chain.Tip()
	if err != nil {
		return st, fmt.Errorf("read tip: %w", err)
	}
	// Reorg / rollback detection. Every pass reconciles against the live UTXO set
	// regardless (so a vanished funding UTXO is caught as GHOST anyway); tracking
	// the tip lets us log the event and know a re-scan is warranted. Anchoring
	// supremacy is enforced by the NODE (it reorgs when Bitcoin reorgs); we only
	// consume its resulting tip.
	if w.lastTipHash != "" && tipHash != w.lastTipHash {
		if tipHeight <= w.lastTipHeight {
			st.ReorgSeen = true
			w.log.Printf("watcher: reorg/rollback detected (was %s@%d, now %s@%d); re-scanning all covenant outpoints",
				short(w.lastTipHash), w.lastTipHeight, short(tipHash), tipHeight)
		}
	}
	w.lastTipHash, w.lastTipHeight = tipHash, tipHeight

	covs := w.book.SnapshotCovenants()
	st.Scanned = len(covs)
	for _, c := range covs {
		exp, err := expectFromTerms(c.Terms)
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s: derive expected program: %v", c.Key.MakerPubkey, c.Key.OfferID, err)
			continue
		}
		state, err := w.chain.Inspect(c.Terms.GetCovenantTxid(), c.Terms.GetCovenantVout(), exp.OrderSPKHex, exp.AssetADisplay)
		if err != nil {
			st.Errors++
			w.log.Printf("watcher: %s/%s: inspect %s:%d: %v", c.Key.MakerPubkey, c.Key.OfferID,
				c.Terms.GetCovenantTxid(), c.Terms.GetCovenantVout(), err)
			continue
		}
		v := Classify(state, exp)
		w.apply(c, v, &st)
	}
	return st, nil
}

// Run reconciles every interval until stop is closed. Errors from a pass are
// logged, not fatal (a transient node hiccup must not tear down the watcher).
func (w *Watcher) Run(interval time.Duration, stop <-chan struct{}) {
	// Reconcile once immediately so the book converges without waiting a tick.
	if _, err := w.ReconcileOnce(); err != nil {
		w.log.Printf("watcher: initial pass: %v", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, err := w.ReconcileOnce(); err != nil {
				w.log.Printf("watcher: pass: %v", err)
			}
		}
	}
}

func (w *Watcher) apply(c CovEntry, v Verdict, st *Stats) {
	k := c.Key
	var err error
	switch v.State {
	case StateLive:
		st.Live++
		if v.LiveSize != c.Active {
			w.log.Printf("watcher: %s/%s LIVE; reconciling active %d -> %d (on-chain)", k.MakerPubkey, k.OfferID, c.Active, v.LiveSize)
		}
		err = w.book.ReconcileLive(k, v.LiveSize)
	case StatePartialFill:
		st.PartialFill++
		w.log.Printf("watcher: %s/%s PARTIAL fill; re-resting remainder %s:%d size %d",
			k.MakerPubkey, k.OfferID, v.RemainderTxid, v.RemainderVout, v.RemainderSize)
		err = w.book.RerestRemainder(k, v.RemainderTxid, v.RemainderVout, v.RemainderSize)
	case StateFilled:
		st.Filled++
		w.log.Printf("watcher: %s/%s FILLED by %s; removing", k.MakerPubkey, k.OfferID, short(v.SpenderTxid))
		err = w.book.RemoveFilled(k, v.SpenderTxid)
	case StateUnconfirmedSpend:
		st.Pending++
		w.log.Printf("watcher: %s/%s spend %s unconfirmed; holding pending", k.MakerPubkey, k.OfferID, short(v.SpenderTxid))
		err = w.book.HoldForSpend(k, v.SpenderTxid)
	case StateGhost:
		st.Ghost++
		// F3 / anchoring supremacy: a Ghost is a funding UTXO gone at the tip. For an ORIGINAL funding
		// (never re-rested) that means it never really existed (never-confirmed / reorged-away before any
		// fill) — remove it. But for a RE-RESTED order the gone outpoint is a remainder from a prior
		// partial fill, and its disappearance most likely means a Bitcoin reorg UNDID that fill; the
		// earlier outpoint is (or will be) restored. Permanently invalidating it would violate Principle 1
		// (the trade the reorg erased should un-happen, not the order). So HOLD it (hidden, reversible):
		// if the reorg heals and the fill re-confirms, a later pass sees the remainder again and re-opens it.
		if c.ReRested {
			w.log.Printf("watcher: %s/%s GHOST of a re-rested remainder — %s; HOLDING (reorg may restore the fill), not invalidating", k.MakerPubkey, k.OfferID, v.Reason)
			err = w.book.HoldGhost(k, v.Reason)
		} else {
			w.log.Printf("watcher: %s/%s GHOST — %s; removing (anchoring supremacy: UTXO gone at tip)", k.MakerPubkey, k.OfferID, v.Reason)
			err = w.book.RemoveGhost(k, v.Reason)
		}
	}
	if err != nil {
		st.Errors++
		w.log.Printf("watcher: %s/%s apply %s: %v", k.MakerPubkey, k.OfferID, v.State, err)
	}
}

// expectFromTerms derives the covenant program (scriptPubKey + display asset id)
// from the advertised CovenantTerms using pkg/covenant — the golden-vector-proven
// module — rather than trusting the advertised spk.
func expectFromTerms(t *seqobv1.CovenantTerms) (Expect, error) {
	o, err := orderFromTerms(t)
	if err != nil {
		return Expect{}, err
	}
	tap, err := o.Derive()
	if err != nil {
		return Expect{}, fmt.Errorf("derive taptree: %w", err)
	}
	return Expect{
		OrderSPKHex:   hex.EncodeToString(tap.ScriptPubKey),
		AssetADisplay: reverseHex(t.GetAssetA()),
		MinLot:        t.GetMinLot(),
	}, nil
}

// orderFromTerms rebuilds a covenant.Order from CovenantTerms. Asset ids are
// INTERNAL-order hex (as on-chain introspection returns them); maker_prog,
// maker_x and internal_key are raw bytes. Mirrors settler.orderFromTerms.
func orderFromTerms(t *seqobv1.CovenantTerms) (covenant.Order, error) {
	var o covenant.Order
	a, err := hex32(t.GetAssetA())
	if err != nil {
		return o, fmt.Errorf("asset_a: %w", err)
	}
	b, err := hex32(t.GetAssetB())
	if err != nil {
		return o, fmt.Errorf("asset_b: %w", err)
	}
	if len(t.GetMakerProg()) != 32 {
		return o, fmt.Errorf("maker_prog must be 32 bytes")
	}
	if len(t.GetMakerX()) != 32 {
		return o, fmt.Errorf("maker_x must be 32 bytes")
	}
	var mx, ik [32]byte
	copy(mx[:], t.GetMakerX())
	if ikb := t.GetInternalKey(); len(ikb) == 32 {
		copy(ik[:], ikb)
	} else {
		ik = covenant.NUMS
	}
	return covenant.Order{
		AssetA:         a,
		AssetB:         b,
		RateNum:        t.GetRateNum(),
		RateDen:        t.GetRateDen(),
		MakerProg:      t.GetMakerProg(),
		MakerVer:       int(t.GetMakerProgVer()),
		MinLot:         t.GetMinLot(),
		ExpiryLocktime: int64(t.GetExpiryLocktime()),
		MakerX:         mx,
		InternalKey:    ik,
	}, nil
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("want 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// reverseHex reverses the byte order of a hex string (internal <-> display asset
// id). Returns "" on malformed input (caller compares, so a mismatch is safe).
func reverseHex(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return ""
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
