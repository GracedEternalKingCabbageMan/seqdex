package main

// xpartial.go: the CLI-side minimum-slice guard shared by xlift (forward) and
// xsell (reverse). A partial -amount must never price to a BTC leg so small that,
// after the HTLC spend fee, the leg's claim/refund output is sub-dust and cannot
// be settled. The driver re-checks this defensively before funding, but rejecting
// at the CLI gives the operator a clear, fatal message before any coins move.

import (
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
)

// guardPartialBtcLeg returns a non-nil error when a PARTIAL take (takeSeq <
// wholeSeq) is below the offer's advertised min_fill, or when its computed BTC leg
// (btcLeg sats) is below client.MinSafeBtcLegSats — i.e. the slice would strand a
// sub-dust HTLC leg. A whole take (takeSeq >= wholeSeq) always passes; the whole
// offer's leg is never partial dust. minFill may be 0 (offer did not advertise
// one): the BTC-leg floor still applies.
func guardPartialBtcLeg(takeSeq, wholeSeq, btcLeg, minFill, spendFee uint64) error {
	if takeSeq >= wholeSeq {
		return nil
	}
	if minFill > 0 && takeSeq < minFill {
		return fmt.Errorf("-amount %d is below this offer's min_fill %d (smaller partials strand sub-dust HTLC legs); take at least %d",
			takeSeq, minFill, minFill)
	}
	if m := client.MinSafeBtcLegSats(spendFee); btcLeg < m {
		return fmt.Errorf("-amount %d prices to a %d-sat BTC leg, below the safe minimum %d (dust+fee); take a larger slice",
			takeSeq, btcLeg, m)
	}
	return nil
}
