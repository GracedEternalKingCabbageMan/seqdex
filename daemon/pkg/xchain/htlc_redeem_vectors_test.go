package xchain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// htlc_redeem_vectors_test.go — the CANONICAL golden vectors for the Design-A
// HTLC redeemScript (HashLock.LockScript / BitcoinLeg.HTLCScript). They exist to
// pin the EXACT bytes Go emits for a (hash_h, claim_pub, refund_pub, t_btc)
// tuple, so a second implementation — notably the LSP's JS buildHtlcRedeem
// (tooling/lsp/bridge-maker.mjs), which PRE-COMPUTES the redeemScript it is about
// to fund and then verify-not-trusts the funded script equals it — can be tested
// to byte-match Go WITHOUT a live node. If the JS ever diverges from Go, the
// payer leg-bridge would broadcast the BTC funding and THEN throw at the
// script-equality gate (fund-loss window); this fixture is the cross-language
// contract that prevents that.
//
// The tuples INCLUDE locktime edge values that exercise btcd's ScriptBuilder.
// AddInt64 encoding boundaries — most importantly 16 (emitted as the single
// opcode OP_16, NOT a data push) vs 17 (a 1-byte CScriptNum push), plus the
// CScriptNum sign-byte boundary at 0x7f/0x80, a 2-byte value (0xffff), and the
// largest value below the nLockTime height/timestamp threshold (500000000-1).
// A JS encoder that always data-pushes would silently disagree at 16.
//
// The fixture pkg/xchain/testdata/htlc_redeem_vectors.json is COMMITTED and is
// the artifact the LSP test loads. Regenerate it (only when the inputs below
// change) with:  UPDATE_HTLC_VECTORS=1 go test ./pkg/xchain/ -run GoldenHTLCRedeemVectors

const htlcRedeemVectorsPath = "testdata/htlc_redeem_vectors.json"

// htlcRedeemVector is one committed row. Field names match the JSON the LSP test
// loads: {hash_h, claim_pub, refund_pub, t_btc, redeem_hex}.
type htlcRedeemVector struct {
	HashH     string `json:"hash_h"`     // 32-byte hex sha256 image H
	ClaimPub  string `json:"claim_pub"`  // 33-byte compressed hex (IF-branch key)
	RefundPub string `json:"refund_pub"` // 33-byte compressed hex (ELSE/CLTV-branch key)
	TBtc      uint32 `json:"t_btc"`      // CLTV locktime (block height)
	RedeemHex string `json:"redeem_hex"` // canonical Go LockScript output
}

// fixedPub returns the 33-byte compressed pubkey for a fixed 32-byte scalar, so
// the vectors are fully deterministic (no randomness) and reproducible.
func fixedPub(b byte) string {
	scalar := bytes.Repeat([]byte{b}, 32)
	return hex.EncodeToString(KeyFromBytes(scalar).PubKey())
}

// computeHTLCRedeemVectors builds the canonical vector set from FIXED inputs and
// computes each redeem_hex via the real HashLock.LockScript (the exact code the
// BTC leg funds + the maker/verifier reconstructs). Two fixed key/hash sets are
// used so the fixture also varies the pubkeys/hash, not just the locktime.
func computeHTLCRedeemVectors(t *testing.T) []htlcRedeemVector {
	t.Helper()

	// Set 1 — one fixed (hash, claim, refund) swept across the locktime edges.
	hash1 := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	claim1 := fixedPub(0x11)
	refund1 := fixedPub(0x22)
	// Set 2 — distinct keys + hash at a realistic height, so a JS bug tied to a
	// particular key/hash byte pattern is also caught.
	hash2 := hex.EncodeToString(bytes.Repeat([]byte{0x3c}, 32))
	claim2 := fixedPub(0x44)
	refund2 := fixedPub(0x55)

	rows := []htlcRedeemVector{
		// Locktime encoding edges on set 1:
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 16},            // OP_16 (single opcode, NOT a push)
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 17},            // 1-byte CScriptNum push
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 0x7f},          // 127: high bit clear, 1 byte
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 0x80},          // 128: sign byte appended -> 2 bytes
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 0xffff},        // 65535: 2 value bytes + sign byte
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 500000000 - 1}, // largest height below the nLockTime threshold
		{HashH: hash1, ClaimPub: claim1, RefundPub: refund1, TBtc: 44300},         // a realistic Sequentia hard-fork-era height
		// Set 2 at a realistic mainnet-scale BTC height:
		{HashH: hash2, ClaimPub: claim2, RefundPub: refund2, TBtc: 850000},
	}

	for i := range rows {
		r := &rows[i]
		hashB, err := hex.DecodeString(r.HashH)
		if err != nil || len(hashB) != 32 {
			t.Fatalf("vector %d: bad hash_h %q", i, r.HashH)
		}
		claimB, err := hex.DecodeString(r.ClaimPub)
		if err != nil || len(claimB) != 33 {
			t.Fatalf("vector %d: bad claim_pub %q", i, r.ClaimPub)
		}
		refundB, err := hex.DecodeString(r.RefundPub)
		if err != nil || len(refundB) != 33 {
			t.Fatalf("vector %d: bad refund_pub %q", i, r.RefundPub)
		}
		script, err := NewHashLockFromHash(hashB).LockScript(claimB, refundB, r.TBtc)
		if err != nil {
			t.Fatalf("vector %d: LockScript(t=%d): %v", i, r.TBtc, err)
		}
		r.RedeemHex = hex.EncodeToString(script)
	}
	return rows
}

// TestGoldenHTLCRedeemVectors is the golden test: it recomputes each committed
// vector's redeemScript from the committed inputs and asserts the committed
// redeem_hex is EXACTLY Go's LockScript output. That makes the fixture
// self-verifying (the LSP test then only has to match these committed bytes).
// Under UPDATE_HTLC_VECTORS=1 it (re)writes the fixture instead.
func TestGoldenHTLCRedeemVectors(t *testing.T) {
	want := computeHTLCRedeemVectors(t)

	if os.Getenv("UPDATE_HTLC_VECTORS") == "1" {
		if err := os.MkdirAll(filepath.Dir(htlcRedeemVectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, '\n')
		if err := os.WriteFile(htlcRedeemVectorsPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d golden HTLC-redeem vectors to %s", len(want), htlcRedeemVectorsPath)
		return
	}

	raw, err := os.ReadFile(htlcRedeemVectorsPath)
	if err != nil {
		t.Fatalf("read golden fixture %s (regenerate with UPDATE_HTLC_VECTORS=1): %v", htlcRedeemVectorsPath, err)
	}
	var got []htlcRedeemVector
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse golden fixture %s: %v", htlcRedeemVectorsPath, err)
	}
	if len(got) != len(want) {
		t.Fatalf("golden fixture has %d vectors, generator produces %d — regenerate with UPDATE_HTLC_VECTORS=1", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.HashH != w.HashH || g.ClaimPub != w.ClaimPub || g.RefundPub != w.RefundPub || g.TBtc != w.TBtc {
			t.Fatalf("vector %d inputs drifted from the fixture (want %+v, fixture %+v) — regenerate with UPDATE_HTLC_VECTORS=1", i, w, g)
		}
		// The committed hex must equal what Go computes now (guards against a hand-edit).
		if g.RedeemHex != w.RedeemHex {
			t.Fatalf("vector %d (t_btc=%d): fixture redeem_hex %s != Go LockScript output %s", i, w.TBtc, g.RedeemHex, w.RedeemHex)
		}
		// And the committed hex must genuinely parse back to the same inputs via a
		// tokenizer, so a JS byte-match against it is a match against a real HTLC.
		assertRedeemBindsInputs(t, i, g)
	}
}

// assertRedeemBindsInputs re-parses a committed redeem_hex and confirms it embeds
// exactly the vector's H, claim pub, and refund pub — a defence-in-depth check
// that the fixture rows are internally consistent HTLCs, not just opaque bytes.
func assertRedeemBindsInputs(t *testing.T, i int, v htlcRedeemVector) {
	t.Helper()
	redeem, err := hex.DecodeString(v.RedeemHex)
	if err != nil {
		t.Fatalf("vector %d: redeem_hex not hex: %v", i, err)
	}
	hashB, _ := hex.DecodeString(v.HashH)
	if !scriptEmbedsHash(redeem, hashB) {
		t.Fatalf("vector %d: redeem script does not embed H=%s", i, v.HashH)
	}
	claimB, _ := hex.DecodeString(v.ClaimPub)
	refundB, _ := hex.DecodeString(v.RefundPub)
	if !bytes.Contains(redeem, claimB) {
		t.Fatalf("vector %d: redeem script does not embed claim pub", i)
	}
	if !bytes.Contains(redeem, refundB) {
		t.Fatalf("vector %d: redeem script does not embed refund pub", i)
	}
}
