package offer

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

func sampleOffer() *seqobv1.Offer {
	return &seqobv1.Offer{
		OfferId:       "00112233445566778899aabbccddeeff",
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		AllowPartial:  true,
		MinFill:       1,
		CreatedAtUnix: 1750000000,
		ExpiresAtUnix: 1750003600,
		FeeAssetHint:  "gold",
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{
			MakerRecvAddress: "el1qqexampleaddr",
			MakerBlindingPub: "02blindingpub",
		}},
	}
}

func newKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	return k
}

func TestSignVerifyRoundTrip(t *testing.T) {
	k := newKey(t)
	o := sampleOffer()
	if err := SignOffer(o, k); err != nil {
		t.Fatalf("SignOffer: %v", err)
	}
	if o.MakerPubkey != hex.EncodeToString(k.PubKey().SerializeCompressed()) {
		t.Fatalf("maker_pubkey not set from key")
	}
	if len(o.MakerSig) == 0 {
		t.Fatalf("maker_sig empty")
	}
	if err := VerifyOffer(o); err != nil {
		t.Fatalf("VerifyOffer on freshly-signed offer: %v", err)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	k := newKey(t)
	o := sampleOffer()
	if err := SignOffer(o, k); err != nil {
		t.Fatalf("SignOffer: %v", err)
	}

	// Tamper a signed field: change the want amount (the ratio is authoritative).
	tampered := cloneOffer(o)
	tampered.WantAmount = 1
	if err := VerifyOffer(tampered); err == nil {
		t.Fatalf("expected verification failure after tampering want_amount")
	}

	// Tamper inside the settlement oneof (must be covered by the signature).
	tampered2 := cloneOffer(o)
	tampered2.GetSameChain().MakerRecvAddress = "el1qqattacker"
	if err := VerifyOffer(tampered2); err == nil {
		t.Fatalf("expected verification failure after tampering settlement terms")
	}

	// Tamper the signature itself.
	tampered3 := cloneOffer(o)
	tampered3.MakerSig[len(tampered3.MakerSig)-1] ^= 0xff
	if err := VerifyOffer(tampered3); err == nil {
		t.Fatalf("expected verification failure after flipping a sig byte")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	k := newKey(t)
	o := sampleOffer()
	if err := SignOffer(o, k); err != nil {
		t.Fatalf("SignOffer: %v", err)
	}
	// Swap in a different pubkey but keep the signature: must fail.
	other := newKey(t)
	o.MakerPubkey = hex.EncodeToString(other.PubKey().SerializeCompressed())
	if err := VerifyOffer(o); err == nil {
		t.Fatalf("expected verification failure with mismatched pubkey")
	}
}

func TestSignOfferRejectsMismatchedPubkey(t *testing.T) {
	k := newKey(t)
	other := newKey(t)
	o := sampleOffer()
	o.MakerPubkey = hex.EncodeToString(other.PubKey().SerializeCompressed())
	if err := SignOffer(o, k); err == nil {
		t.Fatalf("expected SignOffer to reject a maker_pubkey that is not the signing key")
	}
}

func TestCancelSignVerifyRoundTrip(t *testing.T) {
	k := newKey(t)
	c := &seqobv1.OfferCancel{
		OfferId: "00112233445566778899aabbccddeeff",
		Nonce:   7,
	}
	if err := SignCancel(c, k); err != nil {
		t.Fatalf("SignCancel: %v", err)
	}
	if err := VerifyCancel(c); err != nil {
		t.Fatalf("VerifyCancel: %v", err)
	}
	// Changing the nonce must invalidate the signature (replay defense).
	c.Nonce = 8
	if err := VerifyCancel(c); err == nil {
		t.Fatalf("expected cancel verification failure after changing nonce")
	}
}

func TestCanonicalBytesStable(t *testing.T) {
	o := sampleOffer()
	b1, err := CanonicalOfferBytes(o)
	if err != nil {
		t.Fatalf("CanonicalOfferBytes: %v", err)
	}
	// Setting maker_sig must not change the canonical bytes (it is excluded).
	o.MakerSig = []byte{0x01, 0x02, 0x03}
	b2, err := CanonicalOfferBytes(o)
	if err != nil {
		t.Fatalf("CanonicalOfferBytes: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("canonical bytes changed when maker_sig was set")
	}
}

func cloneOffer(o *seqobv1.Offer) *seqobv1.Offer {
	return proto.Clone(o).(*seqobv1.Offer)
}
