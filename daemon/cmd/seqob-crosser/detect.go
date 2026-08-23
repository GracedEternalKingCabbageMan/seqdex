package main

// detect.go — pure crossing logic: find the best executable bid/ask pair whose
// prices cross, clamp the overlap to what BOTH legs can legally settle, and gate
// on profitability. No I/O, no execution — fully unit-tested.

import (
	"fmt"
	"sort"
)

// Executability decides whether the crosser can DRIVE a given order's taker side
// with its configured inventory/nodes; reason explains a false.
type Executability func(n *NormOrder) (ok bool, reason string)

// CrossPlan is a fully-sized, gated, ordered plan for one crossing: buy Take
// base from Ask, sell Take base into Bid, keep the spread.
type CrossPlan struct {
	Bid, Ask *NormOrder
	Take     uint64 // base atoms executed on BOTH legs

	Cost    uint64 // quote atoms paid to the ask (ceil, maker-favoured)
	Revenue uint64 // quote atoms received from the bid (floor, maker-favoured)
	Gross   uint64 // Revenue - Cost
	EstCost uint64 // estimated execution costs (quote atoms), see EstimateCosts
	MinBps  uint64 // the bps floor applied

	// Legs in execution order (see legorder.go).
	First, Second *NormOrder
	OrderReason   string
}

// Detection is what one pass over a pair's union book produced: either a plan,
// or the reason there is none (both are logged — one line per detection).
type Detection struct {
	Plan *CrossPlan
	Skip string // non-empty when no plan (includes "no cross" and every gate)

	// BestBid/BestAsk are the best EXECUTABLE levels seen (nil if none) — logged
	// even when they do not cross, so the operator can see the standing spread.
	BestBid, BestAsk *NormOrder
}

// SortBook orders one side by price-time priority: bids best(highest)-first,
// asks best(lowest)-first, ties broken by created_at (older first).
func SortBook(side []*NormOrder) {
	sort.SliceStable(side, func(i, j int) bool {
		a, b := side[i], side[j]
		aGE, bGE := a.PriceGE(b), b.PriceGE(a)
		if aGE != bGE { // strict price difference
			if a.Side == SideBid {
				return aGE // higher bid first
			}
			return bGE // lower ask first
		}
		return a.CreatedAt < b.CreatedAt
	})
}

// Detect runs one detection pass over a pair's normalized union book.
func Detect(orders []*NormOrder, canExec Executability, gate GateConfig) Detection {
	var bids, asks []*NormOrder
	var skips []string
	for _, n := range orders {
		if ok, why := canExec(n); !ok {
			skips = append(skips, fmt.Sprintf("%s(%s %s): %s", n.ID(), n.Family, n.Side, why))
			continue
		}
		if n.Side == SideBid {
			bids = append(bids, n)
		} else {
			asks = append(asks, n)
		}
	}
	SortBook(bids)
	SortBook(asks)

	d := Detection{}
	if len(bids) > 0 {
		d.BestBid = bids[0]
	}
	if len(asks) > 0 {
		d.BestAsk = asks[0]
	}
	if d.BestBid == nil || d.BestAsk == nil {
		d.Skip = fmt.Sprintf("no executable %s side (inexecutable: %d)", missingSide(d), len(skips))
		return d
	}
	bid, ask := d.BestBid, d.BestAsk
	if !bid.PriceGE(ask) {
		d.Skip = "no cross (best bid < best ask)"
		return d
	}
	if bid.Offer.GetMakerPubkey() == ask.Offer.GetMakerPubkey() {
		// The same maker resting on both sides is that maker's own spread, not an
		// arbitrage for us to close against them twice.
		d.Skip = "both sides same maker"
		return d
	}

	take, why := ClampOverlap(bid, ask)
	if take == 0 {
		d.Skip = "unsizeable: " + why
		return d
	}

	plan := &CrossPlan{Bid: bid, Ask: ask, Take: take}
	plan.Cost = ask.CostFor(take)
	plan.Revenue = bid.RevenueFor(take)
	if ok, detail := Profitable(plan, gate); !ok {
		d.Skip = "unprofitable: " + detail
		return d
	}
	plan.First, plan.Second, plan.OrderReason = OrderLegs(bid, ask)
	d.Plan = plan
	return d
}

func missingSide(d Detection) string {
	switch {
	case d.BestBid == nil && d.BestAsk == nil:
		return "bid+ask"
	case d.BestBid == nil:
		return "bid"
	default:
		return "ask"
	}
}

// ClampOverlap sizes the crossing: the min of the two sizes, shrunk (never
// grown) until BOTH legs can legally settle exactly `take`:
//
//   - a leg taken WHOLE (take == its size) is always legal;
//   - a PARTIAL leg needs a sliceable driver + allow_partial + take >= min_fill;
//   - a partial COVENANT leg additionally needs take >= min_lot and a remainder
//     that is 0 or >= min_lot — when the remainder would be a dust sliver the
//     take is reduced to (size - min_lot), the largest fill leaving a valid
//     remainder, and both legs are re-checked at the smaller size.
//
// It returns 0 with a reason when no legal common size exists. Deterministic
// and terminating: take only ever shrinks, and each covenant reduction is final
// for that leg (its remainder is then exactly min_lot).
func ClampOverlap(bid, ask *NormOrder) (uint64, string) {
	take := bid.BaseSize
	if ask.BaseSize < take {
		take = ask.BaseSize
	}
	for iter := 0; iter < 4; iter++ {
		reduced := false
		for _, leg := range []*NormOrder{bid, ask} {
			if take == leg.BaseSize {
				continue // whole take, always legal
			}
			if take > leg.BaseSize { // cannot happen (take starts at the min) — defensive
				return 0, "internal: take exceeds a leg"
			}
			if !leg.Sliceable {
				return 0, fmt.Sprintf("%s leg (%s) is whole-take only but overlap %d < its size %d",
					leg.Side, leg.Family, take, leg.BaseSize)
			}
			if !leg.AllowPartial {
				return 0, fmt.Sprintf("%s leg forbids partial fills (overlap %d < size %d)",
					leg.Side, take, leg.BaseSize)
			}
			if leg.MinFill > 0 && take < leg.MinFill {
				return 0, fmt.Sprintf("overlap %d below %s leg min_fill %d", take, leg.Side, leg.MinFill)
			}
			if leg.Family == FamCovenant && leg.MinLot > 0 {
				if take < leg.MinLot {
					return 0, fmt.Sprintf("overlap %d below covenant min_lot %d", take, leg.MinLot)
				}
				if rem := leg.BaseSize - take; rem > 0 && rem < leg.MinLot {
					if leg.BaseSize <= leg.MinLot {
						return 0, "covenant leg cannot leave a valid remainder"
					}
					take = leg.BaseSize - leg.MinLot
					reduced = true
				}
			}
		}
		if !reduced {
			return take, ""
		}
	}
	return 0, "could not converge on a legal common size"
}

// GateConfig parameterizes the profitability gate.
type GateConfig struct {
	MinProfitBps uint64 // floor on gross/cost, in basis points (flag -min-profit-bps)
	// SpendFeeAtoms approximates ONE on-chain HTLC/tx spend in QUOTE atoms (flag
	// -spend-fee-atoms). It is an operator-set estimate, not a fee quote: leg
	// fees accrue in different assets (BTC sats, asset atoms via the open fee
	// market) and a precise conversion would need the price server; the gate only
	// needs a conservative ceiling.
	SpendFeeAtoms uint64
}

// onchainSpends is the per-family count of on-chain transactions the CROSSER's
// taker side pays for in one settled lift (funding + claims it fees itself; the
// maker's own spends are the maker's). Used only for cost ESTIMATION.
func onchainSpends(f Family) uint64 {
	switch f {
	case FamPureLN:
		return 0 // both legs Lightning; routing fees are epsilon
	case FamSameChain:
		return 1 // the single co-signed swap tx
	case FamCovenant:
		return 1 // the FILL tx
	case FamSubmarine:
		return 1 // the taker's single on-chain asset leg (fund or claim)
	case FamSubAsset:
		return 2 // fund the on-chain HTLC + (device/wallet) claim, or claim path
	case FamCross:
		return 2 // fund own leg + claim the counterparty leg
	default:
		return 2
	}
}

// EstimateCosts is the estimated quote-atom execution cost of a plan.
func EstimateCosts(bid, ask *NormOrder, cfg GateConfig) uint64 {
	return (onchainSpends(bid.Family) + onchainSpends(ask.Family)) * cfg.SpendFeeAtoms
}

// Profitable applies the never-at-a-loss gate: the gross spread on the overlap
// must strictly exceed the estimated execution costs AND clear the bps floor on
// the deployed notional (the quote paid to the ask).
func Profitable(p *CrossPlan, cfg GateConfig) (bool, string) {
	if p.Revenue <= p.Cost {
		return false, fmt.Sprintf("revenue %d <= cost %d", p.Revenue, p.Cost)
	}
	p.Gross = p.Revenue - p.Cost
	p.EstCost = EstimateCosts(p.Bid, p.Ask, cfg)
	p.MinBps = cfg.MinProfitBps
	if p.Gross <= p.EstCost {
		return false, fmt.Sprintf("gross %d does not cover estimated leg costs %d", p.Gross, p.EstCost)
	}
	// bps floor on the net (after estimated costs), relative to deployed capital.
	net := p.Gross - p.EstCost
	// net/cost >= bps/10000  <=>  net*10000 >= bps*cost (uint64-safe via big-free
	// check: values bounded by atom sizes; use the ceil helper's big path).
	lhs := ceilMulDiv(net, 10000, 1)
	rhs := ceilMulDiv(p.Cost, cfg.MinProfitBps, 1)
	if lhs < rhs {
		return false, fmt.Sprintf("net %d on cost %d is under %d bps", net, p.Cost, cfg.MinProfitBps)
	}
	return true, ""
}
