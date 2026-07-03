package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLNLegPayLive exercises the NORMAL-direction LN primitive (LNLeg.Pay) end
// to end against TWO live SeqLN-on-Bitcoin nodes joined by a real testnet4
// channel. It is the plugin-free half of the Phase 2 swap: the taker mints a
// BOLT11 on a chosen preimage P (payment_hash = H); the maker pays it through
// clnLNLeg and MUST recover exactly P. This proves the hash-binding guard, the
// pay path, and the revealed-preimage check on real Lightning.
//
// It is env-gated so it never runs in normal CI (no baked sockets/creds):
//
//	SEQLN_SOCK1=<maker lightning-rpc>  \
//	SEQLN_SOCK2=<taker lightning-rpc>  \
//	[SEQLN_PAY_MSAT=1000000]           \
//	go test ./pkg/xchain -run TestLNLegPayLive -v
func TestLNLegPayLive(t *testing.T) {
	sock1 := os.Getenv("SEQLN_SOCK1") // maker (pays)
	sock2 := os.Getenv("SEQLN_SOCK2") // taker (mints invoice, holds P)
	if sock1 == "" || sock2 == "" {
		t.Skip("set SEQLN_SOCK1 and SEQLN_SOCK2 (live SeqLN lightning-rpc sockets) to run")
	}
	amtMsat := uint64(1_000_000) // 1000 sat default; keep it small vs channel balance
	if v := os.Getenv("SEQLN_PAY_MSAT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			amtMsat = n
		}
	}

	// The taker chooses P (unique per run so reruns don't collide on the invoice
	// label/preimage) and mints an invoice on it -> payment_hash = H.
	var p [32]byte
	nonce := uint64(time.Now().UnixNano())
	for i := range p {
		p[i] = byte(0xA0 + i + int(nonce>>(uint(i%8)*8)&0xff))
	}
	h := sha256.Sum256(p[:])
	label := "submarine-normal-live-" + hex.EncodeToString(h[:6])

	taker := &lnRPC{socketPath: sock2, timeout: 60 * time.Second}
	var inv struct {
		Bolt11      string `json:"bolt11"`
		PaymentHash string `json:"payment_hash"`
	}
	err := taker.call(&inv, "invoice", map[string]interface{}{
		"amount_msat": amtMsat,
		"label":       label,
		"description": "phase2 normal submarine LN leg (live test)",
		"preimage":    hex.EncodeToString(p[:]),
	})
	if err != nil {
		t.Fatalf("taker mint invoice: %v", err)
	}
	if !hexEq(inv.PaymentHash, h[:]) {
		t.Fatalf("minted invoice payment_hash %s != H %x", inv.PaymentHash, h[:])
	}
	t.Logf("taker minted BOLT11 on H=%x (%d msat)", h[:], amtMsat)

	// The maker pays it through the code under test and must recover P.
	maker := NewCLNLNLeg(sock1)
	got, err := maker.Pay(inv.Bolt11, h[:], amtMsat)
	if err != nil {
		t.Fatalf("maker Pay: %v", err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(p[:]) {
		t.Fatalf("recovered preimage %x != chosen P %x", got, p[:])
	}
	t.Logf("maker recovered P=%x via clnLNLeg.Pay -> NORMAL LN leg validated on live testnet4", got)

	// The hash guard must refuse an invoice not tied to the swap H. (The invoice
	// is already paid/settled above, so this fails at the hash check well before
	// any second payment could be attempted.)
	if _, err := maker.Pay(inv.Bolt11, []byte{0x00, 0x01, 0x02}, amtMsat); err == nil {
		t.Fatal("Pay must refuse when wantHash != invoice payment_hash")
	}
}
