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
	"fmt"
	"sync"
	"time"

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

// Submit verifies the offer's signature and inserts it as a new OPEN order.
// It is an error to Submit a key that already exists (use Edit to re-sign).
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
		return Key{}, fmt.Errorf("offer %s/%s already exists", k.MakerPubkey, k.OfferID)
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

// SnapshotPair returns all live offers for a pair.
func (s *Store) SnapshotPair(p *seqobv1.AssetPair) []*seqobv1.Offer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked(pairKey(p))
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
		_ = pk
		out = append(out, &seqobv1.Market{
			Pair:    pair,
			BestBid: bestBid,
			BestAsk: bestAsk,
			NOrders: n,
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
