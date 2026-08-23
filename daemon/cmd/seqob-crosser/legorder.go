package main

// legorder.go — which leg of a crossing executes FIRST.
//
// Both legs are individually atomic swaps (co-sign / HTLC / hold-invoice), so a
// failed leg never loses funds outright — the failure mode is INVENTORY DRIFT:
// leg 1 settles, leg 2 does not, and the crosser is long/short until it unwinds.
// The exposure window is therefore [leg-1 settled .. leg-2 settled], and the
// ordering rule is FAIL-FAST, CLOSE-FAST:
//
//   run FIRST the leg with the LOWER settlement certainty/speed — if it fails,
//   it fails with NOTHING executed (zero drift); once it settles, close the
//   position with the fastest, most reliable leg, keeping the window minimal.
//
// Proceeds of leg 1 are NOT recycled into leg 2 within a crossing: the legs run
// on different rails (LN channel balance vs on-chain UTXOs) and value does not
// teleport between rails, so the crosser needs standing inventory on BOTH rails
// and the ordering choice is purely about the drift window, never about funding
// leg 2 from leg 1.
//
// Certainty/speed score (higher = safer/faster = prefer as leg 2):
//
//	5  covenant   consensus-enforced passive fill: no maker liveness, one tx;
//	              (not yet executable — scored for future family pairs)
//	4  pureln     seconds; a failed attempt unwinds NATIVELY by hold timeout,
//	              leaving zero on-chain residue
//	3  samechain  one co-signed tx, fast, but needs the maker online to co-sign
//	2  subasset   LN leg + single-chain HTLC: minutes, refund after locktime
//	2  submarine  same class (asset on-chain + BTC-LN)
//	1  cross      two-chain HTLC: slowest (parent-chain confirmations, ~hours
//	              of locktime to refund a stranded leg)
//
// Derived full matrix (row = one leg's family, col = the other's; the LOWER
// score executes first; the cell shows which goes first):
//
//	            covenant  pureln  samechain  subasset  submarine  cross
//	covenant       tie*    pureln₂ samechain₂ subasset₁ submarine₁ cross₁
//	pureln                 tie*    samechain₁ subasset₁ submarine₁ cross₁
//	samechain                      tie*       subasset₁ submarine₁ cross₁
//	subasset                                  tie*      tie*       cross₁
//	submarine                                           tie*       cross₁
//	cross                                                          tie*
//
//	₁ = that family's leg runs FIRST (it is the slower/less certain one)
//	₂ = it runs SECOND
//	tie* = equal scores: the BUY (ask) leg runs first — a leg-2 failure then
//	       leaves the crosser LONG the traded asset (bounded by what it paid,
//	       counted against -max-exposure-atoms, and re-sellable into the still-
//	       resting bid on a later pass), rather than short an asset it must
//	       re-acquire at an unknown price.

func famScore(f Family) int {
	switch f {
	case FamCovenant:
		return 5
	case FamPureLN:
		return 4
	case FamSameChain:
		return 3
	case FamSubAsset, FamSubmarine:
		return 2
	case FamCross:
		return 1
	default:
		return 0
	}
}

// OrderLegs returns (first, second, reason) for a bid/ask pair per the table.
func OrderLegs(bid, ask *NormOrder) (first, second *NormOrder, reason string) {
	bs, as := famScore(bid.Family), famScore(ask.Family)
	switch {
	case as < bs:
		return ask, bid, "ask leg (" + ask.Family.String() + ") is slower/less certain: fail-fast first, close with " + bid.Family.String()
	case bs < as:
		return bid, ask, "bid leg (" + bid.Family.String() + ") is slower/less certain: fail-fast first, close with " + ask.Family.String()
	default:
		return ask, bid, "equal certainty (" + ask.Family.String() + "): BUY first so a leg-2 failure leaves us long the base, not short"
	}
}
