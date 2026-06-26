// Package offer provides canonical serialization, signing, and verification of
// SeqOB order-book offers and cancels.
//
// An Offer is a signed price/size/expiry/keys intent (never a PSET, never named
// UTXOs). The authoritative exchange ratio is want_amount/offer_amount (both
// integers); there is deliberately no floating-point price in the signed bytes
// (review m1), so the canonical encoding is stable across the Go relay and any
// other client.
//
// Canonical bytes = deterministic protobuf encoding of every field EXCLUDING
// maker_sig, which by construction includes the whole `settlement` oneof, so
// every current and future settlement variant is authenticated. We sha256 those
// bytes and sign/verify with secp256k1 (btcec, the same library the rest of the
// repo uses: see pkg/xchain/keys.go and pkg/trade/wallet.go) against the 33-byte
// compressed maker_pubkey.
package offer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// deterministic marshals m with the deterministic option so the same logical
// message always produces the same bytes (stable field ordering, no map jitter).
func deterministic(m proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// CanonicalOfferBytes returns the deterministic encoding of o with maker_sig
// cleared. This is the exact byte string the maker signs and verifiers re-derive.
func CanonicalOfferBytes(o *seqobv1.Offer) ([]byte, error) {
	if o == nil {
		return nil, errors.New("nil offer")
	}
	c := proto.Clone(o).(*seqobv1.Offer)
	c.MakerSig = nil
	return deterministic(c)
}

// OfferHash is sha256 over the canonical offer bytes.
func OfferHash(o *seqobv1.Offer) ([32]byte, error) {
	b, err := CanonicalOfferBytes(o)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// SignOffer fills o.MakerSig with a DER secp256k1 signature over OfferHash(o)
// using priv. If o.MakerPubkey is empty it is set from priv; if it is already
// set it must match priv's compressed public key.
func SignOffer(o *seqobv1.Offer, priv *btcec.PrivateKey) error {
	if o == nil {
		return errors.New("nil offer")
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	if o.MakerPubkey == "" {
		o.MakerPubkey = pubHex
	} else if !strings.EqualFold(o.MakerPubkey, pubHex) {
		return fmt.Errorf("maker_pubkey %q does not match signing key %q", o.MakerPubkey, pubHex)
	}
	h, err := OfferHash(o)
	if err != nil {
		return err
	}
	o.MakerSig = ecdsa.Sign(priv, h[:]).Serialize()
	return nil
}

// VerifyOffer checks o.MakerSig against o.MakerPubkey over OfferHash(o).
func VerifyOffer(o *seqobv1.Offer) error {
	if o == nil {
		return errors.New("nil offer")
	}
	if len(o.MakerSig) == 0 {
		return errors.New("missing maker_sig")
	}
	pub, err := parsePubkeyHex(o.MakerPubkey)
	if err != nil {
		return err
	}
	sig, err := ecdsa.ParseDERSignature(o.MakerSig)
	if err != nil {
		return fmt.Errorf("bad maker_sig encoding: %w", err)
	}
	h, err := OfferHash(o)
	if err != nil {
		return err
	}
	if !sig.Verify(h[:], pub) {
		return errors.New("maker_sig verification failed")
	}
	return nil
}

// CanonicalCancelBytes returns the deterministic encoding of c with sig cleared.
// The signed payload is {offer_id, maker_pubkey, nonce}; the nonce defeats replay
// of an old cancel against a re-posted same offer_id (review m2).
func CanonicalCancelBytes(c *seqobv1.OfferCancel) ([]byte, error) {
	if c == nil {
		return nil, errors.New("nil cancel")
	}
	cc := proto.Clone(c).(*seqobv1.OfferCancel)
	cc.Sig = nil
	return deterministic(cc)
}

// CancelHash is sha256 over the canonical cancel bytes.
func CancelHash(c *seqobv1.OfferCancel) ([32]byte, error) {
	b, err := CanonicalCancelBytes(c)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// SignCancel fills c.Sig with a DER secp256k1 signature over CancelHash(c).
// As with SignOffer, c.MakerPubkey is set from priv if empty, else must match.
func SignCancel(c *seqobv1.OfferCancel, priv *btcec.PrivateKey) error {
	if c == nil {
		return errors.New("nil cancel")
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	if c.MakerPubkey == "" {
		c.MakerPubkey = pubHex
	} else if !strings.EqualFold(c.MakerPubkey, pubHex) {
		return fmt.Errorf("maker_pubkey %q does not match signing key %q", c.MakerPubkey, pubHex)
	}
	h, err := CancelHash(c)
	if err != nil {
		return err
	}
	c.Sig = ecdsa.Sign(priv, h[:]).Serialize()
	return nil
}

// VerifyCancel checks c.Sig against c.MakerPubkey over CancelHash(c).
func VerifyCancel(c *seqobv1.OfferCancel) error {
	if c == nil {
		return errors.New("nil cancel")
	}
	if len(c.Sig) == 0 {
		return errors.New("missing cancel sig")
	}
	pub, err := parsePubkeyHex(c.MakerPubkey)
	if err != nil {
		return err
	}
	sig, err := ecdsa.ParseDERSignature(c.Sig)
	if err != nil {
		return fmt.Errorf("bad cancel sig encoding: %w", err)
	}
	h, err := CancelHash(c)
	if err != nil {
		return err
	}
	if !sig.Verify(h[:], pub) {
		return errors.New("cancel sig verification failed")
	}
	return nil
}

func parsePubkeyHex(s string) (*btcec.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bad maker_pubkey hex: %w", err)
	}
	pub, err := btcec.ParsePubKey(b)
	if err != nil {
		return nil, fmt.Errorf("bad maker_pubkey: %w", err)
	}
	return pub, nil
}
