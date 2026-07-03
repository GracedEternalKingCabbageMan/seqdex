package xchain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSubmarineRunNormalLive drives a FULL NORMAL submarine swap end to end:
// the Sequentia asset leg on the anchored two-chain regtest, and the BTC leg on
// REAL testnet4 Lightning. It proves the whole SubmarineSwap.RunNormal path,
// including the net-new anchor-DEPTH gate (VerifySeqAnchorBuried) which is
// demonstrated deterministically by burying the funding's anchor under parent
// (Bitcoin-stand-in) blocks.
//
// Preconditions (all env-gated; the test SKIPS otherwise, so normal CI stays
// green and no creds/sockets are baked in):
//
//	SEQDEX_XCHAIN_DIR   = the running regtest base dir (parent+seq already up,
//	                      e.g. via testdata/start-regtest.sh up)
//	SEQLN_SOCK1         = maker's SeqLN-on-Bitcoin lightning-rpc (pays)
//	SEQLN_SOCK2         = taker's SeqLN-on-Bitcoin lightning-rpc (mints the invoice)
//	[SEQDEX_XCHAIN_SEQ_RPC=18001] [SEQDEX_XCHAIN_PARENT_RPC=18000]
//
// Flow: the taker mints a BOLT11 on a chosen preimage P (payment_hash = H) and
// funds the SEQ HTLC (claim=maker). The maker verifies the SEQ leg, is shown to
// REFUSE while the anchor is shallow, waits until it is buried >= MinAnchorDepth,
// then pays the BOLT11 (learning P) and claims the SEQ asset with P.
func TestSubmarineRunNormalLive(t *testing.T) {
	base := os.Getenv("SEQDEX_XCHAIN_DIR")
	sock1 := os.Getenv("SEQLN_SOCK1")
	sock2 := os.Getenv("SEQLN_SOCK2")
	if base == "" || sock1 == "" || sock2 == "" {
		t.Skip("set SEQDEX_XCHAIN_DIR (regtest up) + SEQLN_SOCK1 + SEQLN_SOCK2 to run")
	}
	seqPort := envInt("SEQDEX_XCHAIN_SEQ_RPC", 18001)
	parentPort := envInt("SEQDEX_XCHAIN_PARENT_RPC", 18000)

	seqUser, seqPass := readCookie(t, filepath.Join(base, "seq"))
	parUser, parPass := readCookie(t, filepath.Join(base, "parent"))
	seqRPC := NewRPC("127.0.0.1", seqPort, seqUser, seqPass)
	parRPC := NewRPC("127.0.0.1", parentPort, parUser, parPass)
	ensureWallet(t, seqRPC)
	ensureWallet(t, parRPC)
	seq := NewChain(seqRPC, "w")
	parent := NewChain(parRPC, "w")

	// Warm up: fund wallets, sync the anchor watcher, issue the asset.
	if err := parent.Mine(20); err != nil {
		t.Fatalf("parent mine: %v", err)
	}
	if err := seq.Mine(110); err != nil {
		t.Fatalf("seq mine: %v", err)
	}
	waitAnchorOKLocal(t, seq)
	asset := issueAssetLocal(t, seqRPC)
	mustCall(t, "setfeeexchangerates", seqRPC.WithWallet("w").Call(nil,
		"setfeeexchangerates", map[string]interface{}{asset: 100000000}))
	if err := seq.Mine(1); err != nil {
		t.Fatalf("seq mine asset: %v", err)
	}
	t.Logf("SEQ asset issued: %s", asset)

	// The taker chooses P; both legs are gated by H = SHA256(P). The maker knows
	// only H (NewHashLockFromHash) and must LEARN P by paying the invoice.
	p := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		t.Fatal(err)
	}
	hArr := sha256.Sum256(p)
	h := hArr[:]
	prim := NewHashLockFromHash(h)
	t.Logf("swap H = %s (maker does NOT know P yet)", hex.EncodeToString(h))

	m := NewSubmarineSwap(seq, NewCLNLNLeg(sock1), prim)

	makerClaimSEQ := mustKeyLocal(t)  // maker claims the SEQ asset with P
	takerRefundSEQ := mustKeyLocal(t) // taker refunds the SEQ HTLC after CLTV
	seqHeight, _ := seq.BlockCount()
	seqLocktime := uint32(seqHeight + 50)

	// The taker funds the SEQ HTLC (claim=maker). LockSEQLeg funds+confirms it.
	seqLeg, seqBlock, err := m.LockSEQLeg(makerClaimSEQ.PubKey(), takerRefundSEQ.PubKey(), "1000", asset, seqLocktime)
	if err != nil {
		t.Fatalf("fund SEQ HTLC: %v", err)
	}
	t.Logf("SEQ HTLC funded: txid=%s vout=%d in block %s (%d atoms)",
		seqLeg.Funded.TxID, seqLeg.Funded.Vout, seqBlock, seqLeg.Funded.Amount)

	const minAnchorDepth = int64(3)

	// NEGATIVE: right after funding the anchor is shallow — the gate MUST refuse.
	shallow, gerr := m.VerifySeqAnchorBuried(seqBlock, minAnchorDepth)
	if gerr == nil || !errors.Is(gerr, ErrAnchorOrdering) {
		t.Fatalf("anchor gate must REFUSE a shallow anchor (depth %d < %d), got err=%v ev=%+v",
			shallow.AnchorDepth, minAnchorDepth, gerr, shallow)
	}
	t.Logf("anchor gate correctly REFUSED shallow funding: depth=%d < min=%d", shallow.AnchorDepth, minAnchorDepth)

	// The taker mints the BOLT11 on a chosen preimage P (payment_hash = H).
	bolt11 := mintInvoice(t, sock2, p, 1_000_000, "submarine-runnormal-"+hex.EncodeToString(h[:6]))

	// BURY the anchor: advance the parent (Bitcoin stand-in) chain so the SEQ
	// funding's anchor is buried >= minAnchorDepth, then confirm the gate passes.
	buryAnchor(t, parent, seq, m, seqBlock, minAnchorDepth)

	// Run the maker side. The gate is already satisfied, so it pays quickly,
	// learns P, and claims the SEQ asset.
	res, err := m.RunNormal(NormalParams{
		HashH:             h,
		MakerSeqClaimPub:  makerClaimSEQ.PubKey(),
		TakerSeqRefundPub: takerRefundSEQ.PubKey(),
		SeqRedeemScript:   seqLeg.Script,
		SeqLocktime:       seqLocktime,
		SeqTxID:           seqLeg.Funded.TxID,
		SeqVout:           seqLeg.Funded.Vout,
		SeqAmountAtoms:    seqLeg.Funded.Amount,
		SeqAssetID:        "", // amount+script+P2SH already pin the leg; skip asset-id form check
		SeqMinConf:        1,
		Bolt11:            bolt11,
		InvoiceMsat:       1_000_000,
		MinAnchorDepth:    minAnchorDepth,
		AnchorTimeout:     60 * time.Second,
	}, makerClaimSEQ, 100000)
	if err != nil {
		t.Fatalf("RunNormal: %v (anchor=%+v)", err, res.Anchor)
	}

	// The maker must have recovered exactly P and claimed the SEQ asset.
	if hex.EncodeToString(res.Preimage) != hex.EncodeToString(p) {
		t.Fatalf("recovered preimage %x != chosen P %x", res.Preimage, p)
	}
	if res.SeqClaimTxID == "" {
		t.Fatal("expected a SEQ claim txid")
	}
	t.Logf("RunNormal OK: paid LN, recovered P=%x, claimed SEQ asset in %s (anchor depth=%d)",
		res.Preimage, res.SeqClaimTxID, res.Anchor.AnchorDepth)

	// The preimage is now on-chain in the maker's SEQ claim scriptSig.
	if err := seq.Mine(1); err != nil {
		t.Fatalf("mine SEQ claim: %v", err)
	}
	found, asm, err := seq.RedeemScriptSigContains(res.SeqClaimTxID, hex.EncodeToString(p))
	if err != nil {
		t.Fatalf("read preimage from SEQ claim: %v", err)
	}
	if !found {
		t.Fatalf("preimage not found in SEQ claim scriptSig: %s", asm)
	}
	if c, _ := seq.Confirmations(res.SeqClaimTxID); c < 1 {
		t.Fatalf("SEQ claim not confirmed (confs=%d)", c)
	}
	t.Logf("NORMAL submarine swap COMPLETE: SEQ asset claim %s confirmed; same P settled the testnet4 LN leg.", res.SeqClaimTxID)
}

// --- local helpers (internal package: cannot use the xchain_test harness) ---

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func readCookie(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "elementsregtest", ".cookie")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cookie %s: %v", path, err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed cookie %s", path)
	}
	return parts[0], parts[1]
}

func ensureWallet(t *testing.T, rpc *RPC) {
	t.Helper()
	// Ignore errors: the wallet may already exist/be loaded.
	_ = rpc.Call(nil, "createwallet", "w")
	_ = rpc.Call(nil, "loadwallet", "w")
}

func mustCall(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func mustKeyLocal(t *testing.T) *Key {
	t.Helper()
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func waitAnchorOKLocal(t *testing.T, seq *Chain) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, err := seq.GetAnchorStatus()
		if err == nil && s.AnchorStatus == "ok" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("anchor status did not reach ok within timeout")
}

func issueAssetLocal(t *testing.T, rpc *RPC) string {
	t.Helper()
	var res struct {
		Asset string `json:"asset"`
	}
	if err := rpc.WithWallet("w").Call(&res, "issueasset", "100000", "0"); err != nil {
		t.Fatalf("issueasset: %v", err)
	}
	return res.Asset
}

// mintInvoice creates a plain (non-hold) BOLT11 on the taker node bound to the
// chosen preimage p, so paying it reveals p. Returns the bolt11 string.
func mintInvoice(t *testing.T, sock string, preimage []byte, amountMsat uint64, label string) string {
	t.Helper()
	taker := &lnRPC{socketPath: sock, timeout: 60 * time.Second}
	var inv struct {
		Bolt11      string `json:"bolt11"`
		PaymentHash string `json:"payment_hash"`
	}
	err := taker.call(&inv, "invoice", map[string]interface{}{
		"amount_msat": amountMsat,
		"label":       label,
		"description": "phase2 NORMAL submarine (SEQ asset -> BTC-LN) live test",
		"preimage":    hex.EncodeToString(preimage),
	})
	if err != nil {
		t.Fatalf("taker mint invoice: %v", err)
	}
	want := sha256.Sum256(preimage)
	if !hexEq(inv.PaymentHash, want[:]) {
		t.Fatalf("minted invoice payment_hash %s != H %x", inv.PaymentHash, want[:])
	}
	t.Logf("taker minted BOLT11 on H=%x (%d msat)", want[:], amountMsat)
	return inv.Bolt11
}

// buryAnchor advances the parent (Bitcoin stand-in) chain and lets the seq node
// re-anchor until the funding block's anchor is buried >= minAnchorDepth, then
// asserts the gate passes.
func buryAnchor(t *testing.T, parent, seq *Chain, m *SubmarineSwap, seqBlock string, minAnchorDepth int64) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if err := parent.Mine(3); err != nil {
			t.Fatalf("parent mine (bury): %v", err)
		}
		time.Sleep(2 * time.Second) // let the anchor poller (interval=1s) see the new parent tip
		if err := seq.Mine(1); err != nil {
			t.Fatalf("seq mine (bury): %v", err)
		}
		ev, err := m.VerifySeqAnchorBuried(seqBlock, minAnchorDepth)
		if err == nil && ev.OK {
			t.Logf("anchor buried: seq-block anchor=%d, node anchor tip=%d, depth=%d >= min=%d",
				ev.SeqBlockAnchor, ev.NodeAnchorHeight, ev.AnchorDepth, minAnchorDepth)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("anchor did not bury to depth %d within timeout (last: %+v, err=%v)", minAnchorDepth, ev, err)
		}
	}
}
