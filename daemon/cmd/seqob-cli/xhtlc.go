package main

// xhtlc-observe: the read-only, chain-agnostic HTLC OBSERVER the LSP bridge driver polls.
//
// The bridge driver (sequentia-web-wallet tooling/lsp/bridge-driver.mjs) obeys the pure fund-safety
// core leg-bridge.nextBridgeStep, which needs the two ends' live state each tick. This command supplies
// the ON-CHAIN end's state — { tip, funded, amount, confirmations, spent, preimage? } — by REUSING the
// audited xchain.Chain RPC primitives the maker already uses (OutputAt / TxConfirmations / SpenderOf /
// ExtractPreimage / BlockCount). No new crypto, no new tx building.
//
// It is chain-agnostic on purpose: every call is generic JSON-RPC, so the SAME command reads the
// Sequentia asset leg (Elements/sequentiad) AND the Bitcoin leg (bitcoind). The Bitcoin HTLC is the
// legacy-P2SH Design-A form, so its claim reveals the preimage in the input scriptSig — exactly what
// ExtractPreimage scans — so preimage read-back works on both rails when H is supplied.
//
// FUND-SAFETY: this NEVER moves value. Funding/claiming/refunding an HTLC in a bridge stays with the
// existing value-moving primitives (xsubas-fund-btc / xsubas-claim-btc / xsubas-refund), which the LSP
// io wires per the driver's front/recoup/refund actions. Observe only reports what the chain already shows.
//
//   seqob-cli xhtlc-observe -rpc <url> [-wallet <w>] -txid <t> -vout <v> [-hash <H hex>]

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXHtlcObserve(args []string) {
	fs := newFlagSet("xhtlc-observe")
	rpcURL := fs.String("rpc", "", "node RPC URL http://user:pass@host:port (bitcoind for the BTC leg, sequentiad for the asset leg) (required)")
	wallet := fs.String("wallet", "", "optional -rpcwallet name (Elements/bitcoind wallet endpoint)")
	txid := fs.String("txid", "", "HTLC funding txid (required)")
	vout := fs.Uint("vout", 0, "HTLC funding vout")
	hashHex := fs.String("hash", "", "swap hash H (hex); when set AND the HTLC is spent, the preimage is extracted from the spender")
	redeemHex := fs.String("redeem", "", "the HTLC redeemScript (hex); when set, script_bound reports whether the funded output pays P2SH(redeem) — the LSP bridge's fund-safety binding check")
	_ = fs.Parse(args)

	if *rpcURL == "" || *txid == "" {
		fatal("xhtlc-observe requires -rpc and -txid")
	}
	rpc, err := xliftRPCFromURL(*rpcURL)
	if err != nil {
		fatal("-rpc: %v", err)
	}
	// A plain xchain.Chain: its RPC helpers are generic (getblockcount / getrawtransaction / gettxout /
	// getrawmempool), so this reads a bitcoind or a sequentiad output identically.
	chain := xchain.NewChain(rpc, *wallet)

	out := map[string]interface{}{"txid": *txid, "vout": *vout}
	if tip, terr := chain.BlockCount(); terr == nil {
		out["tip"] = tip
	}
	// Funded + amount (+ asset id on the Elements leg). A fetch error = not (yet) funded/visible.
	o, oerr := chain.OutputAt(*txid, uint32(*vout))
	funded := oerr == nil && o != nil
	out["funded"] = funded
	if funded {
		out["amount"] = o.ValueAtoms // sats on the BTC leg, asset atoms on the SEQ leg
		out["script_pubkey"] = o.ScriptPubKeyHex
		if o.AssetID != "" {
			out["asset_id"] = o.AssetID
		}
		// FUND-SAFETY BINDING (bridge): does this funded output actually pay P2SH(redeemScript)? The LSP
		// only reports lockedToLsp:true when its parse of the maker's redeemScript (claim==LSP on H) AND
		// this binding both hold — so a maker that quotes one script but funds a different output is caught
		// here, before the LSP ever fronts. A legacy-P2SH output's scriptPubKey is a914<hash160(redeem)>87.
		if *redeemHex != "" {
			if rb, rerr := hex.DecodeString(*redeemHex); rerr == nil && len(rb) > 0 {
				wantSPK := "a914" + xchain.P2SHHash160(rb) + "87"
				out["script_bound"] = strings.EqualFold(wantSPK, o.ScriptPubKeyHex)
			} else {
				out["script_bound"] = false
			}
		}
	}
	if c, cerr := chain.TxConfirmations(*txid); cerr == nil {
		out["confirmations"] = c
	}
	// Spent? SpenderOf returns "" while the outpoint is still in the UTXO set (gettxout non-null).
	spender, serr := chain.SpenderOf(*txid, uint32(*vout))
	if serr == nil {
		out["spent"] = spender != ""
		if spender != "" {
			out["spender_txid"] = spender
			// The claim reveals P in the spender's scriptSig; surface it when H is known so the LSP can
			// settle/recoup the OTHER end. (Read-only: extracting P is not a value move.)
			if *hashHex != "" {
				if H, herr := hex.DecodeString(*hashHex); herr == nil && len(H) == 32 {
					if pre, perr := chain.ExtractPreimage(spender, H); perr == nil && len(pre) == 32 {
						out["preimage"] = hex.EncodeToString(pre)
					}
				}
			}
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
