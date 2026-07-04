package xchain

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"
)

// Pure-LN swap live tests (Step 2, M2/M3). Two live SeqLN nodes; for a
// self-contained regtest proof the "BTC" leg is a second Sequentia asset (the
// mechanism is network-agnostic; the real global Bitcoin-LN leg is M5).
//
//	SEQLN_TAKER_SOCK     = taker node lightning-rpc
//	SEQLN_MAKER_SOCK     = maker node lightning-rpc (MUST have holdinvoice-seq)
//	SEQLN_ASSET_ID       = 32-byte hex asset id (e.g. GOLD)
//	SEQLN_BTC_ASSET_ID   = 32-byte hex asset id used as the BTC stand-in
//	SEQLN_ASSET_AMT_MSAT = asset amount (default 100000000 = 0.001)
//	SEQLN_BTC_AMT_MSAT   = "BTC" amount (default 200000000 = 0.002; arbitrary rate)
//
//	go test ./pkg/xchain -run TestPureLN -v
func plnEnv(t *testing.T) (taker, maker, asset, btc string, assetAmt, btcAmt uint64) {
	t.Helper()
	taker = os.Getenv("SEQLN_TAKER_SOCK")
	maker = os.Getenv("SEQLN_MAKER_SOCK")
	asset = os.Getenv("SEQLN_ASSET_ID")
	btc = os.Getenv("SEQLN_BTC_ASSET_ID")
	if taker == "" || maker == "" || asset == "" || btc == "" {
		t.Skip("set SEQLN_TAKER_SOCK, SEQLN_MAKER_SOCK, SEQLN_ASSET_ID, SEQLN_BTC_ASSET_ID")
	}
	return taker, maker, asset, btc, envU64("SEQLN_ASSET_AMT_MSAT", 100000000), envU64("SEQLN_BTC_AMT_MSAT", 200000000)
}

func randPreimage(t *testing.T) []byte {
	t.Helper()
	p := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return p
}

// TestPureLNBuyLive: taker buys the asset with BTC. Maker holds BTC, pays the
// asset. NO on-chain leg, NO anchor gate.
func TestPureLNBuyLive(t *testing.T) {
	takerSock, makerSock, asset, btc, assetAmt, btcAmt := plnEnv(t)
	p := randPreimage(t)

	maker := NewPureLNSwap(NewCLNAssetLNLeg(makerSock, asset), NewCLNAssetLNLeg(makerSock, btc))
	taker := NewPureLNSwap(NewCLNAssetLNLeg(takerSock, asset), NewCLNAssetLNLeg(takerSock, btc))

	assetInv, h, err := taker.PrepareTakerBuy(p, assetAmt)
	if err != nil {
		t.Fatalf("PrepareTakerBuy: %v", err)
	}
	if err := maker.MakerRegisterHold(h, btcAmt); err != nil {
		t.Fatalf("MakerRegisterHold: %v", err)
	}
	makerBTCID, err := maker.btcLeg.NodeID()
	if err != nil {
		t.Fatalf("maker btc NodeID: %v", err)
	}

	start := time.Now()
	makerDone := make(chan result, 1)
	go func() { mp, e := maker.MakerFulfill(h, assetInv, assetAmt, 60*time.Second); makerDone <- result{mp, e} }()

	takerP, err := taker.RunTakerBuy(h, makerBTCID, btcAmt, 18, secret32(0x5a))
	if err != nil {
		t.Fatalf("RunTakerBuy: %v", err)
	}
	mr := <-makerDone
	if mr.err != nil {
		t.Fatalf("MakerFulfill: %v", mr.err)
	}
	assertSameP(t, "buy", takerP, mr.p, p)
	t.Logf("pure-LN BUY settled in %s: taker paid %d msat BTC leg, got %d msat asset leg; one preimage, NO on-chain / NO anchor wait",
		time.Since(start), btcAmt, assetAmt)
}

// TestPureLNSellLive: taker sells the asset for BTC. Maker holds the asset, pays
// BTC. Requires the maker to have BTC-leg outbound to the taker (rebalance the
// BTC stand-in asset toward the maker first, e.g. taker pays the maker once).
func TestPureLNSellLive(t *testing.T) {
	takerSock, makerSock, asset, btc, assetAmt, btcAmt := plnEnv(t)
	p := randPreimage(t)

	maker := NewPureLNSwap(NewCLNAssetLNLeg(makerSock, asset), NewCLNAssetLNLeg(makerSock, btc))
	taker := NewPureLNSwap(NewCLNAssetLNLeg(takerSock, asset), NewCLNAssetLNLeg(takerSock, btc))

	btcInv, h, err := taker.PrepareTakerSell(p, btcAmt)
	if err != nil {
		t.Fatalf("PrepareTakerSell: %v", err)
	}
	if err := maker.MakerRegisterHoldSell(h, assetAmt); err != nil {
		t.Fatalf("MakerRegisterHoldSell: %v", err)
	}
	makerAssetID, err := maker.assetLeg.NodeID()
	if err != nil {
		t.Fatalf("maker asset NodeID: %v", err)
	}

	start := time.Now()
	makerDone := make(chan result, 1)
	go func() { mp, e := maker.MakerFulfillSell(h, btcInv, btcAmt, 60*time.Second); makerDone <- result{mp, e} }()

	takerP, err := taker.RunTakerSell(h, makerAssetID, assetAmt, 18, secret32(0x5b))
	if err != nil {
		t.Fatalf("RunTakerSell: %v", err)
	}
	mr := <-makerDone
	if mr.err != nil {
		t.Fatalf("MakerFulfillSell: %v", mr.err)
	}
	assertSameP(t, "sell", takerP, mr.p, p)
	t.Logf("pure-LN SELL settled in %s: taker paid %d msat asset leg, got %d msat BTC leg; one preimage, NO on-chain / NO anchor wait",
		time.Since(start), assetAmt, btcAmt)
}

// TestPureLNRefundLive: the atomic-refund path. The maker's OUTGOING pay fails
// (the taker's asset invoice is for far more than the maker can route), so the
// maker cancels the BTC hold and the taker's BTC is refunded — neither leg
// completes, no value moves.
func TestPureLNRefundLive(t *testing.T) {
	takerSock, makerSock, asset, btc, _, btcAmt := plnEnv(t)
	p := randPreimage(t)
	hugeAsset := uint64(10_000_000_000_000) // 100 GOLD: unroutable, forces the outgoing pay to fail

	maker := NewPureLNSwap(NewCLNAssetLNLeg(makerSock, asset), NewCLNAssetLNLeg(makerSock, btc))
	taker := NewPureLNSwap(NewCLNAssetLNLeg(takerSock, asset), NewCLNAssetLNLeg(takerSock, btc))

	assetInv, h, err := taker.PrepareTakerBuy(p, hugeAsset)
	if err != nil {
		t.Fatalf("PrepareTakerBuy: %v", err)
	}
	if err := maker.MakerRegisterHold(h, btcAmt); err != nil {
		t.Fatalf("MakerRegisterHold: %v", err)
	}
	makerBTCID, err := maker.btcLeg.NodeID()
	if err != nil {
		t.Fatalf("maker btc NodeID: %v", err)
	}

	makerDone := make(chan result, 1)
	go func() { mp, e := maker.MakerFulfill(h, assetInv, hugeAsset, 60*time.Second); makerDone <- result{mp, e} }()

	// The taker's BTC hold must be FAILED back (refund), so PayHash returns an error.
	_, takerErr := taker.RunTakerBuy(h, makerBTCID, btcAmt, 18, secret32(0x5c))
	mr := <-makerDone

	if mr.err == nil {
		t.Fatalf("expected MakerFulfill to fail (outgoing pay unroutable), got success")
	}
	if takerErr == nil {
		t.Fatalf("expected taker BTC to be refunded (PayHash error), got success — atomicity broken!")
	}
	t.Logf("pure-LN REFUND OK: maker's outgoing pay failed -> hold cancelled -> taker refunded. maker err=%q taker err=%q",
		trunc(mr.err.Error()), trunc(takerErr.Error()))
}

// --- helpers ---

type result struct {
	p   []byte
	err error
}

func secret32(b byte) []byte { s := make([]byte, 32); s[0] = b; return s }

func envU64(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func assertSameP(t *testing.T, dir string, takerP, makerP, chosen []byte) {
	t.Helper()
	if hex.EncodeToString(takerP) != hex.EncodeToString(chosen) {
		t.Fatalf("%s: taker preimage %x != chosen %x", dir, takerP, chosen)
	}
	if hex.EncodeToString(makerP) != hex.EncodeToString(chosen) {
		t.Fatalf("%s: maker preimage %x != chosen %x", dir, makerP, chosen)
	}
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
