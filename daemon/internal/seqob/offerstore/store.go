// Package offerstore is a thread-safe, in-memory per-pair order book for SeqOB.
//
// Offers are keyed by (maker_pubkey, offer_id) — offer_id need only be unique per
// maker (review m2). The store mirrors SideSwap's book-distribution shape: a
// per-pair snapshot plus created/removed deltas pushed to subscribers, and an
// active_amount that decrements on partial fills (SideSwap OwnOrder.active_amount).
// The stale-offer state machine maps SideSwap's HistStatus onto our OfferStatus
// (OPEN/PARTIAL/FILLED/CANCELLED/EXPIRED/UTXO_INVALIDATED).
//
// The store holds only signed text; it never holds keys, funds, or a PSET.
package offerstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// Key identifies an offer within the book.
type Key struct {
	MakerPubkey string
	OfferID     string
}

// Entry is a resting offer plus its mutable lifecycle state.
type Entry struct {
	Offer        *seqobv1.Offer
	Status       seqobv1.OfferStatus
	ActiveAmount uint64 // remaining base atoms after partial fills
	SettleTxid   string
	AnchorConfs  uint32
	CreatedAt    time.Time
	heldFill     uint64 // B1: active size captured when a mempool spend zeroed it, so the confirmed fill's trade size isn't lost
	reRested     bool   // F3: this covenant order was re-rested to a remainder outpoint at least once (its funding is no longer the original), so a Ghost of the current outpoint may be a reorg-undone fill whose earlier outpoint is restored — hold it, don't invalidate.
}

// Trade is one recorded cross, captured at on-chain SETTLEMENT: the chain watcher records a
// covenant fill on confirmation, and the interactive-lift SettleAck records its fill. (The matcher
// no longer records at match.) Feeds last_price, the trades feed, and candles. Price is
// quote-per-base, the same convention as displayPrice / best_bid|ask.
type Trade struct {
	TS    time.Time
	Price float64 // quote per base (matches best_bid/best_ask units)
	Size  uint64  // base atoms filled
}

const maxTradesPerPair = 1000 // ring cap per market; oldest dropped

// tradeRec is one line of the durable JSONL trade log: the pairKey plus the trade, so
// last_price/trades/candles can be replayed after a relay restart (the book itself is ephemeral).
type tradeRec struct {
	PK    string    `json:"pk"`
	TS    time.Time `json:"ts"`
	Price float64   `json:"price"`
	Size  uint64    `json:"size"`
}

// EventType is the kind of book delta.
type EventType int

const (
	// EventCreated: a new (or edited) offer entered the book.
	EventCreated EventType = iota
	// EventRemoved: an offer left the book (cancel/expire/fill/invalidated).
	EventRemoved
	// EventStatus: an offer's status/active_amount changed in place.
	EventStatus
)

// Event is a single book delta delivered to subscribers.
type Event struct {
	Type   EventType
	Pair   *seqobv1.AssetPair
	Offer  *seqobv1.Offer       // set for EventCreated
	Ref    *seqobv1.OfferRef    // set for EventRemoved
	Status *seqobv1.OrderStatus // set for EventStatus
}

type subscriber struct {
	pair string // pairKey filter, or "" for all pairs
	ch   chan Event
}

// Store is the in-memory order book.
type Store struct {
	mu          sync.RWMutex
	entries     map[Key]*Entry
	pairs       map[string]map[Key]bool // pairKey -> set of keys
	subs        map[int]*subscriber
	nextSub     int
	cancelNonce map[Key]uint64 // highest cancel nonce seen per key (replay defense, survives removal)
	subBuf      int
	now         func() time.Time
	trades      map[string][]Trade // pairKey -> recent trades ring (newest last), lazy
	tradeLog    *os.File           // B1: append-only JSONL sink so trade history survives a relay restart (nil = disabled)
}

// RecordTrade appends one executed cross for the resting offer's pair at its execution
// price (displayPrice of the resting order). Called by the matcher when a fill is produced.
// Best-effort + non-blocking: bad inputs are ignored, never an error on the match path.
func (s *Store) RecordTrade(restingOffer *seqobv1.Offer, sizeBase uint64) {
	if restingOffer == nil || sizeBase == 0 {
		return
	}
	price, _ := displayPrice(restingOffer)
	pair := restingOffer.GetPair()
	if price <= 0 || pair == nil {
		return
	}
	pk := pairKey(pair)
	ts := s.now()
	s.mu.Lock()
	if s.trades == nil {
		s.trades = make(map[string][]Trade)
	}
	t := append(s.trades[pk], Trade{TS: ts, Price: price, Size: sizeBase})
	if len(t) > maxTradesPerPair {
		t = t[len(t)-maxTradesPerPair:]
	}
	s.trades[pk] = t
	logf := s.tradeLog
	s.mu.Unlock()
	// Durably append AFTER releasing the lock (B-4): a slow/networked disk must never serialize the whole
	// book behind the trade-log write on the match/settle hot path. os.File appends (O_APPEND) are atomic
	// and Write is goroutine-safe, so concurrent appends are fine. Best-effort — a log error never breaks.
	if logf != nil {
		if b, err := json.Marshal(tradeRec{PK: pk, TS: ts, Price: price, Size: sizeBase}); err == nil {
			_, _ = logf.Write(append(b, '\n'))
		}
	}
}

// EnableTradeLog makes the trade history durable: it replays an existing append-only JSONL log into
// the in-memory rings, then keeps appending new trades to it. The book itself is ephemeral, but
// last_price/trades/candles should survive a relay restart. Best-effort; call once at startup.
func (s *Store) EnableTradeLog(path string) error {
	if f, err := os.Open(path); err == nil {
		s.mu.Lock()
		if s.trades == nil {
			s.trades = make(map[string][]Trade)
		}
		// bufio.Reader (not Scanner): a single over-long/corrupt line must not silently END the replay
		// (Scanner returns false past its cap) — read line by line, skip a bad one, keep going (B-5).
		rd := bufio.NewReaderSize(f, 1<<20)
		for {
			line, rerr := rd.ReadBytes('\n')
			if len(line) > 0 {
				var r tradeRec
				if json.Unmarshal(line, &r) == nil && r.PK != "" && r.Price > 0 && r.Size != 0 {
					t := append(s.trades[r.PK], Trade{TS: r.TS, Price: r.Price, Size: r.Size})
					if len(t) > maxTradesPerPair {
						t = t[len(t)-maxTradesPerPair:]
					}
					s.trades[r.PK] = t
				}
			}
			if rerr != nil {
				break
			}
		}
		s.mu.Unlock()
		_ = f.Close()
		// Compact to just the retained tail (<= maxTradesPerPair per pair) so the log can't grow without
		// bound and each restart's replay stays O(retained), not O(all history ever) (B-5).
		s.compactTradeLog(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.tradeLog = f
	s.mu.Unlock()
	return nil
}

// compactTradeLog rewrites the log to exactly the retained in-memory trades (best-effort; startup only,
// before any concurrent RecordTrade writer is armed).
func (s *Store) compactTradeLog(path string) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	s.mu.RLock()
	for pk, ring := range s.trades {
		for _, tr := range ring {
			if b, mErr := json.Marshal(tradeRec{PK: pk, TS: tr.TS, Price: tr.Price, Size: tr.Size}); mErr == nil {
				_, _ = f.Write(append(b, '\n'))
			}
		}
	}
	s.mu.RUnlock()
	if f.Close() == nil {
		_ = os.Rename(tmp, path)
	} else {
		_ = os.Remove(tmp)
	}
}

// TradesFor returns the most recent trades for a pair (up to limit; 0 = all held), newest last.
func (s *Store) TradesFor(pair *seqobv1.AssetPair, limit int) []Trade {
	if pair == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.trades[pairKey(pair)]
	if limit > 0 && len(t) > limit {
		t = t[len(t)-limit:]
	}
	out := make([]Trade, len(t))
	copy(out, t)
	return out
}

// New returns an empty store. If now is nil, time.Now is used.
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		entries:     make(map[Key]*Entry),
		pairs:       make(map[string]map[Key]bool),
		subs:        make(map[int]*subscriber),
		cancelNonce: make(map[Key]uint64),
		subBuf:      256,
		now:         now,
	}
}

func pairKey(p *seqobv1.AssetPair) string {
	return p.GetBaseAsset() + "/" + p.GetQuoteAsset()
}

func keyOf(o *seqobv1.Offer) Key {
	return Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}
}

// ErrExists is returned by Submit when the (maker_pubkey, offer_id) key is already
// resting. Callers refreshing a still-resting DURABLE offer (a reconnecting cross /
// lightning settlement agent re-posts a re-signed copy) should Edit instead of erroring.
var ErrExists = errors.New("offer already resting")

// Submit verifies the offer's signature and inserts it as a new OPEN order.
// It is an error (ErrExists) to Submit a key that already exists (use Edit to re-sign).
func (s *Store) Submit(o *seqobv1.Offer) (Key, error) {
	if err := offer.VerifyOffer(o); err != nil {
		return Key{}, fmt.Errorf("offer signature: %w", err)
	}
	if o.GetPair() == nil {
		return Key{}, fmt.Errorf("offer missing pair")
	}
	k := keyOf(o)
	s.mu.Lock()
	if _, ok := s.entries[k]; ok {
		s.mu.Unlock()
		return Key{}, fmt.Errorf("offer %s/%s: %w", k.MakerPubkey, k.OfferID, ErrExists)
	}
	e := &Entry{
		Offer:        o,
		Status:       seqobv1.OfferStatus_OFFER_STATUS_OPEN,
		ActiveAmount: o.GetBaseAmount(),
		CreatedAt:    s.now(),
	}
	s.put(k, e)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventCreated, Pair: o.GetPair(), Offer: o})
	return k, nil
}

// Edit replaces an existing offer (re-signed, same offer_id) with new terms. The
// new offer's created_at_unix must not move backwards. active_amount resets to
// the new base_amount.
func (s *Store) Edit(o *seqobv1.Offer) error {
	if err := offer.VerifyOffer(o); err != nil {
		return fmt.Errorf("offer signature: %w", err)
	}
	k := keyOf(o)
	s.mu.Lock()
	old, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	if o.GetCreatedAtUnix() < old.Offer.GetCreatedAtUnix() {
		s.mu.Unlock()
		return fmt.Errorf("edit created_at_unix moved backwards")
	}
	// Remove from the old pair bucket in case the pair changed, then re-insert.
	s.remove(k)
	e := &Entry{
		Offer:        o,
		Status:       seqobv1.OfferStatus_OFFER_STATUS_OPEN,
		ActiveAmount: o.GetBaseAmount(),
		CreatedAt:    s.now(),
	}
	s.put(k, e)
	s.mu.Unlock()
	// Emit a removed+created pair so subscribers converge regardless of pair change.
	s.broadcast(Event{Type: EventRemoved, Pair: old.Offer.GetPair(), Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	s.broadcast(Event{Type: EventCreated, Pair: o.GetPair(), Offer: o})
	return nil
}

// Cancel verifies a signed cancel and removes the offer. The nonce must exceed
// the highest nonce previously seen for this key, defeating replay of an old
// cancel against a re-posted same offer_id.
func (s *Store) Cancel(c *seqobv1.OfferCancel) error {
	if err := offer.VerifyCancel(c); err != nil {
		return fmt.Errorf("cancel signature: %w", err)
	}
	k := Key{MakerPubkey: c.GetMakerPubkey(), OfferID: c.GetOfferId()}
	s.mu.Lock()
	if c.GetNonce() <= s.cancelNonce[k] {
		s.mu.Unlock()
		return fmt.Errorf("stale cancel nonce")
	}
	s.cancelNonce[k] = c.GetNonce()
	e, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	pair := e.Offer.GetPair()
	s.remove(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventRemoved, Pair: pair, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	return nil
}

// Get returns a copy-free snapshot of the entry for k.
func (s *Store) Get(k Key) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[k]
	return e, ok
}

// RestingMakerSig returns the maker_sig of the offer currently resting under
// (makerPubkey, offerID), or ok=false if none rests. It lets the validator tell
// a byte-identical replay (same signed content => same signature) from a genuine
// new/edited offer WITHOUT charging the per-maker_pubkey rate budget (the relay
// replay-griefing defense; see internal/seqob/validator). The maker_sig is a
// deterministic, collision-resistant function of the entire signed offer, so an
// equal signature means equal signed content.
func (s *Store) RestingMakerSig(makerPubkey, offerID string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[Key{MakerPubkey: makerPubkey, OfferID: offerID}]
	if !ok {
		return nil, false
	}
	return e.Offer.GetMakerSig(), true
}

// OrderStatusOf returns the live OrderStatus for a resting key, if present. Used
// to re-ack a no-op replay without re-inserting the offer.
func (s *Store) OrderStatusOf(k Key) (*seqobv1.OrderStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[k]
	if !ok {
		return nil, false
	}
	return e.orderStatus(k), true
}

// SnapshotPair returns all live offers for a pair in the requested book
// namespace: confidential=false is the transparent (unblinded) book, confidential=true
// is the separate blinded book. The two namespaces are DISJOINT views over the pair
// so a confidential offer never appears in the transparent book and vice-versa
// (Confidential-book requirement 1); the matcher additionally guarantees they never
// cross.
func (s *Store) SnapshotPair(p *seqobv1.AssetPair, confidential bool) []*seqobv1.Offer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.pairs[pairKey(p)]
	out := make([]*seqobv1.Offer, 0, len(set))
	for k := range set {
		if e, ok := s.entries[k]; ok && e.Offer.GetConfidential() == confidential {
			out = append(out, e.Offer)
		}
	}
	return out
}

func (s *Store) snapshotLocked(pk string) []*seqobv1.Offer {
	set := s.pairs[pk]
	out := make([]*seqobv1.Offer, 0, len(set))
	for k := range set {
		if e, ok := s.entries[k]; ok {
			out = append(out, e.Offer)
		}
	}
	return out
}

// SnapshotPairEntries returns value copies of every live entry for a pair in the
// requested book namespace (confidential true/false), for the matcher's
// price-time-priority walk. Copies (not the live *Entry) so the matcher reads a
// consistent snapshot; the enclosed *Offer is immutable once signed, so sharing its
// pointer is safe. Filtering by confidential here keeps the blinded and unblinded
// books as disjoint order sets.
func (s *Store) SnapshotPairEntries(p *seqobv1.AssetPair, confidential bool) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.pairs[pairKey(p)]
	out := make([]Entry, 0, len(set))
	for k := range set {
		if e, ok := s.entries[k]; ok && e.Offer.GetConfidential() == confidential {
			out = append(out, *e)
		}
	}
	return out
}

// SnapshotMaker returns all live offers posted by a given maker_pubkey.
func (s *Store) SnapshotMaker(makerPubkey string) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Entry, 0)
	for k, e := range s.entries {
		if k.MakerPubkey == makerPubkey {
			out = append(out, e)
		}
	}
	return out
}

// Markets summarizes every pair currently in the book.
func (s *Store) Markets() []*seqobv1.Market {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*seqobv1.Market, 0, len(s.pairs))
	for pk, set := range s.pairs {
		if len(set) == 0 {
			continue
		}
		var pair *seqobv1.AssetPair
		var bestBid, bestAsk float64
		var n uint64
		for k := range set {
			e, ok := s.entries[k]
			if !ok {
				continue
			}
			if e.ActiveAmount == 0 {
				continue // B-7: a held/zeroed entry isn't fillable — don't count it or advertise its price
			}
			if pair == nil {
				pair = e.Offer.GetPair()
			}
			n++
			price, isBid := displayPrice(e.Offer)
			if price <= 0 {
				continue
			}
			if isBid {
				if price > bestBid {
					bestBid = price
				}
			} else {
				if bestAsk == 0 || price < bestAsk {
					bestAsk = price
				}
			}
		}
		var last float64
		if t := s.trades[pk]; len(t) > 0 {
			last = t[len(t)-1].Price // newest recorded cross
		}
		if pair == nil || n == 0 {
			// Every entry in this pair is held/zeroed (ActiveAmount==0): there is no fillable
			// market to advertise. Skip rather than emit a Market with a nil Pair (blank ticker
			// + stale last price) that a UI renders as garbage.
			continue
		}
		out = append(out, &seqobv1.Market{
			Pair:      pair,
			BestBid:   bestBid,
			BestAsk:   bestAsk,
			LastPrice: last,
			NOrders:   n,
		})
	}
	return out
}

// displayPrice returns the quote-per-base price and whether the offer is a bid
// (maker buying base) versus an ask (maker selling base). The authoritative
// ratio is want/offer (integers); this float is for display/sort only.
func displayPrice(o *seqobv1.Offer) (float64, bool) {
	base := o.GetBaseAmount()
	if base == 0 {
		return 0, false
	}
	switch o.GetTradeDir() {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		// give base (offer_amount), want quote (want_amount): ask.
		if o.GetOfferAmount() == 0 {
			return 0, false
		}
		return float64(o.GetWantAmount()) / float64(o.GetOfferAmount()), false
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		// give quote (offer_amount), want base (want_amount): bid.
		if o.GetWantAmount() == 0 {
			return 0, true
		}
		return float64(o.GetOfferAmount()) / float64(o.GetWantAmount()), true
	default:
		return 0, false
	}
}

// ApplyPartialFill decrements an offer's active_amount by filledBase and records
// the settling txid. Status becomes PARTIAL while base remains, else FILLED.
//
// Anchor-aware finality (Principle 1 / review B3): the maker dial min_anchor_depth
// defaults to 0 (0-conf tolerant; no enforced floor — policy override). FILLED is
// surfaced honestly: at 0-conf it is reached immediately, but a later anchor
// orphan can Reopen the order. Callers raise anchorConfs as the settling tx gains
// Bitcoin-anchor confirmations.
func (s *Store) ApplyPartialFill(k Key, filledBase uint64, settleTxid string, anchorConfs uint32) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	if filledBase > e.ActiveAmount {
		filledBase = e.ActiveAmount
	}
	e.ActiveAmount -= filledBase
	e.SettleTxid = settleTxid
	e.AnchorConfs = anchorConfs
	pair := e.Offer.GetPair()
	if e.ActiveAmount == 0 && anchorConfs >= e.Offer.GetMinAnchorDepth() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_FILLED
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	st := e.orderStatus(k)
	filled := e.ActiveAmount == 0 && e.Status == seqobv1.OfferStatus_OFFER_STATUS_FILLED
	if filled {
		s.remove(k)
	}
	s.mu.Unlock()

	s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	if filled {
		s.broadcast(Event{Type: EventRemoved, Pair: pair, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	}
	return nil
}

// MarkInvalidated drops an offer because the maker could not co-sign (coins
// moved). Maps to SideSwap HistStatus UTXO_INVALIDATED; no on-chain cost.
func (s *Store) MarkInvalidated(k Key) error {
	return s.dropWithStatus(k, seqobv1.OfferStatus_OFFER_STATUS_UTXO_INVALIDATED)
}

func (s *Store) dropWithStatus(k Key, status seqobv1.OfferStatus) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("offer %s/%s not found", k.MakerPubkey, k.OfferID)
	}
	e.Status = status
	pair := e.Offer.GetPair()
	st := e.orderStatus(k)
	s.remove(k)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	s.broadcast(Event{Type: EventRemoved, Pair: pair, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	return nil
}

// Reopen restores a (possibly already-removed) offer to the book as OPEN/PARTIAL.
// Used by the session reorg hook: when a settling tx's Bitcoin anchor is later
// orphaned (Principle 1), the swap un-happens and the order must come back. Also
// used when a lift session aborts before settlement.
func (s *Store) Reopen(o *seqobv1.Offer, restoreActive uint64) error {
	if err := offer.VerifyOffer(o); err != nil {
		return fmt.Errorf("offer signature: %w", err)
	}
	k := keyOf(o)
	if restoreActive == 0 || restoreActive > o.GetBaseAmount() {
		restoreActive = o.GetBaseAmount()
	}
	status := seqobv1.OfferStatus_OFFER_STATUS_OPEN
	if restoreActive < o.GetBaseAmount() {
		status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	s.mu.Lock()
	e := &Entry{Offer: o, Status: status, ActiveAmount: restoreActive, CreatedAt: s.now()}
	s.put(k, e)
	s.mu.Unlock()
	s.broadcast(Event{Type: EventCreated, Pair: o.GetPair(), Offer: o})
	return nil
}

// SweepExpired removes offers whose expires_at_unix has passed, emitting EXPIRED
// status then removal. Returns the number swept.
func (s *Store) SweepExpired() int {
	now := uint64(s.now().Unix())
	s.mu.Lock()
	expired := make([]Key, 0)
	for k, e := range s.entries {
		exp := e.Offer.GetExpiresAtUnix()
		if exp != 0 && exp <= now {
			expired = append(expired, k)
		}
	}
	type ev struct {
		pair *seqobv1.AssetPair
		st   *seqobv1.OrderStatus
		ref  *seqobv1.OfferRef
	}
	evs := make([]ev, 0, len(expired))
	for _, k := range expired {
		e := s.entries[k]
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_EXPIRED
		evs = append(evs, ev{pair: e.Offer.GetPair(), st: e.orderStatus(k), ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
		s.remove(k)
	}
	s.mu.Unlock()
	for _, e := range evs {
		s.broadcast(Event{Type: EventStatus, Pair: e.pair, Status: e.st})
		s.broadcast(Event{Type: EventRemoved, Pair: e.pair, Ref: e.ref})
	}
	return len(expired)
}

// RemoveByMaker drops every resting INTERACTIVE SAME-CHAIN offer for makerPubkey,
// emitting a removal event per offer. Called when a maker's websocket disconnects:
// an interactive same-chain co-sign offer needs the maker ONLINE to co-sign a lift,
// so once its connection drops the offer is unliftable and must not linger as an
// un-fillable "ghost".
//
// DURABLE settlement types are the deliberate exception and are NOT evicted, because a
// lift does not need this maker's live co-sign at disconnect time:
//   - COVENANT offers: funded on-chain, fillable permissionlessly with the maker OFFLINE
//     (the whole point of the passive CLOB).
//   - CROSS-CHAIN (BTC<->asset) and LIGHTNING/sub-asset offers: serviced by the maker's
//     always-online settlement agent (seqob-maker), which reconnects and re-registers.
//     A transient WS blip must NOT evict these — that emptied the cross book (BTC vanished
//     from the DEX). Ghost-safety holds regardless: a taker's pre-lock terms handshake
//     fails fast and spends nothing if no agent answers, and the TTL sweep removes truly
//     abandoned offers. See durable-cross-offer design.
//
// Returns the number removed.
func (s *Store) RemoveByMaker(makerPubkey string) int {
	s.mu.Lock()
	victims := make([]Key, 0)
	for k, e := range s.entries {
		// Evict ONLY interactive same-chain co-sign offers (they need the maker online now). A same-chain
		// COVENANT offer carries a covenant and is permissionless, so it is spared alongside cross/lightning.
		if k.MakerPubkey == makerPubkey && e.Offer.GetSameChain() != nil && e.Offer.GetCovenant() == nil {
			victims = append(victims, k)
		}
	}
	type ev struct {
		pair *seqobv1.AssetPair
		ref  *seqobv1.OfferRef
	}
	evs := make([]ev, 0, len(victims))
	for _, k := range victims {
		e := s.entries[k]
		evs = append(evs, ev{pair: e.Offer.GetPair(), ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
		s.remove(k)
	}
	s.mu.Unlock()
	for _, e := range evs {
		s.broadcast(Event{Type: EventRemoved, Pair: e.pair, Ref: e.ref})
	}
	return len(victims)
}

// --- covenant chain-watcher reconciliation (internal/seqob/watcher) ----------
//
// These mutate a resting COVENANT order to match on-chain reality. They are the
// only book operations the chain watcher needs, and they are DISTINCT from
// RemoveByMaker (which deliberately never evicts a covenant on a maker
// disconnect): the watcher removes or resizes a covenant strictly on CHAIN
// evidence (spent / partially filled / funding gone at the tip). None of them
// touch interactive offers.

// CovenantEntry is a resting covenant offer's key + terms + active size, for the
// watcher to reconcile. The returned *CovenantTerms is the live pointer; the
// watcher only reads it.
type CovenantEntry struct {
	Key      Key
	Terms    *seqobv1.CovenantTerms
	Active   uint64
	ReRested bool // F3: the current outpoint is a remainder from a prior partial fill, not the original funding
}

// SnapshotCovenants returns every resting offer that settles via a covenant.
func (s *Store) SnapshotCovenants() []CovenantEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CovenantEntry, 0)
	for k, e := range s.entries {
		if cov := e.Offer.GetCovenant(); cov != nil {
			out = append(out, CovenantEntry{Key: k, Terms: cov, Active: e.ActiveAmount, ReRested: e.reRested})
		}
	}
	return out
}

// HoldCovenantGhost hides a re-rested covenant order whose current (remainder) outpoint is GONE at the
// tip with no spender — most likely a Bitcoin reorg undid the fill (anchoring supremacy). Unlike
// RemoveGhost (permanent MarkInvalidated), this is REVERSIBLE: active drops to 0 (hidden from matching,
// so no taker fills a vanished outpoint) but the entry is kept, so if the reorg heals and the fill
// re-confirms, a later reconcile pass sees the remainder again and re-opens it (F3). No-op for a
// non-covenant/missing entry.
func (s *Store) HoldCovenantGhost(k Key, reason string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || e.Offer.GetCovenant() == nil {
		s.mu.Unlock()
		return nil
	}
	changed := e.ActiveAmount != 0
	if e.ActiveAmount > 0 {
		e.heldFill = e.ActiveAmount
	}
	e.ActiveAmount = 0
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	pair := e.Offer.GetPair()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if changed {
		s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	}
	return nil
}

// ReconcileCovenantActive sets a resting covenant order's active size to its
// on-chain UTXO value. Idempotent. It also RE-OPENS an order the watcher had HELD
// for an unconfirmed spend (HoldCovenantForSpend / HoldCovenantGhost) that then
// dropped or was reorged away — the covenant still holds its value at the tip, so
// the order becomes fillable again. (The matcher no longer decrements at match, so
// there is no optimistic match-time decrement to correct.) No-op for a
// non-covenant or missing entry.
func (s *Store) ReconcileCovenantActive(k Key, active uint64) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || e.Offer.GetCovenant() == nil {
		s.mu.Unlock()
		return nil
	}
	if active > e.Offer.GetBaseAmount() {
		active = e.Offer.GetBaseAmount()
	}
	changed := e.ActiveAmount != active || e.SettleTxid != ""
	e.ActiveAmount = active
	e.SettleTxid = ""
	e.heldFill = 0 // B-8: clear any stale held-fill size so a later RemoveCovenantFilled uses the live active, not a stale hold
	if active >= e.Offer.GetBaseAmount() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_OPEN
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	pair := e.Offer.GetPair()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if changed {
		s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	}
	return nil
}

// RerestCovenantRemainder points a covenant order at the remainder outpoint a
// partial fill re-created (new txid:vout) and sets the reduced active size, so
// the rest of the order stays fillable. The covenant PROGRAM is unchanged (same
// scriptPubKey), so the maker's signed terms still describe it; only the
// chain-observable outpoint moves. The stored Offer is cloned before mutation so
// concurrent matcher reads of the old pointer stay consistent.
func (s *Store) RerestCovenantRemainder(k Key, txid string, vout uint32, size uint64) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || e.Offer.GetCovenant() == nil {
		s.mu.Unlock()
		return nil
	}
	// Clone + swap so the previously-shared *Offer pointer is never mutated.
	no := proto.Clone(e.Offer).(*seqobv1.Offer)
	no.GetCovenant().CovenantTxid = txid
	no.GetCovenant().CovenantVout = vout
	e.Offer = no
	e.reRested = true // F3: mark that this order's funding is now a remainder, not its original outpoint
	if size > no.GetBaseAmount() {
		size = no.GetBaseAmount()
	}
	// B1: consumed portion = size before this re-rest (active now, or the size held aside when a mempool
	// spend zeroed active) minus the re-rested remainder. That portion is a trade.
	prevActive := e.ActiveAmount
	if prevActive == 0 {
		prevActive = e.heldFill
	}
	var filled uint64
	if prevActive > size {
		filled = prevActive - size
	}
	e.heldFill = 0
	e.ActiveAmount = size
	e.SettleTxid = ""
	if size >= no.GetBaseAmount() {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_OPEN
	} else {
		e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	}
	pair := no.GetPair()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if filled > 0 {
		s.RecordTrade(no, filled) // re-locks internally; safe after Unlock
	}
	// Emit created (subscribers re-learn the new outpoint) then status.
	s.broadcast(Event{Type: EventCreated, Pair: pair, Offer: no})
	s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	return nil
}

// HoldCovenantForSpend hides a covenant order from matching while its spend is
// unconfirmed (fill in the mempool): active drops to 0 (the matcher skips
// zero-active entries) and the settling txid is recorded. A later reconcile
// pass restores it (ReconcileCovenantActive) if the spend drops, or removes /
// re-rests it once the spend confirms.
func (s *Store) HoldCovenantForSpend(k Key, spenderTxid string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || e.Offer.GetCovenant() == nil {
		s.mu.Unlock()
		return nil
	}
	changed := e.ActiveAmount != 0 || e.SettleTxid != spenderTxid
	if e.ActiveAmount > 0 {
		e.heldFill = e.ActiveAmount // B1: remember the size so the confirmed-fill trade isn't lost once active zeroes
	}
	e.ActiveAmount = 0
	e.SettleTxid = spenderTxid
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_PARTIAL
	pair := e.Offer.GetPair()
	st := e.orderStatus(k)
	s.mu.Unlock()
	if changed {
		s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	}
	return nil
}

// RemoveCovenantFilled removes a fully-filled covenant order, recording the
// settling txid and emitting FILLED. No-op for a non-covenant or missing entry.
func (s *Store) RemoveCovenantFilled(k Key, settleTxid string) error {
	s.mu.Lock()
	e, ok := s.entries[k]
	if !ok || e.Offer.GetCovenant() == nil {
		s.mu.Unlock()
		return nil
	}
	e.Status = seqobv1.OfferStatus_OFFER_STATUS_FILLED
	e.SettleTxid = settleTxid
	// B1: how much this settlement filled — active now, or the size held aside when a mempool spend
	// zeroed active earlier (HoldCovenantForSpend). A covenant lift IS a trade; record it below.
	filled := e.ActiveAmount
	if filled == 0 {
		filled = e.heldFill
	}
	filledOffer := e.Offer
	e.ActiveAmount = 0
	pair := e.Offer.GetPair()
	st := e.orderStatus(k)
	s.remove(k)
	s.mu.Unlock()
	s.RecordTrade(filledOffer, filled) // re-locks internally; safe after Unlock
	s.broadcast(Event{Type: EventStatus, Pair: pair, Status: st})
	s.broadcast(Event{Type: EventRemoved, Pair: pair, Ref: &seqobv1.OfferRef{OfferId: k.OfferID, MakerPubkey: k.MakerPubkey}})
	return nil
}

// RunExpirySweeper sweeps every interval until ctx-like stop channel closes.
func (s *Store) RunExpirySweeper(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.SweepExpired()
		}
	}
}

// Subscribe registers a delta subscriber. If pairFilter is non-nil, only deltas
// for that pair are delivered. It returns the current snapshot for the pair (or
// all pairs if pairFilter is nil), a subscription id, and the delta channel.
func (s *Store) Subscribe(pairFilter *seqobv1.AssetPair) ([]*seqobv1.Offer, int, <-chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	pk := ""
	if pairFilter != nil {
		pk = pairKey(pairFilter)
	}
	sub := &subscriber{pair: pk, ch: make(chan Event, s.subBuf)}
	s.subs[id] = sub

	var snap []*seqobv1.Offer
	if pk != "" {
		snap = s.snapshotLocked(pk)
	} else {
		snap = make([]*seqobv1.Offer, 0, len(s.entries))
		for _, e := range s.entries {
			snap = append(snap, e.Offer)
		}
	}
	return snap, id, sub.ch
}

// Unsubscribe removes and closes a subscription.
func (s *Store) Unsubscribe(id int) {
	s.mu.Lock()
	sub, ok := s.subs[id]
	if ok {
		delete(s.subs, id)
	}
	s.mu.Unlock()
	if ok {
		close(sub.ch)
	}
}

// --- locked helpers ---

func (s *Store) put(k Key, e *Entry) {
	s.entries[k] = e
	pk := pairKey(e.Offer.GetPair())
	set := s.pairs[pk]
	if set == nil {
		set = make(map[Key]bool)
		s.pairs[pk] = set
	}
	set[k] = true
}

func (s *Store) remove(k Key) {
	e, ok := s.entries[k]
	if !ok {
		return
	}
	pk := pairKey(e.Offer.GetPair())
	delete(s.entries, k)
	if set := s.pairs[pk]; set != nil {
		delete(set, k)
		if len(set) == 0 {
			delete(s.pairs, pk)
		}
	}
}

func (e *Entry) orderStatus(k Key) *seqobv1.OrderStatus {
	return &seqobv1.OrderStatus{
		OfferId:      k.OfferID,
		MakerPubkey:  k.MakerPubkey,
		Status:       e.Status,
		ActiveAmount: e.ActiveAmount,
		SettleTxid:   e.SettleTxid,
		AnchorConfs:  e.AnchorConfs,
	}
}

// broadcast delivers an event to all matching subscribers without blocking. If a
// subscriber's buffer is full the event is dropped for that subscriber (it can
// re-snapshot); the book is never blocked by a slow client.
func (s *Store) broadcast(ev Event) {
	pk := pairKey(ev.Pair)
	s.mu.RLock()
	subs := make([]*subscriber, 0, len(s.subs))
	for _, sub := range s.subs {
		if sub.pair == "" || sub.pair == pk {
			subs = append(subs, sub)
		}
	}
	s.mu.RUnlock()
	for _, sub := range subs {
		select {
		case sub.ch <- ev:
		default:
		}
	}
}
