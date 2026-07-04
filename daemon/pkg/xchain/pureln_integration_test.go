package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestPureLNBuyLive proves the pure-LN "buy the asset with BTC" swap end to end:
// two Lightning legs stitched by one shared secret, NO on-chain leg, NO anchor
// gate. It runs the maker and taker concurrently against two live SeqLN nodes.
//
// For a self-contained regtest proof the "BTC" leg is a second Sequentia asset
// (the mechanism is network-agnostic; the real global Bitcoin-LN leg is M5).
//
//	SEQLN_TAKER_SOCK    = taker node lightning-rpc (receives asset, pays BTC)
//	SEQLN_MAKER_SOCK    = maker node lightning-rpc (pays asset, receives+holds BTC;
//	                      MUST have the holdinvoice-seq plugin loaded)
//	SEQLN_ASSET_ID      = 32-byte hex asset id (e.g. GOLD), maker -> taker
//	SEQLN_BTC_ASSET_ID  = 32-byte hex asset id used as the BTC stand-in, taker -> maker
//	SEQLN_ASSET_AMT_MSAT = asset amount   (default 100000000 = 0.001)
//	SEQLN_BTC_AMT_MSAT   = "BTC" amount   (default 200000000 = 0.002; an arbitrary rate)
//
//	go test ./pkg/xchain -run TestPureLNBuyLive -v
func TestPureLNBuyLive(t *testing.T) {
	takerSock := os.Getenv("SEQLN_TAKER_SOCK")
	makerSock := os.Getenv("SEQLN_MAKER_SOCK")
	assetID := os.Getenv("SEQLN_ASSET_ID")
	btcID := os.Getenv("SEQLN_BTC_ASSET_ID")
	if takerSock == "" || makerSock == "" || assetID == "" || btcID == "" {
		t.Skip("set SEQLN_TAKER_SOCK, SEQLN_MAKER_SOCK, SEQLN_ASSET_ID, SEQLN_BTC_ASSET_ID")
	}
	assetAmt := envU64("SEQLN_ASSET_AMT_MSAT", 100000000)
	btcAmt := envU64("SEQLN_BTC_AMT_MSAT", 200000000)

	// The taker's secret P (and its hash H, shared across both legs).
	var p [32]byte
	copy(p[:], []byte("pureln-buy-secret-32bytes-exactly")) // copy takes the first 32
	hExp := sha256.Sum256(p[:])

	maker := NewPureLNSwap(NewCLNAssetLNLeg(makerSock, assetID), NewCLNAssetLNLeg(makerSock, btcID))
	taker := NewPureLNSwap(NewCLNAssetLNLeg(takerSock, assetID), NewCLNAssetLNLeg(takerSock, btcID))

	// Taker issues the asset invoice on P; the maker will pay it.
	assetInv, h, err := taker.PrepareTakerBuy(p[:], assetAmt)
	if err != nil {
		t.Fatalf("PrepareTakerBuy: %v", err)
	}
	if hex.EncodeToString(h) != hex.EncodeToString(hExp[:]) {
		t.Fatalf("h mismatch")
	}

	// Maker registers the incoming BTC hold on H BEFORE the taker pays it.
	if err := maker.MakerRegisterHold(h, btcAmt); err != nil {
		t.Fatalf("MakerRegisterHold: %v", err)
	}
	makerBTCID, err := maker.btcLeg.NodeID()
	if err != nil {
		t.Fatalf("maker btc NodeID: %v", err)
	}

	start := time.Now()

	// Maker fulfills concurrently (waits for the held BTC, pays the asset, settles).
	type res struct {
		p   []byte
		err error
	}
	makerDone := make(chan res, 1)
	go func() {
		mp, err := maker.MakerFulfill(h, assetInv, assetAmt, 60*time.Second)
		makerDone <- res{mp, err}
	}()

	// Taker pays the maker's BTC hold by hash; blocks until the maker settles.
	secret := make([]byte, 32)
	secret[0] = 0x5a
	takerP, err := taker.RunTakerBuy(h, makerBTCID, btcAmt, 18, secret)
	if err != nil {
		t.Fatalf("RunTakerBuy: %v", err)
	}
	mr := <-makerDone
	if mr.err != nil {
		t.Fatalf("MakerFulfill: %v", mr.err)
	}
	elapsed := time.Since(start)

	// Both sides ended on the SAME preimage the taker chose.
	if hex.EncodeToString(takerP) != hex.EncodeToString(p[:]) {
		t.Fatalf("taker preimage %x != chosen %x", takerP, p[:])
	}
	if hex.EncodeToString(mr.p) != hex.EncodeToString(p[:]) {
		t.Fatalf("maker preimage %x != chosen %x", mr.p, p[:])
	}
	t.Logf("pure-LN buy settled in %s: taker paid %d msat on the BTC leg, received %d msat on the asset leg; one shared preimage, NO on-chain tx / NO anchor wait",
		elapsed, btcAmt, assetAmt)
}

func envU64(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
