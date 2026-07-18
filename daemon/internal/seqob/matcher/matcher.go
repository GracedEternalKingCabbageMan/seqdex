// Package matcher is the SeqOB relay's continuous limit-order-book matching
// engine: per-pair, price-time-priority crossing on top of the offerstore.
//
// On every accepted offer_submit the relay runs Cross(incoming): it walks the
// resting OPPOSITE side of the incoming order's pair, best-price-first then
// oldest-first, and while the incoming crosses the top resting order (integer-
// ratio compare, the same price floor ValidateRequestAgainstOffer enforces) it
// computes a fill size (honoring allow_partial / min_fill, and — for a covenant
// order — the covenant's own min_lot floor on both the fill and the remainder),
// decrements both orders via offerstore.ApplyPartialFill, and records a Match.
// Any incoming remainder rests.
//
// The matcher is I/O-free: it returns []Match for the caller (the api layer,
// which owns the websocket connections) to route as From.matched messages. For
// a COVENANT resting order the maker is offline; the Match carries the covenant
// terms + this fill's recipe so the counterparty (taker) settles the
// permissionless FILL spend itself, with nobody online but the taker.
//
// Advisory-decrement caveat: for a covenant match the on-chain FILL spend is
// authoritative (the order is the coin; first spend wins). The relay decrements
// optimistically at match time so the book reflects the intent; a chain watcher
// reconciling the book against the actual UTXO set (re-opening an order whose
// fill never confirmed, or whose anchor was orphaned) is out of scope here and
// noted as remaining work.
package matcher

import (
	"math/big"
	"sort"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
)

// isSubAssetOffer reports whether an offer is a sub-asset swap (asset over LN <->
// BTC on-chain HTLC). Both directions rest on the <asset>/BTC pair on opposite
// sides with lined-up assets, so without a guard the matcher would auto-cross a
// sub-asset BUY (ln 4) against a sub-asset SELL (ln 5).
func isSubAssetOffer(o *seqobv1.Offer) bool {
	ln := o.GetLightning()
	return ln != nil && offer.IsSubAsset(ln.GetLnDirection())
}

// Book is the subset of offerstore.Store the matcher needs (so it can be faked
// in tests).
type Book interface {
	SnapshotPairEntries(p *seqobv1.AssetPair, confidential bool) []offerstore.Entry
	Get(k offerstore.Key) (*offerstore.Entry, bool)
	ApplyPartialFill(k offerstore.Key, filledBase uint64, settleTxid string, anchorConfs uint32) error
	RecordTrade(restingOffer *seqobv1.Offer, sizeBase uint64)
}

// Match is one crossing of an incoming order against a resting order.
type Match struct {
	Pair        *seqobv1.AssetPair
	RestingKey  offerstore.Key
	Resting     *seqobv1.Offer
	IncomingKey offerstore.Key
	Incoming    *seqobv1.Offer
	FillBase    uint64 // base atoms filled
	FillQuote   uint64 // quote atoms owed for the fill (ceil price, maker's favour)

	// RestingCovenant is non-nil iff the resting order settles via a covenant
	// (maker offline; taker builds the FILL spend). RestingLocked is the atoms of
	// asset A still locked in that covenant UTXO before this fill.
	RestingCovenant *seqobv1.CovenantTerms
	RestingLocked   uint64
	// IncomingCovenant is non-nil iff the INCOMING order is also a covenant: both
	// makers are offline and an always-online settler must build the joint spend.
	IncomingCovenant *seqobv1.CovenantTerms

	// RestingLightning / IncomingLightning are non-nil iff that side chose the
	// Lightning rail (its owner settles the cross off-chain, not with an on-chain
	// covenant FILL or same-chain co-sign). They are the cross-rail dual of the
	// covenant fields: when EXACTLY ONE side is a covenant and the OTHER is
	// Lightning, this Match is a CROSS-RAIL cross (CrossRail) that no same-rail
	// path can settle — the two owners each rest on a different rail. It is bridged
	// by the silent settlement BRIDGE (cmd/seqob-bridge), which fills the on-chain
	// covenant and settles the Lightning leg atomically under one preimage so the
	// two orders cross invisibly to their owners.
	RestingLightning  *seqobv1.LightningTerms
	IncomingLightning *seqobv1.LightningTerms
}

// BothCovenant reports the two-covenant / both-offline case.
func (m *Match) BothCovenant() bool {
	return m.RestingCovenant != nil && m.IncomingCovenant != nil
}

// CrossRail reports the cross-rail case the bridge settles: exactly one side
// rests on-chain as a covenant and the other rests on Lightning. This is the
// only match kind neither the covenant FILL (single or joint) nor the same-chain
// co-sign path can settle by itself, because the two owners chose different
// rails; the bridge crosses them by moving value between the rails under one
// shared preimage. A same-rail match (both covenant, both same-chain, both
// Lightning) is NOT cross-rail and keeps its existing settlement path.
func (m *Match) CrossRail() bool {
	covs, lns := 0, 0
	if m.RestingCovenant != nil {
		covs++
	}
	if m.IncomingCovenant != nil {
		covs++
	}
	if m.RestingLightning != nil {
		lns++
	}
	if m.IncomingLightning != nil {
		lns++
	}
	return covs == 1 && lns == 1
}

// BridgeCovenant returns the covenant (on-chain) side's terms and whether it is
// the RESTING side, for a CrossRail match. The bridge fills this covenant. It
// returns (nil, false) when the match is not cross-rail.
func (m *Match) BridgeCovenant() (terms *seqobv1.CovenantTerms, resting bool) {
	if !m.CrossRail() {
		return nil, false
	}
	if m.RestingCovenant != nil {
		return m.RestingCovenant, true
	}
	return m.IncomingCovenant, false
}

// BridgeLightning returns the Lightning side's terms for a CrossRail match (the
// leg the bridge settles off-chain via the submarine/pure-LN orchestrator), or
// nil when the match is not cross-rail.
func (m *Match) BridgeLightning() *seqobv1.LightningTerms {
	if !m.CrossRail() {
		return nil
	}
	if m.RestingLightning != nil {
		return m.RestingLightning
	}
	return m.IncomingLightning
}

// Matcher crosses incoming orders against a Book.
type Matcher struct{ book Book }

// New returns a Matcher over book.
func New(book Book) *Matcher { return &Matcher{book: book} }

// Cross matches a freshly-rested incoming order against the resting opposite
// side and applies the fills to the book. It returns the matches (best-price,
// oldest-first order) for the caller to route. If nothing crosses, the incoming
// order simply stays resting and Cross returns nil.
func (m *Matcher) Cross(incoming *seqobv1.Offer) []Match {
	inKey := offerstore.Key{MakerPubkey: incoming.GetMakerPubkey(), OfferID: incoming.GetOfferId()}
	inEntry, ok := m.book.Get(inKey)
	if !ok {
		return nil
	}
	incomingRemaining := inEntry.ActiveAmount
	if incomingRemaining == 0 {
		return nil
	}

	rest := m.candidates(incoming)
	if len(rest) == 0 {
		return nil
	}

	// Plan (pure) so we can honor an all-or-nothing incoming before mutating.
	type planned struct {
		e         offerstore.Entry
		key       offerstore.Key
		fillBase  uint64
		fillQuote uint64
	}
	var plan []planned
	rem := incomingRemaining
	for i := range rest {
		if rem == 0 {
			break
		}
		e := rest[i]
		restActive := e.ActiveAmount
		if restActive == 0 {
			continue
		}
		fillBase := restActive
		if rem < fillBase {
			fillBase = rem
		}
		partialOfResting := fillBase < restActive

		// allow_partial / min_fill on the resting order.
		if partialOfResting && !e.Offer.GetAllowPartial() {
			continue
		}
		if partialOfResting && e.Offer.GetMinFill() > 0 && fillBase < e.Offer.GetMinFill() {
			continue
		}

		// Covenant floors: filled >= min_lot; remainder == 0 or >= min_lot. The
		// on-chain FILL leaf enforces these, so a match that violated them would be
		// unsettleable — reject it here.
		cov := e.Offer.GetCovenant()
		if cov != nil {
			ml := cov.GetMinLot()
			if fillBase < ml {
				continue
			}
			if r := restActive - fillBase; r != 0 && r < ml {
				// Trim to leave exactly a >= min_lot remainder, if the incoming can.
				if restActive >= ml && rem >= restActive-ml {
					fillBase = restActive - ml
				} else {
					continue
				}
			}
		}

		fq := fillQuote(e.Offer, fillBase)
		if fq == 0 {
			// fillBase > 0 but the quote came out 0: either a zero rate denominator or a
			// quote that overflowed uint64 (fillQuote returns 0 for both). Emitting the Match
			// anyway would advertise "0 quote owed" for real base, so skip this candidate.
			continue
		}
		plan = append(plan, planned{
			e: e, key: keyOf(e.Offer),
			fillBase:  fillBase,
			fillQuote: fq,
		})
		rem -= fillBase
	}

	// All-or-nothing incoming: if it disallows partials and the crossing
	// liquidity cannot fill it completely, execute nothing (leave it resting).
	if !incoming.GetAllowPartial() && rem != 0 {
		return nil
	}
	if len(plan) == 0 {
		return nil
	}

	out := make([]Match, 0, len(plan))
	for _, p := range plan {
		// Apply to the resting order and to the incoming order.
		_ = m.book.ApplyPartialFill(p.key, p.fillBase, "", 0)
		_ = m.book.ApplyPartialFill(inKey, p.fillBase, "", 0)
		// Record the cross for last_price / trades / candles (execution price = resting order's price).
		m.book.RecordTrade(p.e.Offer, p.fillBase)

		mt := Match{
			Pair:        incoming.GetPair(),
			RestingKey:  p.key,
			Resting:     p.e.Offer,
			IncomingKey: inKey,
			Incoming:    incoming,
			FillBase:    p.fillBase,
			FillQuote:   p.fillQuote,
		}
		if cov := p.e.Offer.GetCovenant(); cov != nil {
			mt.RestingCovenant = cov
			mt.RestingLocked = p.e.ActiveAmount // atoms of A locked before this fill
		}
		if cov := incoming.GetCovenant(); cov != nil {
			mt.IncomingCovenant = cov
		}
		// Surface the Lightning rail on either side so a covenant-vs-Lightning cross
		// classifies as CrossRail (the bridge's job). Same-rail Lightning matches
		// (both sides Lightning) still carry these but are not CrossRail.
		if ln := p.e.Offer.GetLightning(); ln != nil {
			mt.RestingLightning = ln
		}
		if ln := incoming.GetLightning(); ln != nil {
			mt.IncomingLightning = ln
		}
		out = append(out, mt)
	}
	return out
}

// candidates returns the resting opposite-side orders of incoming's pair that
// (a) are live with active size and (b) cross incoming's price, sorted best-
// price-first then oldest-first (price-time priority).
func (m *Matcher) candidates(incoming *seqobv1.Offer) []offerstore.Entry {
	// Snapshot only the incoming order's OWN book namespace (confidential vs
	// transparent), so a confidential offer only ever sees confidential candidates.
	all := m.book.SnapshotPairEntries(incoming.GetPair(), incoming.GetConfidential())
	inKey := keyOf(incoming)
	buy := incoming.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_BUY

	var c []offerstore.Entry
	for _, e := range all {
		if keyOf(e.Offer) == inKey {
			continue // never match against self
		}
		if e.ActiveAmount == 0 {
			continue
		}
		// Belt-and-suspenders even with namespaced snapshots: a confidential offer
		// crosses ONLY another confidential offer. Both legs must blind on-chain, else
		// an observer reads the confidential leg's amount off the public swap ratio.
		if e.Offer.GetConfidential() != incoming.GetConfidential() {
			continue
		}
		// Opposite side only.
		if (e.Offer.GetTradeDir() == seqobv1.TradeDir_TRADE_DIR_BUY) == buy {
			continue
		}
		// Same-pair opposite orders line up assets by construction; require the
		// swap to be well-formed and the price to cross.
		if incoming.GetOfferAsset() != e.Offer.GetWantAsset() ||
			incoming.GetWantAsset() != e.Offer.GetOfferAsset() {
			continue
		}
		// Sub-asset offers (asset-LN <-> BTC on-chain HTLC) are COURIER-LIFT ONLY: a
		// taker runs the HTLC handshake, and the relay has no maker-vs-maker settlement
		// path for them, so they must never auto-cross. This keeps a sub-asset BUY
		// (ln 4) and SELL (ln 5) coexisting on the asset/BTC pair without crossing.
		if isSubAssetOffer(incoming) || isSubAssetOffer(e.Offer) {
			continue
		}
		if !crosses(incoming, e.Offer) {
			continue
		}
		c = append(c, e)
	}
	sort.SliceStable(c, func(i, j int) bool {
		pi, pj := quotePerBase(c[i].Offer), quotePerBase(c[j].Offer)
		cmp := pi.Cmp(pj)
		if cmp != 0 {
			if buy {
				return cmp < 0 // incoming BUY wants the cheapest ask first
			}
			return cmp > 0 // incoming SELL wants the richest bid first
		}
		return c[i].CreatedAt.Before(c[j].CreatedAt) // time priority
	})
	return c
}

// crosses reports whether the incoming (taker) order is willing to pay at least
// the resting (maker) order's price. The taker pays maker.want_asset and
// receives maker.offer_asset; the floor is taker.offer/taker.want >=
// maker.want/maker.offer, cleared of division:
//
//	incoming.offer_amount * resting.offer_amount >= resting.want_amount * incoming.want_amount
func crosses(incoming, resting *seqobv1.Offer) bool {
	lhs := new(big.Int).Mul(u(incoming.GetOfferAmount()), u(resting.GetOfferAmount()))
	rhs := new(big.Int).Mul(u(resting.GetWantAmount()), u(incoming.GetWantAmount()))
	return lhs.Cmp(rhs) >= 0
}

// quotePerBase is the resting order's price as quote atoms per base atom, for
// sorting. For a SELL (gives base=offer, wants quote=want): want/offer. For a
// BUY (gives quote=offer, wants base=want): offer/want.
func quotePerBase(o *seqobv1.Offer) *big.Rat {
	switch o.GetTradeDir() {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		return new(big.Rat).SetFrac(u(o.GetWantAmount()), u(o.GetOfferAmount()))
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		return new(big.Rat).SetFrac(u(o.GetOfferAmount()), u(o.GetWantAmount()))
	default:
		return new(big.Rat)
	}
}

// fillQuote is the quote atoms owed for filling fillBase of the resting order,
// ceil-rounded in the maker's favour. For a covenant it uses the on-chain rate
// (authoritative); otherwise the offer's want/offer ratio. In both cases the
// quote is measured against the resting order's BASE size.
func fillQuote(resting *seqobv1.Offer, fillBase uint64) uint64 {
	var num, den uint64
	if cov := resting.GetCovenant(); cov != nil {
		num, den = cov.GetRateNum(), cov.GetRateDen()
	} else {
		// quote per base: SELL want/offer; BUY offer/want. base == offer(SELL) or
		// want(BUY), and fillBase is in base atoms, so quote = fillBase * (quote/base).
		switch resting.GetTradeDir() {
		case seqobv1.TradeDir_TRADE_DIR_SELL:
			num, den = resting.GetWantAmount(), resting.GetOfferAmount()
		case seqobv1.TradeDir_TRADE_DIR_BUY:
			num, den = resting.GetOfferAmount(), resting.GetWantAmount()
		}
	}
	if den == 0 {
		return 0
	}
	// ceil(fillBase * num / den) with big.Int to avoid overflow.
	p := new(big.Int).Mul(u(fillBase), u(num))
	p.Add(p, u(den-1))
	p.Quo(p, u(den))
	if !p.IsUint64() {
		return 0 // B-6: quote doesn't fit uint64 — p.Uint64() would silently wrap to a wrong amount; reject the match instead
	}
	return p.Uint64()
}

func u(x uint64) *big.Int { return new(big.Int).SetUint64(x) }

func keyOf(o *seqobv1.Offer) offerstore.Key {
	return offerstore.Key{MakerPubkey: o.GetMakerPubkey(), OfferID: o.GetOfferId()}
}
