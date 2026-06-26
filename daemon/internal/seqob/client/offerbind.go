package client

import (
	"fmt"
	"math/big"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// ValidateRequestAgainstOffer binds a decrypted taker SwapRequest to the maker's
// OWN signed offer before the maker co-signs, so a malicious taker cannot drain
// the maker at an arbitrary price or in unexpected assets.
//
// The taker is the proposer: it PAYS asset_p (the maker's want_asset) and
// RECEIVES asset_r (the maker's offer_asset). For a fill of amount_r of the
// offer_asset, the maker must be paid at least the offered ratio:
//
//	amount_p / amount_r >= want_amount / offer_amount
//
// which, cleared of division and evaluated with math/big to avoid uint64
// overflow, is amount_p * offer_amount >= want_amount * amount_r. Pro-rata fills
// round the pay leg up (the taker pays the ceil), so this lower bound is exactly
// "the ratio matches within integer rounding" from the maker's side; overpayment
// only benefits the maker and is allowed. The fill may not exceed the offered
// amount (partial fills allowed).
func ValidateRequestAgainstOffer(req *seqdexv1.SwapRequest, o *seqobv1.Offer) error {
	if req == nil || o == nil {
		return fmt.Errorf("nil request or offer")
	}
	if req.GetAssetP() != o.GetWantAsset() {
		return fmt.Errorf("asset_p %q != offer want_asset %q", req.GetAssetP(), o.GetWantAsset())
	}
	if req.GetAssetR() != o.GetOfferAsset() {
		return fmt.Errorf("asset_r %q != offer offer_asset %q", req.GetAssetR(), o.GetOfferAsset())
	}
	amountP := req.GetAmountP()
	amountR := req.GetAmountR()
	if amountP == 0 || amountR == 0 {
		return fmt.Errorf("amounts must be > 0 (amount_p=%d amount_r=%d)", amountP, amountR)
	}
	if amountR > o.GetOfferAmount() {
		return fmt.Errorf("amount_r %d exceeds offered amount %d", amountR, o.GetOfferAmount())
	}
	// Price floor: amount_p * offer_amount >= want_amount * amount_r.
	lhs := new(big.Int).Mul(new(big.Int).SetUint64(amountP), new(big.Int).SetUint64(o.GetOfferAmount()))
	rhs := new(big.Int).Mul(new(big.Int).SetUint64(o.GetWantAmount()), new(big.Int).SetUint64(amountR))
	if lhs.Cmp(rhs) < 0 {
		return fmt.Errorf(
			"underpays: amount_p %d for amount_r %d is below the offer ratio (want %d / offer %d)",
			amountP, amountR, o.GetWantAmount(), o.GetOfferAmount(),
		)
	}
	return nil
}
