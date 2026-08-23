package main

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// A refund key must be rebuildable from the identity key alone: the same identity
// and purpose always give the same key, different purposes give different keys,
// and none of them is the identity key itself.
func TestDerivedRefundKeyIsStable(t *testing.T) {
	id, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	a := derivedRefundKey(id, "submarine-reverse")
	b := derivedRefundKey(id, "submarine-reverse")
	c := derivedRefundKey(id, "subasset-sell")
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("derivation is not deterministic")
	}
	if bytes.Equal(a.Bytes(), c.Bytes()) {
		t.Fatal("different purposes must not share a key")
	}
	if bytes.Equal(a.Bytes(), id.Serialize()) {
		t.Fatal("the refund key must not be the identity key")
	}
	other, _ := btcec.NewPrivateKey()
	if bytes.Equal(derivedRefundKey(other, "submarine-reverse").Bytes(), a.Bytes()) {
		t.Fatal("different identities must not share a key")
	}
}
