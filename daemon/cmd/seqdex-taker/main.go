// Command seqdex-taker exercises the Sequentia-adapted pkg/trade taker SDK
// against a running tdexd trade interface, performing a same-chain atomic swap.
//
// It creates a fresh taker wallet (its Sequentia confidential address is
// printed so the node can fund it), then on the second invocation runs
// Preview + Buy/SellAndComplete and prints the resulting swap txid.
//
// Secrets (the taker's private + blinding keys) are persisted to / loaded from
// KEY_FILE so the two phases share the same wallet without putting key material
// on the command line.
//
// Env:
//
//	PHASE       = "addr"  -> create wallet, write keys to KEY_FILE, print address
//	            = "swap"  -> load wallet from KEY_FILE, Preview + (Buy|Sell)AndComplete
//	KEY_FILE    path to the taker key file
//	TRADE_ADDR  daemon trade endpoint host:port (default localhost:9945)
//	NODE_RPC    elements node RPC url http://user:pass@host:port
//	NATIVE      native/policy asset hex (pegged_asset)
//	BASE,QUOTE  market assets (hex)
//	TYPE        "buy" or "sell"
//	AMOUNT      amount (in sats) of QUOTE asset to buy/sell (TDEX amount is in quote)
//	IN_ASSET    asset the taker spends/funds (hex) — used only for logging
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/aejkcs50/seqdex/daemon/pkg/explorer/elements"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/aejkcs50/seqdex/daemon/pkg/trade"
	tradeclient "github.com/aejkcs50/seqdex/daemon/pkg/trade/client"
	trademarket "github.com/aejkcs50/seqdex/daemon/pkg/trade/market"
	tradetype "github.com/aejkcs50/seqdex/daemon/pkg/trade/type"
)

func main() {
	phase := os.Getenv("PHASE")
	keyFile := mustEnv("KEY_FILE")
	native := mustEnv("NATIVE")

	// Sequentia regtest network with the runtime native asset stamped on.
	net := seqnet.SequentiaRegtest
	net.AssetID = native

	switch phase {
	case "addr":
		w, err := trade.NewRandomWallet(&net)
		must(err, "NewRandomWallet")
		// Persist keys (hex, one per line: priv, blinding).
		data := hex.EncodeToString(w.PrivateKey()) + "\n" + hex.EncodeToString(w.BlindingKey()) + "\n"
		must(os.WriteFile(keyFile, []byte(data), 0600), "write keyfile")
		fmt.Println(w.Address())
	case "swap":
		priv, blind := loadKeys(keyFile)
		w := trade.NewWalletFromKey(priv, blind, &net)

		nodeRPC := mustEnv("NODE_RPC")
		tradeAddr := envDefault("TRADE_ADDR", "localhost:9945")
		base := mustEnv("BASE")
		quote := mustEnv("QUOTE")
		typ := envDefault("TYPE", "buy")
		// ASSET is the asset whose AMOUNT is specified (must be base or quote);
		// defaults to quote (the classic TDEX convention).
		assetParam := envDefault("ASSET", quote)
		// FEE_ASSET defaults to base (native SEQ, the only asset with an on-node
		// exchange rate).
		feeAsset := envDefault("FEE_ASSET", base)
		amount, err := strconv.ParseUint(mustEnv("AMOUNT"), 10, 64)
		must(err, "parse AMOUNT")

		// Explorer: the elements node, with the Sequentia regtest network so
		// the taker's own confidential-address UTXO lookups resolve.
		expl, err := elements.NewServiceWithNetwork(nodeRPC, nil, &net)
		must(err, "explorer")

		host, portStr := splitHostPort(tradeAddr)
		port, _ := strconv.Atoi(portStr)
		client, err := tradeclient.NewTradeClient(host, port)
		must(err, "trade client")

		t, err := trade.NewTrade(trade.NewTradeOpts{
			Chain:           seqnet.Regtest,
			NativeAsset:     native,
			ExplorerService: expl,
			Client:          client,
		})
		must(err, "NewTrade")

		mkt := trademarket.Market{BaseAsset: base, QuoteAsset: quote}

		// Determine the tradeType + the "asset" param. TDEX convention: amount
		// & asset describe the QUOTE leg.
		var tt tradetype.TradeType
		if typ == "buy" {
			tt = tradetype.Buy
		} else {
			tt = tradetype.Sell
		}

		// Preview first.
		preview, err := t.Preview(trade.PreviewOpts{
			Market:    mkt,
			TradeType: int(tt),
			Amount:    amount,
			Asset:     assetParam,
			FeeAsset:  feeAsset,
		})
		must(err, "Preview")
		fmt.Println("PREVIEW:")
		printJSON(map[string]interface{}{
			"asset_to_send":     preview.AssetToSend,
			"amount_to_send":    preview.AmountToSend,
			"asset_to_receive":  preview.AssetToReceive,
			"amount_to_receive": preview.AmountToReceive,
			"fee_asset":         preview.FeeAsset,
			"fee_amount":        preview.FeeAmount,
		})

		opts := trade.BuyOrSellAndCompleteOpts{
			Market:      mkt,
			Amount:      amount,
			Asset:       assetParam,
			PrivateKey:  priv,
			BlindingKey: blind,
			FeeAsset:    feeAsset,
		}

		var txid string
		if tt.IsBuy() {
			txid, err = t.BuyAndComplete(opts)
		} else {
			txid, err = t.SellAndComplete(opts)
		}
		must(err, "Buy/SellAndComplete")
		fmt.Println("SWAP_TXID=" + txid)
		_ = w
	default:
		fmt.Fprintln(os.Stderr, "unknown PHASE:", phase)
		os.Exit(2)
	}
}

func loadKeys(path string) ([]byte, []byte) {
	raw, err := os.ReadFile(path)
	must(err, "read keyfile")
	var priv, blind string
	fmt.Sscanf(string(raw), "%s\n%s", &priv, &blind)
	p, err := hex.DecodeString(priv)
	must(err, "decode priv")
	b, err := hex.DecodeString(blind)
	must(err, "decode blind")
	return p, b
}

func splitHostPort(s string) (string, string) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func printJSON(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Fprintln(os.Stderr, "missing env:", k)
		os.Exit(1)
	}
	return v
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error, ctx string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", ctx, err)
		os.Exit(1)
	}
}
