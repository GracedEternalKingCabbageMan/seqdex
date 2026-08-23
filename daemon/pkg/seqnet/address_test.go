package seqnet

import (
	"encoding/hex"
	"testing"

	"github.com/vulpemventures/go-elements/address"
)

// Sequentia is transparent by default: a maker account hands the swap a bech32
// address, and the swap must decode it as readily as a blinded one.
func TestFromAnyDecodesBothForms(t *testing.T) {
	net := SequentiaTestnet
	prog := make([]byte, 20)
	for i := range prog {
		prog[i] = byte(i + 1)
	}
	transparent, err := address.ToBech32(&address.Bech32{Prefix: net.Bech32, Version: 0, Program: prog})
	if err != nil {
		t.Fatal(err)
	}
	info, err := FromAny(transparent, &net)
	if err != nil {
		t.Fatalf("transparent: %v", err)
	}
	if hex.EncodeToString(info.Script) != "0014"+hex.EncodeToString(prog) {
		t.Fatalf("transparent script = %x", info.Script)
	}
	if len(info.BlindingKey) != 0 {
		t.Fatal("a transparent address carries no blinding key")
	}
	if _, err := FromConfidential(transparent, &net); err == nil {
		t.Fatal("FromConfidential must refuse a transparent address (the old decoder returned nil and the swap dereferenced it)")
	}

	blindKey := make([]byte, 33)
	blindKey[0] = 0x02
	blindKey[32] = 0x01
	conf, err := address.ToBlech32(&address.Blech32{Prefix: net.Blech32, Version: 0, Program: prog, PublicKey: blindKey})
	if err != nil {
		t.Fatal(err)
	}
	info, err = FromAny(conf, &net)
	if err != nil {
		t.Fatalf("confidential: %v", err)
	}
	if hex.EncodeToString(info.Script) != "0014"+hex.EncodeToString(prog) || hex.EncodeToString(info.BlindingKey) != hex.EncodeToString(blindKey) {
		t.Fatalf("confidential decode wrong: script=%x key=%x", info.Script, info.BlindingKey)
	}
}
