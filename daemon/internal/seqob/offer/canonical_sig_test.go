package offer

import (
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// derParts splits a DER ECDSA signature into its r and s integers.
func derParts(t *testing.T, der []byte) (r, s *big.Int) {
	t.Helper()
	if len(der) < 8 || der[0] != 0x30 || der[2] != 0x02 {
		t.Fatalf("not a DER signature: %x", der)
	}
	rl := int(der[3])
	r = new(big.Int).SetBytes(der[4 : 4+rl])
	if der[4+rl] != 0x02 {
		t.Fatalf("bad DER s tag: %x", der)
	}
	sl := int(der[5+rl])
	s = new(big.Int).SetBytes(der[6+rl : 6+rl+sl])
	return r, s
}

// derEncode builds a minimal DER signature from r and s.
func derEncode(r, s *big.Int) []byte {
	enc := func(v *big.Int) []byte {
		b := v.Bytes()
		if len(b) == 0 || b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return append([]byte{0x02, byte(len(b))}, b...)
	}
	body := append(enc(r), enc(s)...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

// malleate returns the (r, N-s) twin of a DER signature: a different byte string
// that verifies against the same key and hash under plain ECDSA.
func malleate(t *testing.T, der []byte) []byte {
	t.Helper()
	r, s := derParts(t, der)
	return derEncode(r, new(big.Int).Sub(btcec.S256().N, s))
}

// A malleated twin of a public signature must not pass as a fresh, different
// signature: the relay's replay gate and edit ownership key on the signature bytes.
func TestNonCanonicalOfferSignatureRejected(t *testing.T) {
	k := newKey(t)
	o := sampleOffer()
	if err := SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	twin := malleate(t, o.MakerSig)
	if string(twin) == string(o.MakerSig) {
		t.Fatal("malleated signature should differ from the original")
	}
	o.MakerSig = twin
	if err := VerifyOffer(o); err == nil {
		t.Fatal("high-S twin of a valid signature verified; signatures must be canonical")
	}
}

func TestNonCanonicalCancelSettledTradeReattachRejected(t *testing.T) {
	k := newKey(t)
	c := &seqobv1.OfferCancel{OfferId: "aaaa", Nonce: 7}
	if err := SignCancel(c, k); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCancel(c); err != nil {
		t.Fatalf("canonical cancel must verify: %v", err)
	}
	c.Sig = malleate(t, c.Sig)
	if err := VerifyCancel(c); err == nil {
		t.Fatal("high-S cancel signature verified")
	}

	st := &seqobv1.SettledTrade{OfferId: "aaaa", FillBase: 10, Nonce: 1}
	if err := SignSettledTrade(st, k); err != nil {
		t.Fatal(err)
	}
	st.Sig = malleate(t, st.Sig)
	if err := VerifySettledTrade(st); err == nil {
		t.Fatal("high-S settled_trade signature verified")
	}

	pub := k.PubKey().SerializeCompressed()
	sig := SignReattach("sid", "maker", k)
	if err := VerifyReattach("sid", "maker", pub, sig); err != nil {
		t.Fatalf("canonical reattach sig must verify: %v", err)
	}
	if err := VerifyReattach("sid", "maker", pub, malleate(t, sig)); err == nil {
		t.Fatal("high-S reattach signature verified")
	}
}
