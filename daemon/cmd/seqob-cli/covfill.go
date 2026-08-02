package main

// covfill: the SINGLE-FILL taker for a resting passive-CLOB covenant order.
//
// A covenant offer is a tapscript-locked on-chain UTXO ANYONE can fill by
// assembling the fill spend that pays the maker its want (asset B, at the
// covenant's own ceil price) and takes the offered asset A — no maker liveness,
// no courier session, no signature from anyone but the taker's own wallet. This
// command fetches the offer from the relay, verifies the maker signature AND the
// on-chain covenant UTXO (trustless spk re-derivation, unspent, funded as
// advertised), then drives internal/seqob/covfill: assemble, wallet-sign,
// testmempoolaccept-gate, broadcast.
//
// A covenant fill is ONE atomic tx: on failure nothing moved (no refund state,
// no resume command — unlike the HTLC takers there is nothing to recover).
//
//	seqob-cli covfill -relay <r> -offer-id <id> -maker-pubkey <pk>
//	  -amount <asset-A atoms | 0 = whole covenant>
//	  -seq-rpc http://user:pass@host:port -seq-wallet <w>
//	  [-base <hex> -quote <hex>]   (the offer's market; scanned from /v1/markets when omitted)
//	  [-fee-asset <display hex>]   (default: the chain's pegged bitcoin asset)
//	  [-spend-fee <atoms>]         (explicit network fee in the fee asset's own atoms)

import (
	"fmt"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/bridge"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/covfill"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdCovFill(args []string) {
	fs := newFlagSet("covfill")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	base := fs.String("base", "", "market base asset (optional; with -quote skips the market scan)")
	quote := fs.String("quote", "", "market quote asset (optional)")
	offerID := fs.String("offer-id", "", "offer id to fill (required)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (required)")
	amount := fs.Uint64("amount", 0, "asset-A atoms to take (0 = the whole covenant; a dust remainder auto-shrinks)")
	seqRPCURL := fs.String("seq-rpc", "", "Sequentia node RPC URL http://user:pass@host:port (required)")
	seqWallet := fs.String("seq-wallet", "", "Sequentia node wallet funding the fill (pays asset B + the fee, receives asset A) (required)")
	feeAsset := fs.String("fee-asset", "", "fee asset (display hex; default: the chain's pegged bitcoin asset)")
	spendFee := fs.Uint64("spend-fee", 2000, "explicit network fee, in the fee asset's own atoms")
	allowCancellable := fs.Bool("allow-maker-cancellable", false, "fill a non-NUMS-internal-key covenant (maker key-path cancel/rug risk); default: refuse")
	_ = fs.Parse(args)

	if *offerID == "" || *makerPub == "" || *seqRPCURL == "" || *seqWallet == "" {
		fatal("covfill requires -offer-id, -maker-pubkey, -seq-rpc and -seq-wallet")
	}

	target := findOffer(*relay, *base, *quote, *offerID, *makerPub)
	if target == nil {
		fatal("offer %s by %s not found on %s", *offerID, short(*makerPub), *relay)
	}
	// The relay is untrusted: verify the maker's signature over the served offer
	// before acting on it (the same gate every taker command applies).
	if err := offer.VerifyOffer(target); err != nil {
		fatal("offer %s failed maker signature verification (refusing to fill): %v", *offerID, err)
	}
	ct := target.GetCovenant()
	if ct == nil {
		fatal("offer %s is not a covenant offer (settlement family %T)", *offerID, target.GetSettlement())
	}
	order, err := bridge.OrderFromTerms(ct)
	if err != nil {
		fatal("covenant terms: %v", err)
	}
	op := bridge.OutpointFromTerms(ct)

	seqRPC, err := xliftRPCFromURL(*seqRPCURL)
	if err != nil {
		fatal("-seq-rpc: %v", err)
	}
	chain := xchain.NewChain(seqRPC, *seqWallet)

	fee := *feeAsset
	if fee == "" {
		pegged, perr := chain.PeggedAsset()
		if perr != nil {
			fatal("no -fee-asset given and getsidechaininfo failed (%v); pass -fee-asset explicitly", perr)
		}
		fee = pegged
	}

	fmt.Printf("covenant offer %s: fills at rate %d/%d (min_lot %d), resting at %s:%d\n",
		*offerID, ct.GetRateNum(), ct.GetRateDen(), ct.GetMinLot(), op.Txid, op.Vout)

	res, err := covfill.FillCovenant(covfill.Params{
		Order:                 order,
		Txid:                  op.Txid,
		Vout:                  op.Vout,
		TakeAtoms:             *amount,
		FeeAtoms:              *spendFee,
		FeeAssetDisplay:       fee,
		Node:                  chain.RPC(),
		AllowMakerCancellable: *allowCancellable,
	})
	if err != nil {
		fatal("covenant fill: %v", err)
	}

	fmt.Printf("filled %d asset-A atoms%s, paid the maker %d asset-B atoms (remainder %d re-rested)\n",
		res.Filled, partialNote(res.Filled, res.Locked), res.PaidB, res.Remainder)
	fmt.Printf("SWAP SETTLED: txid %s\n", res.Txid)
}

// findOffer locates the target offer: directly in one market's book when
// base/quote are given, else by scanning every market the relay lists.
func findOffer(relay, base, quote, offerID, makerPub string) *seqobv1.Offer {
	lookup := func(b, q string) *seqobv1.Offer {
		var book seqobv1.PublicBook
		if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", relay, b, q), &book); err != nil {
			return nil
		}
		for _, o := range book.GetOffers() {
			if o.GetOfferId() == offerID && o.GetMakerPubkey() == makerPub {
				return o
			}
		}
		return nil
	}
	if base != "" && quote != "" {
		return lookup(base, quote)
	}
	var ml seqobv1.MarketList
	if err := getJSON(relay+"/v1/markets", &ml); err != nil {
		fatal("list markets on %s: %v (or pass -base/-quote)", relay, err)
	}
	for _, m := range ml.GetMarkets() {
		if o := lookup(m.GetPair().GetBaseAsset(), m.GetPair().GetQuoteAsset()); o != nil {
			return o
		}
	}
	return nil
}
