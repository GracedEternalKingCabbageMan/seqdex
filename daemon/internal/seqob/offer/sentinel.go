package offer

// BTCSentinel is the canonical asset-id placeholder for the Bitcoin (parent-chain)
// side of a cross-chain offer. It is the literal "BTC" (matching the web wallet's
// existing sentinel), deliberately NOT a 64-hex Sequentia asset id so it can never
// collide with a real issued asset and is trivially distinguishable in a pair.
const BTCSentinel = "BTC"

// Cross-chain swap direction (CrossChainTerms.direction). Roles follow the proven
// pkg/xchain single-secret HTLC pattern:
//
//	DirBTCToAsset: the taker pays BTC and receives the SEQ asset; the maker SELLs the
//	  asset. The TAKER holds the secret (locks BTC first; the maker then locks the SEQ leg).
//	DirAssetToBTC: the taker pays the SEQ asset and receives BTC; the maker BUYs the
//	  asset. The MAKER holds the secret (locks BTC first; the taker then funds the SEQ leg).
const (
	DirBTCToAsset uint32 = 0
	DirAssetToBTC uint32 = 1
)

// Lightning (submarine-swap) direction (LightningTerms.ln_direction). The asset
// leg is always on-chain (Sequentia); the BTC leg is over Lightning. The name is
// FLOW-oriented (what is exchanged for what); the maker's trade side is the
// opposite of the party giving the asset:
//
//	LnAssetForBTC (NORMAL, on-chain -> LN): the on-chain asset is exchanged for
//	  BTC-LN. The TAKER holds the secret: it mints a BOLT11 on P, funds the asset
//	  HTLC (claim=maker), and the maker pays the invoice (learning P) and CLAIMS
//	  the asset. So the maker acquires the asset -> the maker BUYS (trade_dir=BUY).
//	LnBTCForAsset (REVERSE, LN -> on-chain): BTC-LN is exchanged for the on-chain
//	  asset. The maker locks the asset (claim=taker) and the taker pays the maker's
//	  invoice. The maker gives up the asset -> the maker SELLS (trade_dir=SELL).
//	  Secret holder depends on maker_issues_hold_invoice.
const (
	LnAssetForBTC uint32 = 0
	LnBTCForAsset uint32 = 1
	// Pure-LN directions (Step 2): BOTH legs are off-chain Lightning — a Sequentia
	// asset-LN leg and a BTC-LN leg — stitched by one shared secret, with no
	// on-chain leg and no anchor gate. Same flow orientation as above:
	//   LnAssetLNForBTCLN: asset-LN exchanged for BTC-LN; the maker acquires the
	//     asset -> BUY (the maker holds the incoming asset-LN, pays BTC-LN).
	//   LnBTCLNForAssetLN: BTC-LN exchanged for asset-LN; the maker gives the asset
	//     -> SELL (the maker holds the incoming BTC-LN, pays asset-LN).
	LnAssetLNForBTCLN uint32 = 2
	LnBTCLNForAssetLN uint32 = 3
)

// IsPureLN reports whether an ln_direction is a pure-LN (both-legs-Lightning)
// swap, as opposed to the on-chain-asset submarine directions.
func IsPureLN(lnDirection uint32) bool {
	return lnDirection == LnAssetLNForBTCLN || lnDirection == LnBTCLNForAssetLN
}

// IsBTCSentinel reports whether an asset id is the BTC sentinel.
func IsBTCSentinel(assetID string) bool { return assetID == BTCSentinel }

// DirectionConsistent checks that a cross-chain offer's direction matches its trade
// direction, for a cross-chain pair whose base is the SEQ asset and quote is the BTC
// sentinel: a SELL (maker gives the asset, wants BTC) is BTC_TO_ASSET (the taker pays
// BTC); a BUY (maker gives BTC, wants the asset) is ASSET_TO_BTC (the taker sells it).
func DirectionConsistent(direction uint32, isSell bool) bool {
	if isSell {
		return direction == DirBTCToAsset
	}
	return direction == DirAssetToBTC
}

// LnDirectionConsistent checks that a Lightning offer's ln_direction matches its
// trade direction. Because ln_direction is FLOW-oriented and the maker's side is
// the opposite of the asset-giver, the mapping is the INVERSE of the cross-chain
// case: a SELL (maker gives the asset) is the REVERSE flow (BTC-LN -> asset); a
// BUY (maker acquires the asset) is the NORMAL flow (asset -> BTC-LN).
func LnDirectionConsistent(lnDirection uint32, isSell bool) bool {
	switch lnDirection {
	case LnAssetForBTC, LnAssetLNForBTCLN: // maker acquires the asset -> BUY
		return !isSell
	case LnBTCForAsset, LnBTCLNForAssetLN: // maker gives the asset -> SELL
		return isSell
	}
	return false
}
