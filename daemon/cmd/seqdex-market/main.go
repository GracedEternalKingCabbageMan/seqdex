// Command seqdex-market drives the operator OperatorService over :9000 to
// create / inspect / open a market and to derive its deposit addresses, so a
// same-chain atomic swap can be demonstrated end to end. It is a throwaway
// regtest helper.
//
// Usage (ACTION env):
//
//	ACTION=new       BASE=<hex> QUOTE=<hex>            -> NewMarket (balanced)
//	ACTION=derive    BASE=<hex> QUOTE=<hex> NUM=<n>    -> DeriveMarketAddresses
//	ACTION=open      BASE=<hex> QUOTE=<hex>            -> OpenMarket
//	ACTION=close     BASE=<hex> QUOTE=<hex>            -> CloseMarket
//	ACTION=info      BASE=<hex> QUOTE=<hex>            -> GetMarketInfo (reserves/price/tradable)
//	ACTION=list                                        -> ListMarkets
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	daemonv2 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/tdex-daemon/v2"
	tdexv2 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/tdex/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := envDefault("OPERATOR_ADDR", "localhost:9000")
	action := os.Getenv("ACTION")
	base := os.Getenv("BASE")
	quote := os.Getenv("QUOTE")

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(err, "dial")
	defer conn.Close()
	cli := daemonv2.NewOperatorServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mkt := &tdexv2.Market{BaseAsset: base, QuoteAsset: quote}

	switch action {
	case "new":
		resp, err := cli.NewMarket(ctx, &daemonv2.NewMarketRequest{
			Market:              mkt,
			BaseAssetPrecision:  8,
			QuoteAssetPrecision: 8,
			Name:                "SEQ-ASSETX",
			// StrategyType left UNSPECIFIED -> daemon defaults to BALANCED, so
			// the market is priced by its reserves (no explicit price needed).
		})
		must(err, "NewMarket")
		printJSON(resp)
	case "derive":
		num, _ := strconv.ParseInt(envDefault("NUM", "1"), 10, 64)
		resp, err := cli.DeriveMarketAddresses(ctx, &daemonv2.DeriveMarketAddressesRequest{
			Market:         mkt,
			NumOfAddresses: num,
		})
		must(err, "DeriveMarketAddresses")
		for _, a := range resp.GetAddresses() {
			fmt.Println(a)
		}
	case "open":
		_, err := cli.OpenMarket(ctx, &daemonv2.OpenMarketRequest{Market: mkt})
		must(err, "OpenMarket")
		fmt.Println("market opened")
	case "close":
		_, err := cli.CloseMarket(ctx, &daemonv2.CloseMarketRequest{Market: mkt})
		must(err, "CloseMarket")
		fmt.Println("market closed")
	case "info":
		resp, err := cli.GetMarketInfo(ctx, &daemonv2.GetMarketInfoRequest{Market: mkt})
		must(err, "GetMarketInfo")
		printMarketInfo(resp.GetInfo())
	case "list":
		resp, err := cli.ListMarkets(ctx, &daemonv2.ListMarketsRequest{})
		must(err, "ListMarkets")
		for _, m := range resp.GetMarkets() {
			printMarketInfo(m)
		}
	case "fee":
		num, _ := strconv.ParseInt(envDefault("NUM", "1"), 10, 64)
		resp, err := cli.DeriveFeeAddresses(ctx, &daemonv2.DeriveFeeAddressesRequest{
			NumOfAddresses: num,
		})
		must(err, "DeriveFeeAddresses")
		for _, a := range resp.GetAddresses() {
			fmt.Println(a)
		}
	case "feebalance":
		resp, err := cli.GetFeeBalance(ctx, &daemonv2.GetFeeBalanceRequest{})
		must(err, "GetFeeBalance")
		b := resp.GetBalance()
		printJSON(map[string]uint64{
			"confirmed": b.GetConfirmedBalance(),
			"total":     b.GetTotalBalance(),
			"locked":    b.GetLockedBalance(),
		})
	case "withdrawfee":
		// Isolation test: a simple wallet-blinded CT transfer of native SEQ
		// from the fee account to ADDR. Exercises the same go-elements blinding
		// path the swap uses, but with the wallet as the sole/only blinder.
		amt, _ := strconv.ParseUint(envDefault("AMOUNT", "100000000"), 10, 64)
		pw, _ := os.ReadFile(os.Getenv("PW_FILE"))
		resp, err := cli.WithdrawFee(ctx, &daemonv2.WithdrawFeeRequest{
			Outputs: []*daemonv2.TxOutput{{
				Asset:       base, // native SEQ
				Amount:      amt,
				Script:      os.Getenv("ADDR_SCRIPT"),
				BlindingKey: os.Getenv("ADDR_BLINDKEY"),
			}},
			MillisatsPerByte: 110,
			Password:         string(pw),
		})
		must(err, "WithdrawFee")
		fmt.Println("withdraw txid:", resp.GetTxid())
	default:
		fmt.Fprintln(os.Stderr, "unknown ACTION:", action)
		os.Exit(2)
	}
}

func printMarketInfo(info *daemonv2.MarketInfo) {
	if info == nil {
		fmt.Println("<nil market info>")
		return
	}
	out := map[string]interface{}{
		"base_asset":    info.GetMarket().GetBaseAsset(),
		"quote_asset":   info.GetMarket().GetQuoteAsset(),
		"tradable":      info.GetTradable(),
		"strategy_type": info.GetStrategyType().String(),
		"name":          info.GetName(),
	}
	if p := info.GetPrice(); p != nil {
		out["base_price"] = p.GetBasePrice()
		out["quote_price"] = p.GetQuotePrice()
	}
	bal := map[string]map[string]uint64{}
	for asset, b := range info.GetBalance() {
		bal[asset] = map[string]uint64{
			"confirmed": b.GetConfirmedBalance(),
			"total":     b.GetTotalBalance(),
			"locked":    b.GetLockedBalance(),
		}
	}
	out["balance"] = bal
	printJSON(out)
}

func printJSON(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
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
