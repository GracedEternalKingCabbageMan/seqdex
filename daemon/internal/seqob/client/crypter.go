// Package client provides the maker + taker helpers that drive a SeqOB lift to
// settlement. The settlement itself REUSES the proven seqdex same-chain PSET
// co-sign (pkg/swap.{Request,Accept,Complete} + wallet.Service.CompleteSwap);
// nothing here rebuilds it.
//
// Because the relay courier is opaque and end-to-end encrypted (review B1), this
// package owns the E2E crypto: each peer derives a shared key by ECDH between its
// ephemeral session key and the peer's, and seals the inner swap message with
// AES-256-GCM. The relay only ever sees ciphertext.
package client

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

// Crypter seals/opens the inner swap payload for one session peer. The symmetric
// key is sha256(ECDH(myPriv, peerPub)) over secp256k1 (btcec, the repo's curve
// library). Both peers derive the same key because ECDH is symmetric.
type Crypter struct {
	aead cipher.AEAD
}

// NewCrypter derives the session AEAD from this peer's private session key and
// the counterparty's public session key.
func NewCrypter(myPriv *btcec.PrivateKey, peerPub *btcec.PublicKey) (*Crypter, error) {
	if myPriv == nil || peerPub == nil {
		return nil, errors.New("nil session key")
	}
	secret := btcec.GenerateSharedSecret(myPriv, peerPub)
	key := sha256.Sum256(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Crypter{aead: aead}, nil
}

// Seal encrypts plaintext, returning nonce||ciphertext.
func (c *Crypter) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts nonce||ciphertext produced by Seal under the matching key.
func (c *Crypter) Open(sealed []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return c.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
}

// NewMakerCrypterFromLift derives a maker's per-lift E2E crypter from the maker's
// offer private key (its key doubles as its session key) and the taker's session
// pubkey, as delivered in From.lift_requested. The maker then drives the existing
// Maker.HandleRequest to open the taker's sealed SwapRequest and seal its accept.
//
// ITEM C (relay-MITM re-attack: VERIFIED FALSE POSITIVE; DoS only, NOT a CT leak).
// The taker side already derives its key from the SIGNED, VERIFIED offer's maker
// pubkey (round-1 fix in cmd/seqob-cli), not from the relay echo, so the taker
// leg is leak-proof. The remaining concern raised was that the maker still trusts
// the relay-supplied takerSessionPubkey here. That is denial-of-service, not a
// confidentiality leak, for a structural reason: a Crypter holds ONE symmetric
// key per peer used for BOTH Open and Seal (see Crypter above). The taker sealed
// its SwapRequest under the key derived from the REAL maker+taker pubkeys, so the
// maker must OPEN that ciphertext under the matching key BEFORE it ever produces a
// SwapAccept (see Maker.HandleRequest). If a malicious relay substitutes its own
// pubkey for takerSessionPubkey, the maker derives the WRONG key, the AES-GCM Open
// fails, and NO SwapAccept is ever sealed; so there is nothing to leak. The relay
// also holds no private key, so it cannot itself decrypt the taker->maker leg to
// mount a man-in-the-middle. Net effect of substitution: the lift simply fails.
// No functional change is required for this item.
func NewMakerCrypterFromLift(makerOfferPriv *btcec.PrivateKey, takerSessionPubkey []byte) (*Crypter, error) {
	if makerOfferPriv == nil {
		return nil, errors.New("nil maker key")
	}
	pub, err := btcec.ParsePubKey(takerSessionPubkey)
	if err != nil {
		return nil, err
	}
	return NewCrypter(makerOfferPriv, pub)
}
