package main

// xsubas-sell.go: the SUB-ASSET SELL taker — pay an asset OVER LIGHTNING and receive
// BTC ON-CHAIN. It matches a maker offer where the maker BUYS the asset (ln_direction
// LnBTCForAssetLN): the maker locks BTC on-chain + holds an asset invoice on H; this
// taker verifies the BTC HTLC, pays the asset invoice from its OWN asset LN node (device
// co-signs), learns P when the maker settles, and claims the BTC on-chain (device-signed).
// It drives internal/seqob/client.RunTakerSubAssetSell over the relay courier.

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdXSubAsSell(args []string) {
	fs := newFlagSet("xsubas-sell")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	asset := fs.String("asset", "", "Sequentia asset id (hex); the pair is <asset>/BTC (required)")
	offerID := fs.String("offer-id", "", "offer id to take (empty: first verified matching sub-asset-sell offer)")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer (with -offer-id)")
	priv := fs.String("priv", "", "taker SESSION secret key (32-byte hex, E2E only); generated if empty")
	btcRPCURL := fs.String("btc-rpc", "", "bitcoind RPC URL http://user:pass@host:port (required; verifies + claims the maker's BTC HTLC)")
	btcWallet := fs.String("btc-wallet", "", "bitcoind wallet that RECEIVES the claimed BTC")
	btcChainName := fs.String("btc-chain", "testnet4", "parent chain params: testnet4 | regtest")
	assetLnSocket := fs.String("asset-ln-socket", "", "the taker's asset LN node (pays the asset over LN) (required)")
	minBTCConf := fs.Int("min-btc-conf", 1, "confirmations required on the maker's BTC HTLC before paying the asset")
	spendFee := fs.Uint64("spend-fee", 1000, "BTC HTLC claim fee target (sats)")
	claimPrivHex := fs.String("btc-claim-priv", "", "device BTC claim privkey (32-byte hex); generated if empty (the key that claims the BTC — never given to the LSP)")
	termsWait := fs.Duration("terms-wait", 2*time.Minute, "max wait for the maker's terms")
	payWait := fs.Duration("pay-wait", 15*time.Minute, "max wait for the asset LN payment to settle")
	_ = fs.Parse(args)

	if *asset == "" || *btcRPCURL == "" || *assetLnSocket == "" {
		fatal("xsubas-sell requires -asset, -btc-rpc, -asset-ln-socket")
	}

	// 1. Find + verify a matching sub-asset-SELL offer (maker BUYS: LnBTCForAssetLN).
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *asset, offer.BTCSentinel), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		if *offerID != "" && (o.GetOfferId() != *offerID || o.GetMakerPubkey() != *makerPub) {
			continue
		}
		lt := o.GetLightning()
		if lt == nil || lt.GetLnDirection() != offer.LnBTCForAssetLN {
			continue
		}
		if err := offer.VerifyOffer(o); err != nil {
			continue
		}
		target = o
		break
	}
	if target == nil {
		fatal("no verified sub-asset-SELL offer found to sell %s for on-chain BTC", *asset)
	}
	assetAtoms := target.GetWantAmount() // the asset the taker pays over LN
	btcSats := target.GetOfferAmount()   // the BTC sats the taker receives on-chain
	fmt.Printf("taking sub-asset-SELL offer %s by %s: pay %d %s OVER LIGHTNING, receive %d BTC sats ON-CHAIN\n",
		target.GetOfferId(), short(target.GetMakerPubkey()), assetAtoms, *asset, btcSats)

	// 2. Wire bitcoind (BTC claim leg) + the taker's asset LN node (pays). Validate.
	btcRPC, err := xliftRPCFromURL(*btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(*btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, *btcWallet, params)
	if _, err := btcChain.BlockCount(); err != nil {
		fatal("bitcoind unreachable: %v", err)
	}
	if _, err := xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset).NodeID(); err != nil {
		fatal("asset lightning-rpc %s unreachable: %v", *assetLnSocket, err)
	}
	// The device BTC claim key (kept device-side; never handed to the LSP).
	var claimKey *xchain.Key
	if *claimPrivHex != "" {
		kb, derr := hex.DecodeString(*claimPrivHex)
		if derr != nil || len(kb) != 32 {
			fatal("-btc-claim-priv must be 32-byte hex")
		}
		claimKey = xchain.KeyFromBytes(kb)
	} else if claimKey, err = xchain.NewKey(); err != nil {
		fatal("mint claim key: %v", err)
	}

	// 3. Open the swap session; E2E key from the SIGNED offer. The taker's BTC-leg swap
	//    uses a hash-only lock (it learns P from paying the asset invoice).
	takerKey := loadOrGenKey(*priv)
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer conn.Close()
	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId:            target.GetOfferId(),
		MakerPubkey:        target.GetMakerPubkey(),
		TakeAmount:         target.GetBaseAmount(),
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}})
	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sid := la.GetLiftAccepted().GetSessionId()
	fmt.Printf("sub-asset-SELL session %s opened\n", sid)

	makerOfferPub, err := hex.DecodeString(target.GetMakerPubkey())
	if err != nil {
		fatal("decode maker pubkey: %v", err)
	}
	pk, err := btcec.ParsePubKey(makerOfferPub)
	if err != nil {
		fatal("parse maker pubkey: %v", err)
	}
	crypter, err := client.NewCrypter(takerKey, pk)
	if err != nil {
		fatal("crypter: %v", err)
	}
	send := func(sealed []byte) error {
		b, err := jsonMarshal.Marshal(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	recv := func(timeout time.Duration) ([]byte, error) {
		from, err := readWSUntilSwap(conn, timeout)
		if err != nil {
			return nil, err
		}
		return from.GetSwapMsg().GetCiphertext(), nil
	}

	res, err := client.RunTakerSubAssetSell(client.TakerSubAssetSellParams{
		// Bind the BTC-leg swap to the maker's H once Terms arrive (VerifyBTCLeg/
		// ClaimBTCLeg recompute against it); a fresh asset LN leg pays the invoice.
		NewTakerOps: func(hashH []byte) client.SubAssetSellTakerOps {
			return &client.LiveSubAssetSellTakerOps{
				Swap:    xchain.NewSwapBitcoin(btcChain, nil, xchain.NewHashLockFromHash(hashH)),
				AssetLN: xchain.NewCLNAssetLNLeg(*assetLnSocket, *asset),
				BTC:     btcChain,
			}
		},
		Crypter:      crypter,
		BtcAmount:    btcSats,
		AssetAmount:  assetAtoms,
		MinBTCConf:   *minBTCConf,
		SpendFeeSats: *spendFee,
		BtcClaimKey:  claimKey,
		Timing:       client.XcTiming{TermsWait: *termsWait, BtcFundWait: *termsWait, SeqLockWait: *payWait},
		Log:          func(format string, a ...interface{}) { fmt.Printf(format+"\n", a...) },
	}, send, recv)
	if err != nil {
		fmt.Printf("sub-asset-SELL swap ended: %v\n", err)
		return
	}
	fmt.Printf("SUB-ASSET SELL SWAP SETTLED: paid %d %s over Lightning, received %d BTC sats on-chain in %s; preimage %s\n",
		assetAtoms, *asset, btcSats, res.BtcClaimTxid, hex.EncodeToString(res.Preimage))
}
