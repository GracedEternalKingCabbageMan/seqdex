package xchain

import (
	"errors"
	"fmt"
)

// ChainTiming is how a chain's blocks arrive, in seconds per block, for deadline
// arithmetic ACROSS chains. A deadline expressed in blocks of one chain must be
// compared with one on another chain in time, and honestly: the chain we need to
// be slow is assumed slow, the one we need to be fast is assumed fast.
type ChainTiming struct {
	Nominal uint32 // the target spacing
	Fast    uint32 // the fastest sustained spacing worth defending against
	Slow    uint32 // the slowest sustained spacing worth defending against
}

var (
	// BTCTiming: Poisson blocks around 600 s; runs of fast blocks reach ~150 s
	// average over an hour, lulls stretch past 900 s.
	BTCTiming = ChainTiming{Nominal: 600, Fast: 150, Slow: 900}
	// SeqTiming: fixed 60 s slots, so blocks can never come faster than 60 s; a
	// missed slot or two stretches the average toward 90 s.
	SeqTiming = ChainTiming{Nominal: 60, Fast: 60, Slow: 90}
)

// chainTimer is implemented by legs that know which chain their HTLCs expire on.
type chainTimer interface {
	Timing() ChainTiming
}

// ErrCltvUncapped is returned when a payment's timelock could not be bounded
// against the deadline that protects the payer: the legs do not report their
// chain timing, or the hold did not report its expiry. Paying blind is never the
// right default on a rail where the counterparty may hold the outgoing HTLC.
var ErrCltvUncapped = errors.New("xchain: cannot bound the outgoing timelock against the incoming deadline")

// CoverDelay is the number of blocks on the incoming chain an incoming hold must
// still have so that an outgoing payment with up to outDelay blocks of timelock
// resolves first, with marginSecs to spare: the outgoing chain is taken slow, the
// incoming one fast.
func CoverDelay(outDelay uint32, out, in ChainTiming, marginSecs uint32) uint32 {
	secs := uint64(outDelay)*uint64(out.Slow) + uint64(marginSecs)
	return uint32((secs + uint64(in.Fast) - 1) / uint64(in.Fast))
}

// CapDelay is the largest outgoing timelock, in blocks of the outgoing chain,
// that still resolves at least marginSecs before an incoming deadline inRemaining
// blocks away: the incoming chain is taken fast, the outgoing slow. 0 means
// nothing fits.
func CapDelay(inRemaining uint32, in, out ChainTiming, marginSecs uint32) uint32 {
	secs := uint64(inRemaining) * uint64(in.Fast)
	if secs <= uint64(marginSecs) {
		return 0
	}
	return uint32((secs - uint64(marginSecs)) / uint64(out.Slow))
}

// RouteAllowance is the timelock slack, in blocks of the paying leg's chain, that
// an outgoing payment may add on top of the invoice's min_final_cltv for its
// route. Both rails here settle over direct or one-hop channels; the allowance
// covers a hop's cltv_expiry_delta without opening the door to a multi-day route.
func RouteAllowance(t ChainTiming) uint32 {
	if t.Nominal >= 600 {
		return 24 // ~4 h of Bitcoin blocks
	}
	return 120 // 2 h of Sequentia slots
}

// timingOf returns the leg's chain timing, or an error for a leg that has none.
func timingOf(leg LNLeg, what string) (ChainTiming, error) {
	if t, ok := leg.(chainTimer); ok {
		return t.Timing(), nil
	}
	return ChainTiming{}, fmt.Errorf("%w: %s leg reports no chain timing", ErrCltvUncapped, what)
}
