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

func TestSettledTradeSignVerifyRoundTrip(t *testing.T) {
	k := newKey(t)
	st := &seqobv1.SettledTrade{
		OfferId:     "00112233445566778899aabbccddeeff",
		FillBase:    100,
		SettleTxid:  "deadbeef",
		AnchorConfs: 1,
		Nonce:       7,
	}
	if err := SignSettledTrade(st, k); err != nil {
		t.Fatalf("SignSettledTrade: %v", err)
	}
	if st.MakerPubkey != hex.EncodeToString(k.PubKey().SerializeCompressed()) {
		t.Fatalf("maker_pubkey not set from key")
	}
	if err := VerifySettledTrade(st); err != nil {
		t.Fatalf("VerifySettledTrade on freshly-signed record: %v", err)
	}
	// Every signed field is authenticated: mutating any one invalidates the signature.
	for name, mut := range map[string]func(*seqobv1.SettledTrade){
		"fill_base":    func(s *seqobv1.SettledTrade) { s.FillBase = 99 },
		"settle_txid":  func(s *seqobv1.SettledTrade) { s.SettleTxid = "cafe" },
		"anchor_confs": func(s *seqobv1.SettledTrade) { s.AnchorConfs = 2 },
		"nonce":        func(s *seqobv1.SettledTrade) { s.Nonce = 8 },
		"offer_id":     func(s *seqobv1.SettledTrade) { s.OfferId = "ffff" },
	} {
		c := proto.Clone(st).(*seqobv1.SettledTrade)
		mut(c)
		if err := VerifySettledTrade(c); err == nil {
			t.Fatalf("expected verification failure after mutating %s", name)
		}
	}
}

func TestSettledTradeWrongKeyRejected(t *testing.T) {
	k := newKey(t)
	other := newKey(t)
	st := &seqobv1.SettledTrade{OfferId: "aaaa", FillBase: 10, Nonce: 1}
	if err := SignSettledTrade(st, k); err != nil {
		t.Fatalf("SignSettledTrade: %v", err)
	}
	// A record claiming a different maker_pubkey than the signer must not verify.
	st.MakerPubkey = hex.EncodeToString(other.PubKey().SerializeCompressed())
	if err := VerifySettledTrade(st); err == nil {
		t.Fatalf("expected verification failure for a mismatched maker_pubkey")
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

func sampleCovenantOffer() *seqobv1.Offer {
	o := sampleOffer()
	o.Settlement = &seqobv1.Offer_Covenant{Covenant: &seqobv1.CovenantTerms{
		CovenantTxid:   "ab12cd34",
		CovenantVout:   1,
		AssetA:         "gold",
		AssetB:         "usdx",
		RateNum:        3,
		RateDen:        7,
		MakerProg:      []byte{0x11, 0x11},
		MakerProgVer:   1,
		MinLot:         5,
		ExpiryLocktime: 400,
		MakerX:         []byte{0x22, 0x22},
		InternalKey:    []byte{0x50, 0x92},
		MerklePath:     [][]byte{{0xaa, 0xbb}},
	}}
	return o
}

// TestCovenantOutpointRewriteKeepsSignatureValid is the partial-fill re-rest
// regression: the watcher rewrites covenant_txid/covenant_vout while the maker
// is offline, so the signature must survive an outpoint rewrite (the outpoint
// is excluded from the canonical bytes; funding is authenticated by spk
// re-derivation + gettxout, not by the signature).
func TestCovenantOutpointRewriteKeepsSignatureValid(t *testing.T) {
	k := newKey(t)
	o := sampleCovenantOffer()
	if err := SignOffer(o, k); err != nil {
		t.Fatalf("SignOffer: %v", err)
	}
	if err := VerifyOffer(o); err != nil {
		t.Fatalf("VerifyOffer on freshly-signed covenant offer: %v", err)
	}

	// Simulate the watcher's re-rest rewrite: new outpoint, same signature.
	rerested := cloneOffer(o)
	rerested.GetCovenant().CovenantTxid = "ffee9988"
	rerested.GetCovenant().CovenantVout = 3
	if err := VerifyOffer(rerested); err != nil {
		t.Fatalf("signature must survive a watcher outpoint rewrite: %v", err)
	}

	// The canonical bytes are outpoint-independent by construction.
	b1, err := CanonicalOfferBytes(o)
	if err != nil {
		t.Fatalf("CanonicalOfferBytes: %v", err)
	}
	b2, err := CanonicalOfferBytes(rerested)
	if err != nil {
		t.Fatalf("CanonicalOfferBytes: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("canonical bytes changed when only the covenant outpoint changed")
	}

	// Every OTHER covenant field stays load-bearing in the signature.
	for name, mut := range map[string]func(*seqobv1.CovenantTerms){
		"rate_num":        func(c *seqobv1.CovenantTerms) { c.RateNum = 5 },
		"asset_b":         func(c *seqobv1.CovenantTerms) { c.AssetB = "eurx" },
		"maker_prog":      func(c *seqobv1.CovenantTerms) { c.MakerProg = []byte{0xde, 0xad} },
		"expiry_locktime": func(c *seqobv1.CovenantTerms) { c.ExpiryLocktime = 999 },
		"merkle_path":     func(c *seqobv1.CovenantTerms) { c.MerklePath = [][]byte{{0xcc}} },
	} {
		tampered := cloneOffer(o)
		mut(tampered.GetCovenant())
		if err := VerifyOffer(tampered); err == nil {
			t.Fatalf("expected verification failure after mutating covenant %s", name)
		}
	}
}

// TestNonCovenantCanonicalBytesUnchanged pins that for a non-covenant offer the
// canonical bytes are still exactly "deterministic marshal with only maker_sig
// cleared" — i.e. the covenant-outpoint exclusion touches nothing else.
func TestNonCovenantCanonicalBytesUnchanged(t *testing.T) {
	o := sampleOffer()
	o.MakerSig = []byte{0x01, 0x02}
	got, err := CanonicalOfferBytes(o)
	if err != nil {
		t.Fatalf("CanonicalOfferBytes: %v", err)
	}
	pre := cloneOffer(o)
	pre.MakerSig = nil
	want, err := deterministic(pre)
	if err != nil {
		t.Fatalf("deterministic: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("non-covenant canonical bytes drifted from the pre-change form (maker_sig-cleared deterministic marshal)")
	}
}

func cloneOffer(o *seqobv1.Offer) *seqobv1.Offer {
	return proto.Clone(o).(*seqobv1.Offer)
}
