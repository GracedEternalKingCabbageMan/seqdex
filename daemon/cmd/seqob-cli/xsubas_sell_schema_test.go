package main

// xsubas_sell_schema_test.go pins the crash-recovery state-file JSON CONTRACT that the LSP's
// recoverSubasSell() reads. If a field is renamed here without updating the LSP (or vice-versa),
// recovery would silently fail and a settled swap's preimage could be lost — so this test guards
// the exact key names + the H = SHA256(P) invariant, and that save() is atomic (no torn/.tmp file).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestXSubAsSellStateSchema(t *testing.T) {
	p := sha256.Sum256([]byte("schema-preimage"))
	h := sha256.Sum256(p[:])
	st := &xsubasSellState{
		CreatedAt:     "2026-07-18T00:00:00Z",
		UpdatedAt:     "2026-07-18T00:00:01Z",
		Phase:         "paid",
		Asset:         "abc123",
		HashH:         hex.EncodeToString(h[:]),
		Preimage:      hex.EncodeToString(p[:]),
		MakerLNNodeID: "02deadbeef",
		BtcHtlc: &xsubasSellBtcHtlc{
			Txid:              "cafef00d",
			Vout:              1,
			Amount:            2000,
			RedeemScript:      "51",
			TBtc:              200,
			TakerClaimPubkey:  "02aaaa",
			MakerRefundPubkey: "02bbbb",
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "xsubas-sell-nonce.json")
	if err := st.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Atomic save must leave the final file and NO leftover temp file.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("atomic save left a .tmp file (err=%v)", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("state file is not valid json: %v", err)
	}

	// Top-level keys the LSP reads.
	for _, k := range []string{"hash_h", "preimage", "maker_ln_node_id", "btc_htlc"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("state file missing top-level key %q; LSP recovery reads it. keys=%v", k, keysOf(m))
		}
	}
	bh, ok := m["btc_htlc"].(map[string]interface{})
	if !ok {
		t.Fatalf("btc_htlc is not an object")
	}
	// btc_htlc keys must MATCH the CLI success-JSON btc_htlc block (the wallet claims from it).
	for _, k := range []string{"txid", "vout", "amount", "redeem_script", "t_btc", "taker_claim_pubkey", "maker_refund_pubkey"} {
		if _, ok := bh[k]; !ok {
			t.Fatalf("btc_htlc missing key %q; the wallet needs it to claim the BTC. keys=%v", k, keysOf(bh))
		}
	}

	// The recovery invariant: the persisted preimage must open the persisted hashlock.
	preHex, _ := m["preimage"].(string)
	hHex, _ := m["hash_h"].(string)
	preB, err := hex.DecodeString(preHex)
	if err != nil {
		t.Fatalf("preimage not hex: %v", err)
	}
	got := sha256.Sum256(preB)
	if hex.EncodeToString(got[:]) != hHex {
		t.Fatalf("SHA256(preimage) %s != hash_h %s", hex.EncodeToString(got[:]), hHex)
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
