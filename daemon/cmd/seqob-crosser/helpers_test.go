package main

// helpers_test.go — offer construction for the pure-logic tests. Offers are
// built exactly as the maker fleet builds them (same field population per
// family) and, where a test exercises BuildUnion's signature gate, signed with a
// real key via offer.SignOffer.

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

const (
	tBase  = "aa11" // display asset id stand-ins (Normalize treats them opaquely)
	tQuote = "bb22"
)

type offerOpt func(*seqobv1.Offer)

func withMinFill(v uint64) offerOpt   { return func(o *seqobv1.Offer) { o.MinFill = v } }
func withNoPartial() offerOpt         { return func(o *seqobv1.Offer) { o.AllowPartial = false } }
func withCreatedAt(t uint64) offerOpt { return func(o *seqobv1.Offer) { o.CreatedAtUnix = t } }
func withExpiry(t uint64) offerOpt    { return func(o *seqobv1.Offer) { o.ExpiresAtUnix = t } }
func withMinLot(v uint64) offerOpt {
	return func(o *seqobv1.Offer) { o.GetCovenant().MinLot = v }
}

var offerSeq int

// mkOffer builds one offer of `fam` on base/quote: an ask sells baseAmt of base
// for quoteAmt of quote; a bid is the mirror. Follows the maker fleet's field
// population (TradeDir + offer/want + base_amount + family-consistent
// direction fields).
func mkOffer(fam Family, side Side, base, quote string, baseAmt, quoteAmt uint64, opts ...offerOpt) *seqobv1.Offer {
	offerSeq++
	o := &seqobv1.Offer{
		OfferId:       fmt.Sprintf("o%03d", offerSeq),
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: base, QuoteAsset: quote},
		BaseAmount:    baseAmt,
		AllowPartial:  true,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(time.Hour).Unix()),
		MakerPubkey:   fmt.Sprintf("maker-%03d", offerSeq),
	}
	if side == SideAsk {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = base, baseAmt
		o.WantAsset, o.WantAmount = quote, quoteAmt
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = quote, quoteAmt
		o.WantAsset, o.WantAmount = base, baseAmt
	}
	switch fam {
	case FamSameChain:
		o.Settlement = &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}}
	case FamCovenant:
		o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
			RateNum: quoteAmt, RateDen: baseAmt, MinLot: 1,
		}}
	case FamCross:
		dir := uint32(0) // BTC_TO_ASSET: the maker gives the asset (ask)
		if side == SideBid {
			dir = 1
		}
		o.Settlement = &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{Direction: dir}}
	case FamSubmarine:
		ld := offer.LnBTCForAsset // maker gives the asset (ask)
		if side == SideBid {
			ld = offer.LnAssetForBTC
		}
		o.Settlement = &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{LnDirection: ld}}
	case FamPureLN:
		ld := offer.LnBTCLNForAssetLN
		if side == SideBid {
			ld = offer.LnAssetLNForBTCLN
		}
		o.Settlement = &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{LnDirection: ld}}
	case FamSubAsset:
		ld := offer.LnAssetLNForBTC
		if side == SideBid {
			ld = offer.LnBTCForAssetLN
		}
		o.Settlement = &seqobv1.Offer_Lightning{Lightning: &seqobv1.LightningTerms{LnDirection: ld}}
	}
	for _, f := range opts {
		f(o)
	}
	return o
}

// signOffer re-keys and really signs an offer so offer.VerifyOffer passes.
func signOffer(o *seqobv1.Offer) *seqobv1.Offer {
	k, err := btcec.NewPrivateKey()
	if err != nil {
		panic(err)
	}
	o.MakerPubkey = "" // let SignOffer stamp the real key
	if err := offer.SignOffer(o, k); err != nil {
		panic(err)
	}
	return o
}

// norm normalizes an offer for the detect-level tests, failing loudly on a skip.
func norm(o *seqobv1.Offer, base, quote string) *NormOrder {
	n, why := Normalize(o, "http://relay.test", base, quote)
	if n == nil {
		panic("test offer did not normalize: " + why)
	}
	return n
}

// execAll marks every family executable (detect-level tests that are not about
// executability).
func execAll(*NormOrder) (bool, string) { return true, "" }

func noLog(string, ...interface{}) {}
