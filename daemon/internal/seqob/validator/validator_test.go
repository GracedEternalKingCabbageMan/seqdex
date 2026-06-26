package validator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

var now = time.Unix(1750000000, 0)

func cfg() Config {
	c := DefaultConfig()
	c.Now = func() time.Time { return now }
	return c
}

func signed(t *testing.T, k *btcec.PrivateKey, mut func(*seqobv1.Offer)) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       "aaaa",
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: uint64(now.Add(time.Hour).Unix()),
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if mut != nil {
		mut(o)
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return o
}

func key(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestValidOfferPasses(t *testing.T) {
	v := New(cfg(), nil)
	o := signed(t, key(t), nil)
	if err := v.ValidateOffer(context.Background(), o, "1.2.3.4"); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*seqobv1.Offer)
	}{
		{"zero amount", func(o *seqobv1.Offer) { o.OfferAmount = 0 }},
		{"wrong dir mapping", func(o *seqobv1.Offer) { o.WantAsset = "gold" }},
		{"identical assets", func(o *seqobv1.Offer) { o.Pair.QuoteAsset = "gold" }},
		{"no expiry", func(o *seqobv1.Offer) { o.ExpiresAtUnix = 0 }},
		{"expiry too far", func(o *seqobv1.Offer) { o.ExpiresAtUnix = uint64(now.Add(100 * 24 * time.Hour).Unix()) }},
		{"already expired", func(o *seqobv1.Offer) { o.ExpiresAtUnix = uint64(now.Add(-time.Hour).Unix()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := New(cfg(), nil)
			o := signed(t, key(t), tc.mut)
			if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestBadSignatureRejected(t *testing.T) {
	v := New(cfg(), nil)
	o := signed(t, key(t), nil)
	o.WantAmount = 1 // breaks the signature
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected signature rejection")
	}
}

func TestUnknownAssetRejected(t *testing.T) {
	c := cfg()
	c.KnownAssets = map[string]bool{"gold": true, "usdx": true}
	v := New(c, nil)
	o := signed(t, key(t), func(o *seqobv1.Offer) {
		o.Pair.QuoteAsset = "silvr"
		o.WantAsset = "silvr"
	})
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected unknown-asset rejection")
	}
}

func TestRateLimitPerPubkey(t *testing.T) {
	c := cfg()
	c.MaxOffersPerMinPerPubkey = 2
	c.MaxOffersPerMinPerIP = 0
	v := New(c, nil)
	k := key(t)
	for i := 0; i < 2; i++ {
		o := signed(t, k, func(o *seqobv1.Offer) { o.OfferId = "id" })
		if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
			t.Fatalf("offer %d should pass: %v", i, err)
		}
	}
	o := signed(t, k, nil)
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected rate-limit rejection on 3rd offer")
	}
}

func TestMinAnchorDepthNotFloored(t *testing.T) {
	// Policy override: 0-conf tolerant. min_anchor_depth=0 must be accepted.
	v := New(cfg(), nil)
	o := signed(t, key(t), func(o *seqobv1.Offer) { o.MinAnchorDepth = 0 })
	if err := v.ValidateOffer(context.Background(), o, ""); err != nil {
		t.Fatalf("min_anchor_depth=0 must be accepted, got %v", err)
	}
}

type failProbe struct{}

func (failProbe) CheckOffer(context.Context, *seqobv1.Offer) error {
	return errors.New("no funds")
}

func TestLivenessProbeWired(t *testing.T) {
	v := New(cfg(), failProbe{})
	o := signed(t, key(t), nil)
	if err := v.ValidateOffer(context.Background(), o, ""); err == nil {
		t.Fatalf("expected liveness-probe rejection")
	}
}
