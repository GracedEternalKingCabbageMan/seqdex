package main

// xsubas-fund-btc: the DEVICE side of a fully non-custodial sub-asset buy — build,
// sign, and broadcast the BTC on-chain HTLC from the USER's OWN Bitcoin, so the LSP
// never fronts the BTC. In the browser this is btc.js signing with the device key;
// this CLI reproduces it for headless/box proofs.
//
// It builds the Design-A HTLC (claim = the maker's stable claim pubkey with P;
// refund = a device refund key after T_btc) on hash H, funds it from -btc-wallet
// (the user's bitcoind wallet), broadcasts it, and prints the HTLC params the
// wallet passes to POST /swap PLUS the device refund PRIVATE key (kept device-side,
// used only to reclaim the HTLC at CLTV if the maker never pays).
//
//   seqob-cli xsubas-fund-btc -maker-claim-pub <hex> -hash <H hex> -btc-amount <sats>
//     -btc-locktime <T_btc> -btc-rpc <url> -btc-wallet <user wallet> -btc-chain testnet4
//     [-refund-priv <hex>]   (a device refund key is generated + printed if omitted)

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSubAsFundBtc(args []string) {
	fs := newFlagSet("xsubas-fund-btc")
	makerClaimPubHex := fs.String("maker-claim-pub", "", "the maker's STABLE BTC claim pubkey (hex) from the offer's LightningTerms.maker_claim_pub (required)")
	hashHex := fs.String("hash", "", "the swap hash H (hex) = SHA256(device preimage); the HTLC is locked on it (required)")
	btcAmount := fs.Uint64("btc-amount", 0, "BTC sats to lock in the HTLC (required)")
	btcLocktime := fs.Uint("btc-locktime", 0, "T_btc: the CLTV height for the device refund branch (required; = btc_tip + offer OnchainCltv)")
	btcRPCURL := fs.String("btc-rpc", "", "bitcoind RPC URL http://user:pass@host:port (required)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet holding the USER's BTC that funds the HTLC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	refundPrivHex := fs.String("refund-priv", "", "device BTC refund privkey (32-byte hex); a fresh one is generated + printed if empty")
	feeRate := fs.Float64("btc-fee-rate", 2, "sat/vB fee rate for funding the HTLC (0 = node default)")
	_ = fs.Parse(args)

	if *makerClaimPubHex == "" || *hashHex == "" || *btcAmount == 0 || *btcLocktime == 0 || *btcRPCURL == "" {
		fatal("xsubas-fund-btc requires -maker-claim-pub, -hash, -btc-amount, -btc-locktime, -btc-rpc")
	}
	makerClaimPub, err := hex.DecodeString(*makerClaimPubHex)
	if err != nil || len(makerClaimPub) == 0 {
		fatal("bad -maker-claim-pub")
	}
	hashH, err := hex.DecodeString(*hashHex)
	if err != nil || len(hashH) != 32 {
		fatal("-hash must be 32-byte hex H")
	}
	btcRPC, err := xliftRPCFromURL(*btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(*btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
	if *feeRate > 0 {
		btcChain.SetFeeRate(*feeRate)
	}
	// The device refund key: generated + printed if not supplied. The wallet keeps
	// the PRIVATE key (only it can reclaim the HTLC at CLTV) — the LSP never sees it.
	var refundKey *xchain.Key
	if *refundPrivHex != "" {
		kb, derr := hex.DecodeString(*refundPrivHex)
		if derr != nil || len(kb) != 32 {
			fatal("-refund-priv must be 32-byte hex")
		}
		refundKey = xchain.KeyFromBytes(kb)
	} else {
		refundKey, err = xchain.NewKey()
		if err != nil {
			fatal("mint refund key: %v", err)
		}
	}

	// Build the HTLC on H (claim=maker with P, refund=device after T_btc) and fund it
	// from the USER's wallet (device-signed). This is the on-chain payment the user
	// makes; the maker claims it later with the revealed preimage.
	swap := xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(hashH))
	leg, _, err := swap.LockBTCLeg(makerClaimPub, refundKey.PubKey(), atomsToCoinsCli(*btcAmount), uint32(*btcLocktime))
	if err != nil {
		fatal("fund BTC HTLC from the user's wallet: %v", err)
	}
	out := map[string]interface{}{
		"btc_htlc_txid":   leg.Funded.TxID,
		"btc_htlc_vout":   leg.Funded.Vout,
		"btc_htlc_amount": leg.Funded.Amount,
		"btc_htlc_script": hex.EncodeToString(leg.Script),
		"btc_locktime":    leg.Locktime,
		"btc_refund_pub":  hex.EncodeToString(refundKey.PubKey()),
		"btc_refund_priv": hex.EncodeToString(refundKey.Bytes()), // DEVICE-HELD; never send to the LSP
		"hash_h":          hex.EncodeToString(hashH),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// atomsToCoinsCli renders sats as an 8-decimal coin string for the wallet-funding RPC.
func atomsToCoinsCli(a uint64) string { return fmt.Sprintf("%d.%08d", a/100_000_000, a%100_000_000) }
