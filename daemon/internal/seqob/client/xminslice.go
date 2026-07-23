package client

// xminslice.go: the minimum-slice guard for PARTIAL cross-chain fills.
//
// A partial take prices a slice of a resting cross offer down to a proportional
// BTC leg (ProportionalBtc forward / ProportionalBtcFloor reverse) and an asset
// leg of exactly the taken atoms. The zero-leg checks in the drivers only reject
// a slice that prices to a literal 0 sats — but a slice that prices to a FEW sats
// (or a few asset atoms) is just as unsettleable: after the HTLC spend fee, the
// (Amount - Fee) claim/refund output falls below the dust limit and cannot be
// relayed or economically claimed, so the leg is stranded and atomicity breaks.
//
// The guard fails CLOSED below a safe minimum on BOTH sides, for a partial only
// (a whole take always locks the whole offer, which is >> any slice and never
// creates partial dust). The maker advertises the smallest safe base slice in
// Offer.min_fill (MinFillBase); the CLI rejects a smaller -amount; and the driver
// re-checks the actual computed leg before any secret mint / leg fund.

import "fmt"

// btcDustLimit is the standard-relay dust threshold for a Bitcoin P2SH HTLC
// output: below it a spend is non-standard and will not relay or claim. 546 sats
// is Bitcoin's canonical dust value (at/above the ~540-sat P2SH default), used
// here as the conservative floor the (Amount - Fee) output must clear.
const btcDustLimit = 546

// legSpendFeeFloor is the flat native-sats fee a single HTLC claim/refund spend
// pays — the drivers' SpendFeeSats default. A safe leg must cover it with room to
// spare so the output left after the fee still clears dust. A spend fee set below
// this is floored up to it: the leg must survive a real spend either way.
const legSpendFeeFloor = 1000

// MinSafeBtcLegSats is the smallest BTC leg (sats) a partial may create: the dust
// limit plus 2x the per-spend fee floor, so the (Amount - Fee) output of BOTH a
// claim AND, on the unhappy path, a refund still clears dust. At the default
// 1000-sat fee this is 546 + 2000 = 2546 sats. A larger configured spend fee
// raises the floor proportionally.
func MinSafeBtcLegSats(spendFeeSats uint64) uint64 {
	fee := spendFeeSats
	if fee < legSpendFeeFloor {
		fee = legSpendFeeFloor
	}
	return btcDustLimit + 2*fee
}

// minSafeAssetLeg is the smallest asset leg (atoms) a partial may create so its
// claim/refund stays economical. The per-spend fee is converted to the asset's
// OWN atoms through the open-fee-market exchange rate (asset_atoms =
// ceil(fee*1e8/rate), exactly as xcSeqLegFee does; a valuable asset pays fewer
// atoms), then the leg must cover 1 atom of asset dust plus 2x that fee (claim +
// refund headroom). Without a published rate it falls back to the flat native
// target. This backstops the BTC-derived min_fill on an "expensive" asset (few
// atoms per sat), where a BTC-safe slice could still be a sub-dust pile of atoms.
func minSafeAssetLeg(ops XcOps, assetHex string, spendFeeSats uint64) uint64 {
	fee := spendFeeSats
	if fee < legSpendFeeFloor {
		fee = legSpendFeeFloor
	}
	if rate, ok := ops.SeqFeeRate(assetHex); ok && rate > 0 {
		const scale = 100_000_000
		fee = (fee*scale + rate - 1) / rate
		if fee == 0 {
			fee = 1
		}
	}
	return 1 + 2*fee
}

// MinFillBase is the smallest base-amount slice (asset atoms) of an offer priced
// at wholeBtc sats for wholeAsset atoms whose resulting BTC leg still clears
// MinSafeBtcLegSats. It is ceil(minSafeBtc * wholeAsset / wholeBtc): because the
// forward pricing rounds UP and the reverse rounds DOWN, this same value makes
// the BTC leg meet the floor under BOTH (for the floor, take >= minSafe*whole/btc
// => floor(btc*take/whole) >= minSafe). The maker publishes it as Offer.min_fill.
//
// Clamped to [1, wholeAsset]. When even the whole offer's BTC barely clears (or
// fails to clear) the floor it returns wholeAsset — advertising "whole take only,
// no partials" — so no partial can ever slip under the dust floor. The early
// return also keeps the 128-bit divide's quotient < wholeAsset, so it never
// overflows.
func MinFillBase(wholeBtc, wholeAsset, spendFeeSats uint64) uint64 {
	if wholeAsset == 0 {
		return 0
	}
	minB := MinSafeBtcLegSats(spendFeeSats)
	if wholeBtc <= minB {
		// Even a whole take is at/under the floor: only whole fills are ever safe.
		return wholeAsset
	}
	q, rem := mulDiv64(minB, wholeAsset, wholeBtc)
	if rem != 0 {
		q++
	}
	if q == 0 {
		q = 1
	}
	if q > wholeAsset {
		q = wholeAsset
	}
	return q
}

// minSafeBtcErr fails closed when a PARTIAL slice's BTC leg is below the safe
// minimum (sub-dust after the spend fee). A whole take (takeSeq >= whole) is
// exempt: its leg is the whole offer, not a partial slice.
func minSafeBtcErr(takeSeq, whole, btcLeg, spendFeeSats uint64) error {
	if takeSeq >= whole {
		return nil
	}
	if m := MinSafeBtcLegSats(spendFeeSats); btcLeg < m {
		return fmt.Errorf("%w: partial slice %d of %d prices to a %d-sat BTC leg, below the safe minimum %d (dust+fee); take a larger slice",
			ErrXcBadTerms, takeSeq, whole, btcLeg, m)
	}
	return nil
}

// minSafeAssetErr fails closed when a PARTIAL slice's asset leg (atoms) is below
// the safe minimum for economical claim/refund. A whole take is exempt.
func minSafeAssetErr(ops XcOps, assetHex string, takeSeq, whole, assetLeg, spendFeeSats uint64) error {
	if takeSeq >= whole {
		return nil
	}
	if m := minSafeAssetLeg(ops, assetHex, spendFeeSats); assetLeg < m {
		return fmt.Errorf("%w: partial slice %d of %d leaves a %d-atom asset leg, below the safe minimum %d (dust+fee); take a larger slice",
			ErrXcBadTerms, takeSeq, whole, assetLeg, m)
	}
	return nil
}
