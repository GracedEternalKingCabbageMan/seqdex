package client

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"google.golang.org/protobuf/proto"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

func newKeyPair(t *testing.T) (*btcec.PrivateKey, *btcec.PrivateKey) {
	t.Helper()
	a, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func mustSeal(t *testing.T, c *Crypter, m proto.Message) []byte {
	t.Helper()
	pt, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// bindOffer is a SELL of 100 gold for 45 usdx (the same shape used elsewhere in
// the client tests). The taker pays usdx (want_asset) and receives gold
// (offer_asset).
func bindOffer() *seqobv1.Offer {
	return &seqobv1.Offer{
		OfferId:     "aaaa",
		Pair:        &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:    seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:  100,
		OfferAmount: 100,
		OfferAsset:  "gold",
		WantAmount:  45,
		WantAsset:   "usdx",
	}
}

func req(assetP string, amountP uint64, assetR string, amountR uint64) *seqdexv1.SwapRequest {
	return &seqdexv1.SwapRequest{AssetP: assetP, AmountP: amountP, AssetR: assetR, AmountR: amountR}
}

func TestValidateRequestAgainstOffer(t *testing.T) {
	o := bindOffer()
	cases := []struct {
		name string
		req  *seqdexv1.SwapRequest
		ok   bool
	}{
		// Full fill at the offered price: 45 usdx for 100 gold.
		{"full fill exact", req("usdx", 45, "gold", 100), true},
		// Partial fill: 50 gold needs ceil(45*50/100)=23 usdx.
		{"partial fill ceil", req("usdx", 23, "gold", 50), true},
		// Overpaying only benefits the maker.
		{"overpay allowed", req("usdx", 50, "gold", 100), true},
		// Underpaying the offered ratio (drain attempt) is rejected.
		{"underpay rejected", req("usdx", 22, "gold", 50), false},
		{"full fill underpay", req("usdx", 44, "gold", 100), false},
		// Taking more than offered is rejected.
		{"over-take rejected", req("usdx", 90, "gold", 200), false},
		// Wrong assets are rejected (asset substitution).
		{"wrong pay asset", req("silvr", 45, "gold", 100), false},
		{"wrong recv asset", req("usdx", 45, "silvr", 100), false},
		// Zero amounts are rejected.
		{"zero amounts", req("usdx", 0, "gold", 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRequestAgainstOffer(tc.req, o)
			if tc.ok && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
}

// TestMakerHandleRequestRejectsDrain proves the maker refuses to co-sign a
// request that underpays its own offer, before the wallet is ever touched.
func TestMakerHandleRequestRejectsDrain(t *testing.T) {
	makerKey, takerKey := newKeyPair(t)
	mc, _ := NewCrypter(makerKey, takerKey.PubKey())
	tc, _ := NewCrypter(takerKey, makerKey.PubKey())

	maker := &Maker{Wallet: &StubWallet{Name: "maker"}, Offer: bindOffer()}

	// A taker sealing a request that drains the maker (1 usdx for all 100 gold).
	drain := req("usdx", 1, "gold", 100)
	sealed := mustSeal(t, tc, drain)
	if _, err := maker.HandleRequest(sealed, mc); err == nil {
		t.Fatalf("maker must reject a draining request")
	}

	// A faithful request at the offered price is accepted.
	fair := req("usdx", 45, "gold", 100)
	fair.Id = "ok"
	sealed = mustSeal(t, tc, fair)
	if _, err := maker.HandleRequest(sealed, mc); err != nil {
		t.Fatalf("maker must accept a faithful request: %v", err)
	}
}
