// Command seqob-maker is the long-running MAKER participant for the SeqOB
// order-book DEX. It is just one participant (anyone can run it the same way):
// it posts a signed resting offer to the relay over WebSocket, then settles each
// lift by reusing the PROVEN Ocean settlement (wallet.Service.CompleteSwap, now
// blind-aware) via the shared internal/seqob/client primitives. Confidential is
// opt-in: a confidential offer publishes a blinding pubkey and settles blinded;
// an explicit offer omits it and settles explicit. The relay never decrypts the
// couriered swap messages; it only routes ciphertext.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"github.com/thanhpk/randstr"
	"google.golang.org/protobuf/encoding/protojson"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/core/application"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	oceanwallet "github.com/aejkcs50/seqdex/daemon/internal/infrastructure/ocean-wallet"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	"github.com/aejkcs50/seqdex/daemon/pkg/swap"
	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

var jsonMarshal = protojson.MarshalOptions{UseProtoNames: true}
var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

func main() {
	relay := flag.String("relay", "http://127.0.0.1:9955", "relay base URL")
	ocean := flag.String("ocean", "127.0.0.1:18000", "ocean wallet endpoint")
	nodeRPC := flag.String("node-rpc", "", "Sequentia node RPC URL (for the open fee market)")
	account := flag.String("account", "", "Ocean account that holds the OFFER asset (and funds the fee)")
	makerPriv := flag.String("maker-priv", "", "maker offer/identity + E2E key (32-byte hex); generated if empty")
	base := flag.String("base", "gold", "base asset id")
	quote := flag.String("quote", "usdx", "quote asset id")
	side := flag.String("side", "sell", "maker side: sell|buy (sells/buys the base)")
	baseAmt := flag.Uint64("base-amount", 100, "base size (base atoms)")
	quoteAmt := flag.Uint64("quote-amount", 45, "quote size (quote atoms)")
	feeAsset := flag.String("fee-asset", "", "preferred fee asset hint (any-asset fee market)")
	expiry := flag.Duration("expiry", time.Hour, "time until the offer expires")
	minAnchor := flag.Uint("min-anchor-depth", 0, "Bitcoin-anchor confs before FILLED (0 = 0-conf tolerant)")
	confidential := flag.Bool("confidential", true, "post a confidential offer (blinded settlement); false = explicit")
	msats := flag.Uint64("msats-per-byte", 110, "network fee rate (milli-sat/vByte); raise if the node rejects for low fee")
	offerID := flag.String("offer-id", "", "offer id (random 16-byte hex if empty)")
	mode := flag.String("mode", "samechain", "settlement mode: samechain | cross | lightning | pureln | subasset (cross = BTC<->asset on-chain HTLC; lightning = asset<->BTC-over-LN submarine swap; pureln = BOTH legs over Lightning; subasset = the submarine's MIRROR: asset over Lightning + BTC on-chain HTLC, taker pays BTC on-chain and receives the asset over LN; base is the asset, quote is the BTC sentinel)")
	// Cross-mode settlement wiring (pkg/xchain, no Ocean needed): the SEQ leg is
	// funded from the Sequentia NODE wallet and the BTC leg is claimed into the
	// bitcoind wallet — the same reserves the RFQ maker uses.
	btcRPCURL := flag.String("btc-rpc", "", "cross: bitcoind RPC URL http://user:pass@host:port (required for -mode cross)")
	btcWallet := flag.String("btc-wallet", "", "cross: bitcoind wallet holding/receiving the BTC side")
	btcChainName := flag.String("btc-chain", "testnet4", "cross: parent chain params: testnet4 | regtest")
	xseqRPCURL := flag.String("xseq-rpc", "", "cross: Sequentia node RPC URL http://user:pass@host:port (required for -mode cross)")
	xseqWallet := flag.String("xseq-wallet", "", "cross: Sequentia node wallet funding the asset leg")
	btcDelta := flag.Uint("btc-locktime-delta", 100, "cross: T_btc = parent tip + this (longer leg in time; ~16h)")
	seqDelta := flag.Uint("seq-locktime-delta", 240, "cross: T_seq = SEQ tip + this (shorter leg in time; ~2h — must cover the taker's real parent confirmation, or takers refuse the terms)")
	minBTCConf := flag.Int("min-btc-conf", 1, "cross: confirmations required on the taker's BTC leg (1 = testnet-grade; confirmation depth, not anchoring, protects the maker's BTC side — raise for real value)")
	spendFee := flag.Uint64("spend-fee", 1000, "cross: HTLC spend fee target in native sats (converted per-asset via the fee market)")
	btcFeeRate := flag.Float64("btc-fee-rate", 2, "cross: sat/vB fee rate for funding the BTC HTLC leg (explicit, so it never depends on the node's estimatesmartfee/settxfee; 0 = node default)")
	xstateDir := flag.String("xstate-dir", "xmaker-sessions", "cross: directory for per-lift session state (keys/legs; the recovery material)")
	resume := flag.Bool("resume", false, "cross: instead of serving, finish every non-terminal session in -xstate-dir (post-restart on-chain claim/refund) and exit")
	lnSocket := flag.String("ln-socket", "", "lightning/pureln: the maker's SeqLN-on-Bitcoin lightning-rpc unix socket (BTC leg; required for -mode lightning and -mode pureln)")
	subAnchor := flag.Int64("sub-anchor-depth", 3, "lightning: Bitcoin-anchor depth the maker requires on the taker's asset funding before it pays the invoice (>=2; the submarine cross-leg safety gate)")
	max0conf := flag.Uint64("max-0conf", 0, "lightning: 0-conf LP-fronting cap (asset atoms). Trades whose on-chain asset leg is <= this settle INSTANTLY (skip the anchor-bury wait); the maker/taker fronts the Bitcoin-reorg risk. Advertised in the offer's LightningTerms. 0 = disabled (always anchor-gate).")
	onchainCltv := flag.Uint("onchain-cltv", 240, "lightning: advisory CLTV (blocks) in the resting LightningTerms (the load-bearing T_seq is minted per-lift)")
	assetLnSocket := flag.String("asset-ln-socket", "", "pureln: the maker's SeqLN-on-Sequentia lightning-rpc unix socket (asset leg; required for -mode pureln)")
	btcAsset := flag.String("btc-asset", "", "pureln: counter-leg SETTLEMENT asset id (hex); empty = policy asset / real BTC-LN. Set to route the counter leg over a 2nd issued asset (asset<->asset pure-LN)")
	quoteAsset := flag.String("quote-asset", "", "pureln: QUOTE asset id (hex) the offer advertises; empty = the BTC sentinel (asset<->BTC). Set to a real asset id for a truthful asset<->asset market. Put the NUMERAIRE on the QUOTE side so the market key matches the wallet's canonicalPair (e.g. base=OILX -quote-asset=EURX, i.e. OILX priced in EURX) — the wallet queries <base>/<quote> in exactly that orientation, so a reversed pair rests in a market the wallet never reads. Defaults -btc-asset to the quote id so settlement routes over it")
	holdTimeout := flag.Duration("hold-timeout", 2*time.Minute, "pureln: how long the maker waits for the taker to lock its hold and then fulfills before giving up")
	requote := flag.Bool("requote", false, "cross/lightning/pureln: after each settled fill, reconnect dropped channel peers and re-post a FRESH offer (same offer id) instead of cancelling and exiting; keeps a live quote without a manual restart (default off = quote once then exit)")
	flag.Parse()

	// Cross resume needs no maker key or offer: it drives on-chain settlement
	// from persisted per-session keys. Handle it before the key/offer setup.
	if strings.ToLower(*mode) == "cross" && *resume {
		resumeCrossSessions(*xstateDir, *btcRPCURL, *btcWallet, *btcChainName, *xseqRPCURL, *xseqWallet, *spendFee, *btcFeeRate)
		return
	}

	cross := strings.ToLower(*mode) == "cross"
	lightning := strings.ToLower(*mode) == "lightning"
	pureln := strings.ToLower(*mode) == "pureln"
	subasset := strings.ToLower(*mode) == "subasset"
	subassetSell := strings.ToLower(*mode) == "subasset-sell"
	if !cross && !lightning && !pureln && !subasset && !subassetSell && *account == "" {
		fatal("-account is required (the Ocean account holding the offer asset)")
	}

	makerKey := loadOrGenKey(*makerPriv)
	makerPubHex := hex.EncodeToString(makerKey.PubKey().SerializeCompressed())
	ctx := context.Background()

	if subassetSell {
		runSubAssetSellMaker(subAssetSellMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, offerID: *offerID,
			btcRPCURL: *btcRPCURL, btcWallet: *btcWallet, btcChainName: *btcChainName,
			assetLnSock: *assetLnSocket, btcDelta: uint32(*btcDelta), minBTCConf: *minBTCConf,
			spendFee: *spendFee, holdTimeout: *holdTimeout, requote: *requote,
		})
		return
	}

	if subasset {
		runSubAssetMaker(subAssetMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			btcRPCURL: *btcRPCURL, btcWallet: *btcWallet, btcChainName: *btcChainName,
			assetLnSock: *assetLnSocket, btcDelta: uint32(*btcDelta), minBTCConf: *minBTCConf,
			spendFee: *spendFee, holdTimeout: *holdTimeout, requote: *requote,
		})
		return
	}

	if pureln {
		// asset<->asset: -quote-asset advertises the true quote AND (unless -btc-asset is set
		// explicitly) routes the settlement counter-leg over that same asset, so one flag makes a
		// truthful OILX/EURX market (base=commodity, quote=numeraire) instead of the old BTC-sentinel
		// disguise. Keep the numeraire as the quote so the market key matches the wallet's canonicalPair.
		effBtcAsset := *btcAsset
		if effBtcAsset == "" {
			effBtcAsset = *quoteAsset
		}
		runPureLNMaker(pureLNMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			assetLnSock: *assetLnSocket, btcLnSock: *lnSocket, btcAsset: effBtcAsset, quoteAsset: *quoteAsset,
			holdTimeout: *holdTimeout, onchainCltv: uint32(*onchainCltv),
			reverse: strings.ToLower(*side) == "sell", // sell = maker gives the asset (holds BTC); buy = maker acquires (holds asset)
			requote: *requote,
		})
		return
	}

	if lightning {
		runSubmarineMaker(submarineMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			makerPubKey: makerKey.PubKey().SerializeCompressed(),
			asset:       *base, assetAmt: *baseAmt, btcSats: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			seqRPCURL: *xseqRPCURL, seqWallet: *xseqWallet, lnSocket: *lnSocket,
			seqDelta: uint32(*seqDelta), subAnchor: *subAnchor, onchainCltv: uint32(*onchainCltv),
			spendFee: *spendFee, max0conf: *max0conf,
			reverse: strings.ToLower(*side) == "sell", // sell = maker-secret REVERSE; buy = NORMAL
			requote: *requote,
		})
		return
	}

	if cross {
		runCrossMaker(crossMakerConfig{
			relay: *relay, makerKey: makerKey, makerPubHex: makerPubHex,
			asset: *base, side: *side, assetAmt: *baseAmt, btcAmt: *quoteAmt,
			feeAsset: *feeAsset, expiry: *expiry, minAnchor: uint32(*minAnchor), offerID: *offerID,
			btcRPCURL: *btcRPCURL, btcWallet: *btcWallet, btcChainName: *btcChainName,
			seqRPCURL: *xseqRPCURL, seqWallet: *xseqWallet,
			btcDelta: uint32(*btcDelta), seqDelta: uint32(*seqDelta),
			minBTCConf: *minBTCConf, spendFee: *spendFee, stateDir: *xstateDir,
			btcFeeRate: *btcFeeRate,
			requote:    *requote,
		})
		return
	}

	// Reuse the proven Ocean settlement exactly like the daemon.
	w, err := oceanwallet.NewService(*ocean)
	if err != nil {
		fatal("connect ocean wallet %q: %v", *ocean, err)
	}
	svc, err := application.NewWalletService(w, *nodeRPC)
	if err != nil {
		fatal("wallet service: %v", err)
	}
	defer svc.Close()
	net := svc.Network()

	// Derive the maker's receive address; publish its blinding pubkey only for a
	// confidential offer so the taker mirrors the maker's confidentiality posture.
	addrs, err := svc.Account().DeriveAddresses(ctx, *account, 1)
	if err != nil || len(addrs) == 0 {
		fatal("derive recv address for account %q: %v", *account, err)
	}
	recvAddr := addrs[0]
	blindingPub := ""
	if *confidential {
		info, err := seqnet.FromConfidential(recvAddr, &net)
		if err != nil {
			fatal("parse recv address: %v", err)
		}
		blindingPub = hex.EncodeToString(info.BlindingKey)
	}

	o := buildOffer(*base, *quote, *side, *baseAmt, *quoteAmt, *feeAsset,
		*expiry, uint32(*minAnchor), recvAddr, blindingPub, *offerID)
	// Post into the SEPARATE blinded book when confidential: the signed namespace tag
	// segregates this offer so it only crosses another confidential offer (both legs
	// blind on-chain). recvAddr is already the blech32 confidential form here (its
	// blinding pubkey was just parsed), satisfying the relay's confidential-offer rules.
	o.Confidential = *confidential
	if err := offer.SignOffer(o, makerKey); err != nil {
		fatal("sign offer: %v", err)
	}

	// Maker-only backend: the LiveWallet only calls ResponderComplete, which uses
	// CompleteSwapFn. Wire it to the blind-aware CompleteSwap; the taker-side seams
	// are unused here (dummy key, never exercised).
	rb := client.NewRealBackend(&net, makerKey.Serialize(), makerKey.Serialize())
	rb.CompleteSwapFn = func(req *seqdexv1.SwapRequest, blind bool) (string, []swap.UnblindedInput, error) {
		signedPSET, utxos, _, err := svc.CompleteSwap(*account, swapReqAdapter{req}, *msats, true, blind)
		if err != nil {
			return "", nil, err
		}
		return signedPSET, utxosToSwapUnblinded(utxos), nil
	}
	maker := &client.Maker{
		Wallet: &client.LiveWallet{Backend: rb, MakerOutputsConfidential: *confidential, RequireConfidential: *confidential},
		// Bind every co-sign to this signed offer (asset legs, price floor,
		// remaining size) so a malicious taker cannot drain the maker.
		Offer: o,
	}

	// Connect, submit the offer (this registers the conn for live lifts), then
	// serve lifts until killed.
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	defer conn.Close()

	if err := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}}); err != nil {
		fatal("post offer: %v", err)
	}
	fmt.Printf("seqob-maker up: posted %s offer %s by maker %s\n", *side, o.GetOfferId(), makerPubHex)
	fmt.Printf("  pair %s/%s  give %d %s  want %d %s  confidential=%v  fee-rate=%d msat/vB\n",
		o.GetPair().GetBaseAsset(), o.GetPair().GetQuoteAsset(), o.GetOfferAmount(), o.GetOfferAsset(), o.GetWantAmount(), o.GetWantAsset(), *confidential, *msats)
	fmt.Printf("  taker lifts with: -offer-id %s -maker-pubkey %s\n", o.GetOfferId(), makerPubHex)

	serve(conn, maker, makerKey, wsURL, *expiry)
}

// serve is the maker's single-goroutine event loop: derive a per-lift E2E key on
// lift_requested, then on the taker's couriered SwapRequest run the responder and
// courier back the SwapAccept. A later swap_msg for the same session is the
// taker's SwapComplete (the swap settled).
func serve(conn *websocket.Conn, maker *client.Maker, makerKey *btcec.PrivateKey, wsURL string, expiry time.Duration) {
	crypters := make(map[string]*client.Crypter)
	accepted := make(map[string]bool)
	takes := make(map[string]uint64) // session -> take_amount, for the settle-ack fill_base (B-1)
	o := maker.Offer
	// Exit-for-requote (see requote_exit.go). A co-sign session quiet for 10
	// minutes is treated as abandoned (taker vanished after SwapAccept) so a
	// stuck session can never pin a stale quote on the book forever.
	var liveSessions, lastAccept atomic.Int64
	idle := func() bool {
		return liveSessions.Load() == 0 || time.Since(time.Unix(lastAccept.Load(), 0)) > 10*time.Minute
	}
	armRequoteExit(o, idle)
	// Reconnect resilience + STAY on the book. Exiting on a WS error lets the supervisor restart us,
	// which mints a NEW offer_id → a taker who lifted the old id races a replaced offer and gets "no
	// maker co-sign". So we reconnect and re-post the SAME offer_id (refreshed window + re-sign). Two
	// hardening rules the naive version missed: (1) re-posts are RATE-LIMITED — the relay validates a
	// submit ASYNC and rejects (rate limit / expired) with a From_Error, keeping the conn open; an
	// unthrottled recovery loop would exhaust the per-maker rate budget and leave us silently off-book
	// (blocked on ReadMessage with no live offer). (2) reconnects use an ESCALATING backoff so a
	// reconnect-succeeds-then-immediately-drops cycle can't spin the CPU / hammer a struggling relay.
	var lastPost time.Time
	const minRepostGap = 20 * time.Second
	repost := func(c *websocket.Conn, reason string) bool {
		if time.Since(lastPost) < minRepostGap {
			return false
		}
		if e := refreshOfferForRequote(o, expiry, makerKey); e != nil {
			fmt.Printf("re-post (%s): re-sign failed: %v\n", reason, e)
			return false
		}
		if we := writeWS(c, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}}); we != nil {
			return false
		}
		lastPost = time.Now()
		fmt.Printf("re-posted offer %s (%s)\n", o.GetOfferId(), reason)
		return true
	}
	backoff := time.Second
	redial := func(old *websocket.Conn) *websocket.Conn {
		old.Close()
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		for {
			nc, _, de := websocket.DefaultDialer.Dial(wsURL, nil)
			if de != nil {
				time.Sleep(backoff)
				continue
			}
			lastPost = time.Time{} // a reconnect genuinely evicted our offer — force the re-post past the rate gate
			if !repost(nc, "reconnect") {
				nc.Close()
				time.Sleep(backoff)
				continue
			}
			return nc
		}
	}
	for {
		var from seqobv1.From
		_, data, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("ws read: %v; reconnecting + re-posting\n", err)
			conn = redial(conn)
			continue
		}
		backoff = time.Second // a healthy read: the connection is good, reset the reconnect backoff
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		switch {
		case from.GetLiftRequested() != nil:
			lr := from.GetLiftRequested()
			cr, err := client.NewMakerCrypterFromLift(makerKey, lr.GetTakerSessionPubkey())
			if err != nil {
				fmt.Printf("lift %s: crypter error: %v\n", lr.GetSessionId(), err)
				continue
			}
			crypters[lr.GetSessionId()] = cr
			takes[lr.GetSessionId()] = lr.GetTakeAmount()
			fmt.Printf("lift requested: session %s offer %s take %d\n",
				lr.GetSessionId(), lr.GetOfferId(), lr.GetTakeAmount())

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			sid := sm.GetSessionId()
			if accepted[sid] {
				// The taker couriered SwapComplete: the same-chain lift settled on-chain. Ack the relay
				// so it records the trade + decrements this resting order — the relay never parses the
				// opaque courier, so without this ack the order lingers as a ghost and the trades feed
				// stays empty (B-1). Best-effort txid from the completed tx; fill_base is the lift's take.
				txid, _ := maker.HandleComplete(sm.GetCiphertext(), crypters[sid])
				// anchor_confs = the offer's own min_anchor_depth: the maker declares its finality
				// bar met at settlement, so a min_anchor_depth>0 offer reaches FILLED (default 0 keeps
				// the 0-conf-tolerant behavior). Previously the relay hard-coded 0, so >0 never finalized.
				if we := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SettleAck{SettleAck: &seqobv1.SettleAck{
					SessionId: sid, SettleTxid: txid, FillBase: takes[sid], AnchorConfs: o.GetMinAnchorDepth(),
				}}}); we != nil {
					fmt.Printf("session %s: settle-ack write failed: %v; reconnecting\n", sid, we)
					conn = redial(conn)
				}
				fmt.Printf("session %s: SWAP SETTLED (acked relay; txid=%s)\n", sid, txid)
				delete(accepted, sid) // B3: prune per-session state now the swap is done (serve() runs forever)
				delete(crypters, sid)
				delete(takes, sid)
				liveSessions.Add(-1)
				requoteExitIfPending(idle)
				continue
			}
			cr := crypters[sid]
			if cr == nil {
				fmt.Printf("session %s: swap_msg before lift_requested; ignoring\n", sid)
				continue
			}
			sealedAccept, err := maker.HandleRequest(sm.GetCiphertext(), cr)
			if err != nil {
				fmt.Printf("session %s: complete swap failed: %v\n", sid, err)
				continue
			}
			if we := writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealedAccept}}}); we != nil {
				fmt.Printf("session %s: co-sign write failed: %v; reconnecting\n", sid, we)
				conn = redial(conn)
				continue
			}
			accepted[sid] = true
			liveSessions.Add(1)
			lastAccept.Store(time.Now().Unix())
			fmt.Printf("session %s: couriered SwapAccept (%d bytes); awaiting taker broadcast\n", sid, len(sealedAccept))

		case from.GetOrderStatus() != nil:
			st := from.GetOrderStatus()
			fmt.Printf("order %s status=%s active=%d txid=%s\n",
				st.GetOfferId(), st.GetStatus(), st.GetActiveAmount(), st.GetSettleTxid())

		case from.GetError() != nil:
			e := from.GetError()
			fmt.Printf("relay error %d: %s\n", e.GetCode(), e.GetMessage())
			// B1: a relay error can mean our offer was rejected/evicted (rate limit, expired) and is now
			// off-book — the relay validates a submit ASYNC and reports rejection here, not as a write
			// error. Re-post it (rate-limited, so a persistent-error loop can't storm the relay) so we
			// don't sit silently unliftable on a still-open connection.
			if !repost(conn, "after relay error") && offerRejected(e.GetCode(), e.GetMessage()) {
				requoteExitIfIdle("relay rejected our offer ("+e.GetMessage()+")", idle)
			}
		}
	}
}

func buildOffer(base, quote, side string, baseAmt, quoteAmt uint64, feeAsset string,
	expiry time.Duration, minAnchor uint32, recvAddr, blindingPub, id string) *seqobv1.Offer {
	o := &seqobv1.Offer{
		OfferId:        orDefault(id, randstr.Hex(16)),
		SchemaVersion:  1,
		Pair:           &seqobv1.AssetPair{BaseAsset: base, QuoteAsset: quote},
		BaseAmount:     baseAmt,
		AllowPartial:   true,
		CreatedAtUnix:  uint64(time.Now().Unix()),
		ExpiresAtUnix:  uint64(time.Now().Add(expiry).Unix()),
		FeeAssetHint:   feeAsset,
		MinAnchorDepth: minAnchor,
		Settlement: &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{
			MakerRecvAddress: recvAddr,
			MakerBlindingPub: blindingPub,
		}},
	}
	switch strings.ToLower(side) {
	case "sell":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = base, baseAmt
		o.WantAsset, o.WantAmount = quote, quoteAmt
	case "buy":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = quote, quoteAmt
		o.WantAsset, o.WantAmount = base, baseAmt
	default:
		fatal("side must be sell or buy")
	}
	return o
}

// buildCrossOffer builds a CROSS-CHAIN (BTC<->asset) order-book offer: pair is
// base=asset, quote=the BTC sentinel. The resting CrossChainTerms keys/locktime are
// ADVISORY (display + a stable signed commitment from the maker identity key); the
// load-bearing HTLC keys and CLTVs are minted per-lift over the E2E courier (Phase 2).
// A SELL gives the asset for BTC (taker pays BTC; direction BTC_TO_ASSET); a BUY gives
// BTC for the asset (taker sells the asset; direction ASSET_TO_BTC).
func buildCrossOffer(asset, side string, assetAmt, btcAmt, spendFee uint64, feeAsset string,
	expiry time.Duration, minAnchor uint32, recvAddr, makerPubHex, id string) *seqobv1.Offer {
	isSell := strings.ToLower(side) == "sell"
	direction := offer.DirAssetToBTC
	if isSell {
		direction = offer.DirBTCToAsset
	}
	o := &seqobv1.Offer{
		OfferId:       orDefault(id, randstr.Hex(16)),
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: asset, QuoteAsset: offer.BTCSentinel},
		BaseAmount:    assetAmt,
		AllowPartial:  true, // a taker may take a slice; the serve loop re-rests the remainder
		// min_fill = the smallest base slice whose BTC leg still clears the dust+fee
		// floor, so the book never advertises a partial that would strand a sub-dust
		// HTLC leg. Clamped to <= base_amount (validator requires it); when even the
		// whole offer barely clears the floor it equals base_amount (whole take only).
		MinFill:        client.MinFillBase(btcAmt, assetAmt, spendFee),
		CreatedAtUnix:  uint64(time.Now().Unix()),
		ExpiresAtUnix:  uint64(time.Now().Add(expiry).Unix()),
		FeeAssetHint:   feeAsset,
		MinAnchorDepth: minAnchor,
		Settlement: &seqobv1.Offer_CrossChain{CrossChain: &seqobv1.CrossChainTerms{
			BtcSentinel:      offer.BTCSentinel,
			MakerRecvAddress: recvAddr,
			MakerClaimPub:    makerPubHex,
			MakerRefundPub:   makerPubHex,
			MakerLegLocktime: 144,
			Direction:        direction,
		}},
	}
	if isSell {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = asset, assetAmt
		o.WantAsset, o.WantAmount = offer.BTCSentinel, btcAmt
	} else {
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = offer.BTCSentinel, btcAmt
		o.WantAsset, o.WantAmount = asset, assetAmt
	}
	return o
}

// crossReRestRemainder decides how a settled cross fill re-rests, enforcing the
// minimum-slice floor on the REMAINDER (the covenant CLOB's 'remainder == 0 OR
// remainder >= min_fill'). Given the offer's base_amount, the base atoms actually
// filled, and the offer's min_fill, it returns the remainder to re-rest and
// whether to re-rest at all. reRest is false for a whole fill (nothing left) OR a
// fill that would leave a sub-min_fill dust remainder — in both cases the caller
// drops to a full fill (retire / -requote) rather than re-resting an order whose
// only fills would strand sub-dust HTLC legs. A min_fill of 0 (unset) imposes no
// remainder floor: any non-empty remainder re-rests.
func crossReRestRemainder(base, filledSeq, minFill uint64) (remainder uint64, reRest bool) {
	if filledSeq == 0 || filledSeq >= base {
		return 0, false // whole fill (or nonsensical over-fill): nothing to re-rest
	}
	remainder = base - filledSeq
	if minFill > 0 && remainder < minFill {
		return remainder, false // sub-min_fill dust remainder: drop to a full fill
	}
	return remainder, true
}

// resumeCrossSessions finishes every non-terminal cross session persisted in
// dir after a restart: it reconstructs the legs/keys from each <sid>.json and
// re-enters the on-chain settle loop (claim on the taker's reveal, or refund
// after the CLTV). This is the 2f recovery path — a mid-swap crash or courier
// timeout no longer strands the maker's asset leg. FORWARD sessions only for
// now (the direction served today); reverse resume lands with reverse serving.
func resumeCrossSessions(dir, btcRPCURL, btcWallet, btcChainName, seqRPCURL, seqWallet string, spendFee uint64, btcFeeRate float64) {
	if btcRPCURL == "" || seqRPCURL == "" {
		fatal("-resume requires -btc-rpc and -xseq-rpc")
	}
	btcRPC, err := rpcFromURL(btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	seqRPC, err := rpcFromURL(seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, btcWallet, params)
	btcChain.SetFeeRate(btcFeeRate)
	seqChain := xchain.NewChain(seqRPC, seqWallet)
	drivePendingCrossSessions(dir, btcChain, seqChain, spendFee)
}

// drivePendingCrossSessions scans dir for non-terminal cross sessions and re-drives each to settlement
// (claim on the counterparty's reveal, or refund after CLTV), each in its own goroutine, then waits. Shared
// core of BOTH the explicit -resume one-shot AND the automatic resume pass every serve startup runs — so a
// supervised restart no longer strands a pending session (whose asset HTLC otherwise never gets claimed and
// the counterparty never learns P). Non-fatal: an unreadable dir/record is logged and skipped, never killing
// the caller — the live serving maker depends on that.
func drivePendingCrossSessions(dir string, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain, spendFee uint64) {
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		fmt.Printf("resume: read -xstate-dir %s: %v (nothing to resume)\n", dir, err)
		return
	}
	var pending []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			pending = append(pending, e.Name())
		}
	}
	if len(pending) == 0 {
		fmt.Printf("no cross sessions to resume in %s\n", dir)
		return
	}
	fmt.Printf("resuming %d cross session(s) from %s\n", len(pending), dir)

	var wg sync.WaitGroup
	for _, name := range pending {
		path := filepath.Join(dir, name)
		raw, rerr := ioutil.ReadFile(path)
		if rerr != nil {
			fmt.Printf("%s: read: %v\n", name, rerr)
			continue
		}
		var st xmakerSessionState
		if jerr := json.Unmarshal(raw, &st); jerr != nil {
			fmt.Printf("%s: parse: %v\n", name, jerr)
			continue
		}
		if st.Settled || st.SeqRefundTx != "" || st.BtcRefundTx != "" {
			fmt.Printf("%s: already terminal (settled=%v seqrefund=%s btcrefund=%s); skipping\n", name, st.Settled, st.SeqRefundTx, st.BtcRefundTx)
			continue
		}
		if st.Direction == "reverse" {
			if st.BtcLegTxid == "" || st.BtcLegScriptHex == "" || st.BtcRefundPrivHex == "" || st.SecretHex == "" {
				fmt.Printf("%s: reverse session with no funded BTC leg / secret to recover; skipping\n", name)
				continue
			}
			rp, rerr := resumeParamsFromStateReverse(&st, btcChain, seqChain, spendFee, dir)
			if rerr != nil {
				fmt.Printf("%s: reverse reconstruct: %v\n", name, rerr)
				continue
			}
			wg.Add(1)
			go func(name string, rp client.MakerReverseResumeParams) {
				defer wg.Done()
				fmt.Printf("%s: resuming reverse session (claim asset if funded, else refund BTC after T_btc)\n", name)
				res, rerr := client.ResumeMakerReverse(rp)
				if rerr != nil {
					fmt.Printf("%s: reverse resume ended: %v\n", name, rerr)
					if res != nil && res.BtcRefundTx != "" {
						fmt.Printf("%s: BTC leg refunded in %s\n", name, res.BtcRefundTx)
					}
					return
				}
				fmt.Printf("%s: REVERSE RESUMED + SETTLED: claimed the asset in %s\n", name, res.SeqClaimTxid)
			}(name, rp)
			continue
		}
		if st.SeqLegTxid == "" || st.SeqLegScriptHex == "" || st.BtcClaimPrivHex == "" || st.SeqRefundPrivHex == "" {
			fmt.Printf("%s: no locked SEQ leg / keys to resume (session died before lock); nothing on-chain to settle\n", name)
			continue
		}
		p, perr := resumeParamsFromState(&st, btcChain, seqChain, spendFee, dir)
		if perr != nil {
			fmt.Printf("%s: reconstruct: %v\n", name, perr)
			continue
		}
		wg.Add(1)
		go func(name string, p client.MakerForwardResumeParams) {
			defer wg.Done()
			fmt.Printf("%s: resuming on-chain settle loop\n", name)
			res, rerr := client.ResumeMakerForward(p)
			if rerr != nil {
				fmt.Printf("%s: resume ended: %v\n", name, rerr)
				if res != nil && res.SeqRefundTx != "" {
					fmt.Printf("%s: SEQ leg refunded in %s\n", name, res.SeqRefundTx)
				}
				return
			}
			fmt.Printf("%s: RESUMED + SETTLED: BTC claimed in %s\n", name, res.BtcClaimTxid)
		}(name, p)
	}
	wg.Wait()
	fmt.Println("resume pass complete")
}

// resumeParamsFromState rebuilds the resume params (legs, keys, swap) from a
// persisted session record.
func resumeParamsFromState(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain,
	spendFee uint64, dir string) (client.MakerForwardResumeParams, error) {
	var zero client.MakerForwardResumeParams
	hashH, err := hex.DecodeString(st.HashHex)
	if err != nil || len(hashH) != 32 {
		return zero, fmt.Errorf("bad hash_hex")
	}
	btcClaimBytes, err := hex.DecodeString(st.BtcClaimPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_claim_priv_hex")
	}
	seqRefundBytes, err := hex.DecodeString(st.SeqRefundPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad seq_refund_priv_hex")
	}
	btcScript, err := hex.DecodeString(st.BtcLegScriptHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_leg_script_hex")
	}
	seqScript, err := hex.DecodeString(st.SeqLegScriptHex)
	if err != nil {
		return zero, fmt.Errorf("bad seq_leg_script_hex")
	}
	sid := st.SessionID
	return client.MakerForwardResumeParams{
		Ops: &client.LiveXcOps{
			Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(hashH)),
			BTC:  btcChain, SEQ: seqChain,
		},
		BtcLeg: &xchain.LegLock{
			Script:   btcScript,
			Funded:   &xchain.FundedHTLC{TxID: st.BtcLegTxid, Vout: st.BtcLegVout, Amount: st.BtcLegAmount},
			Locktime: st.BtcLocktime,
		},
		SeqLeg: &xchain.LegLock{
			Script:   seqScript,
			Funded:   &xchain.FundedHTLC{TxID: st.SeqLegTxid, Vout: st.SeqLegVout, Amount: st.SeqLegAmount, AssetID: st.SeqLegAsset},
			Locktime: st.SeqLocktime,
		},
		BtcClaimKey:  xchain.KeyFromBytes(btcClaimBytes),
		SeqRefundKey: xchain.KeyFromBytes(seqRefundBytes),
		HashH:        hashH,
		BtcLocktime:  st.BtcLocktime,
		SeqLocktime:  st.SeqLocktime,
		AssetHex:     st.SeqLegAsset,
		BtcAmount:    st.BtcLegAmount,
		SeqAmount:    st.SeqLegAmount,
		SpendFeeSats: spendFee,
		OnUpdate: func(r *client.MakerForwardResult) {
			persistXSession(dir, sid, st.OfferID, r)
		},
		Log: func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) },
	}, nil
}

// resumeParamsFromStateReverse rebuilds a reverse resume from a persisted record.
// The taker's asset leg may be absent (taker never funded) — resume then just
// refunds our BTC leg after T_btc.
func resumeParamsFromStateReverse(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain,
	spendFee uint64, dir string) (client.MakerReverseResumeParams, error) {
	var zero client.MakerReverseResumeParams
	hashH, err := hex.DecodeString(st.HashHex)
	if err != nil || len(hashH) != 32 {
		return zero, fmt.Errorf("bad hash_hex")
	}
	secret, err := hex.DecodeString(st.SecretHex)
	if err != nil || len(secret) != 32 {
		return zero, fmt.Errorf("bad secret_hex")
	}
	seqClaimBytes, err := hex.DecodeString(st.SeqClaimPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad seq_claim_priv_hex")
	}
	btcRefundBytes, err := hex.DecodeString(st.BtcRefundPrivHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_refund_priv_hex")
	}
	btcScript, err := hex.DecodeString(st.BtcLegScriptHex)
	if err != nil {
		return zero, fmt.Errorf("bad btc_leg_script_hex")
	}
	sid := st.SessionID
	rp := client.MakerReverseResumeParams{
		// The maker holds the secret, so the ops swap is built from it.
		Ops: &client.LiveXcOps{
			Swap: xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLock(secret)),
			BTC:  btcChain, SEQ: seqChain,
		},
		BtcLeg: &xchain.LegLock{
			Script:   btcScript,
			Funded:   &xchain.FundedHTLC{TxID: st.BtcLegTxid, Vout: st.BtcLegVout, Amount: st.BtcLegAmount},
			Locktime: st.BtcLocktime,
		},
		SeqBlockHash: st.SeqBlockHash,
		Secret:       secret,
		HashH:        hashH,
		SeqClaimKey:  xchain.KeyFromBytes(seqClaimBytes),
		BtcRefundKey: xchain.KeyFromBytes(btcRefundBytes),
		BtcLocktime:  st.BtcLocktime,
		SeqLocktime:  st.SeqLocktime,
		AssetHex:     st.SeqLegAsset,
		BtcAmount:    st.BtcLegAmount,
		SeqAmount:    st.SeqLegAmount,
		SpendFeeSats: spendFee,
		OnUpdate: func(r *client.MakerReverseResult) {
			persistXSessionReverse(dir, sid, st.OfferID, r)
		},
		Log: func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) },
	}
	// The taker's asset leg is present only if it was funded + verified.
	if st.SeqLegTxid != "" && st.SeqLegScriptHex != "" {
		seqScript, serr := hex.DecodeString(st.SeqLegScriptHex)
		if serr != nil {
			return zero, fmt.Errorf("bad seq_leg_script_hex")
		}
		rp.SeqLeg = &xchain.LegLock{
			Script:   seqScript,
			Funded:   &xchain.FundedHTLC{TxID: st.SeqLegTxid, Vout: st.SeqLegVout, Amount: st.SeqLegAmount, AssetID: st.SeqLegAsset},
			Locktime: st.SeqLocktime,
		}
	}
	return rp, nil
}

// swapReqAdapter adapts a seqob *seqdexv1.SwapRequest to ports.SwapRequest. The
// seqob request carries no fee asset/amount (the open fee market is resolved
// inside CompleteSwap), so those return zero values; *seqdexv1.UnblindedInput
// already satisfies ports.UnblindedInput.
type swapReqAdapter struct{ r *seqdexv1.SwapRequest }

func (a swapReqAdapter) GetId() string          { return a.r.GetId() }
func (a swapReqAdapter) GetAssetP() string      { return a.r.GetAssetP() }
func (a swapReqAdapter) GetAmountP() uint64     { return a.r.GetAmountP() }
func (a swapReqAdapter) GetAssetR() string      { return a.r.GetAssetR() }
func (a swapReqAdapter) GetAmountR() uint64     { return a.r.GetAmountR() }
func (a swapReqAdapter) GetTransaction() string { return a.r.GetTransaction() }
func (a swapReqAdapter) GetFeeAsset() string    { return "" }
func (a swapReqAdapter) GetFeeAmount() uint64   { return 0 }
func (a swapReqAdapter) GetUnblindedInputs() []ports.UnblindedInput {
	src := a.r.GetUnblindedInputs()
	out := make([]ports.UnblindedInput, 0, len(src))
	for _, u := range src {
		out = append(out, u)
	}
	return out
}

// utxosToSwapUnblinded converts the maker's CompleteSwap-selected utxos to the
// swap.UnblindedInput list for the SwapAccept, using the same index convention as
// the proven trade path (trading.go).
func utxosToSwapUnblinded(utxos []ports.Utxo) []swap.UnblindedInput {
	ins := make([]swap.UnblindedInput, 0, len(utxos))
	for i, u := range utxos {
		ins = append(ins, swap.UnblindedInput{
			Index:         uint32(i),
			Asset:         u.GetAsset(),
			Amount:        u.GetValue(),
			AssetBlinder:  u.GetAssetBlinder(),
			AmountBlinder: u.GetValueBlinder(),
		})
	}
	return ins
}

func loadOrGenKey(hexKey string) *btcec.PrivateKey {
	if hexKey == "" {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			fatal("gen key: %v", err)
		}
		fmt.Printf("generated maker key: priv=%s pub=%s\n",
			hex.EncodeToString(k.Serialize()), hex.EncodeToString(k.PubKey().SerializeCompressed()))
		return k
	}
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		fatal("-maker-priv must be 32-byte hex")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}

// wsWriteMu serializes WS writes: cross-mode lift sessions courier from their
// own goroutines and gorilla/websocket allows only one concurrent writer.
var wsWriteMu sync.Mutex

// writeWS returns the write error (rather than exiting) so the same-chain serve loop can reconnect
// and re-post instead of dying — dying would let the supervisor restart us, which churns the offer_id.
func writeWS(c *websocket.Conn, to *seqobv1.To) error {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		return err
	}
	wsWriteMu.Lock()
	defer wsWriteMu.Unlock()
	return c.WriteMessage(websocket.TextMessage, b)
}

// --- Cross-chain (BTC<->asset) maker -----------------------------------------

type crossMakerConfig struct {
	relay        string
	makerKey     *btcec.PrivateKey
	makerPubHex  string
	asset, side  string
	assetAmt     uint64
	btcAmt       uint64
	feeAsset     string
	expiry       time.Duration
	minAnchor    uint32
	offerID      string
	btcRPCURL    string
	btcWallet    string
	btcChainName string
	seqRPCURL    string
	seqWallet    string
	btcDelta     uint32
	seqDelta     uint32
	minBTCConf   int
	spendFee     uint64
	stateDir     string
	btcFeeRate   float64
	requote      bool
}

// runCrossMaker posts a cross-chain offer and serves forward lifts with the
// xdriver over pkg/xchain. It needs no Ocean wallet: the SEQ asset leg is
// funded from the Sequentia NODE wallet and the claimed BTC lands in the
// bitcoind wallet — the same reserves the RFQ maker uses (do not re-fund).
func runCrossMaker(cfg crossMakerConfig) {
	sideL := strings.ToLower(cfg.side)
	if sideL != "sell" && sideL != "buy" {
		fatal("-mode cross -side must be sell (taker pays BTC) or buy (taker sells the asset)")
	}
	if cfg.btcRPCURL == "" || cfg.seqRPCURL == "" {
		fatal("-mode cross requires -btc-rpc and -xseq-rpc")
	}
	btcRPC, err := rpcFromURL(cfg.btcRPCURL)
	if err != nil {
		fatal("-btc-rpc: %v", err)
	}
	seqRPC, err := rpcFromURL(cfg.seqRPCURL)
	if err != nil {
		fatal("-xseq-rpc: %v", err)
	}
	params, err := xchain.BitcoinChainParams(cfg.btcChainName)
	if err != nil {
		fatal("-btc-chain: %v", err)
	}
	btcChain := xchain.NewBitcoinChain(btcRPC, cfg.btcWallet, params)
	btcChain.SetFeeRate(cfg.btcFeeRate)
	seqChain := xchain.NewChain(seqRPC, cfg.seqWallet)

	// Sanity: both nodes reachable before we advertise anything.
	if _, err := btcChain.BlockCount(); err != nil {
		fatal("bitcoind unreachable: %v", err)
	}
	if _, err := seqChain.BlockCount(); err != nil {
		fatal("sequentia node unreachable: %v", err)
	}

	// Advisory receive address for the resting offer, from the node wallet.
	var recvAddr string
	if err := seqChain.RPC().Call(&recvAddr, "getnewaddress"); err != nil {
		fatal("getnewaddress on the Sequentia wallet: %v", err)
	}

	o := buildCrossOffer(cfg.asset, cfg.side, cfg.assetAmt, cfg.btcAmt, cfg.spendFee, cfg.feeAsset,
		cfg.expiry, cfg.minAnchor, recvAddr, cfg.makerPubHex, cfg.offerID)
	if err := offer.SignOffer(o, cfg.makerKey); err != nil {
		fatal("sign offer: %v", err)
	}

	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		fatal("create -xstate-dir %s: %v", cfg.stateDir, err)
	}
	// NOTE: pending-session resume is deliberately NOT done per-maker here. The fleet shares ONE
	// -xstate-dir, so having every serving maker scan+resume on startup would make 144 makers herd on the
	// same sessions (redundant claims/refunds, broadcast conflicts). Recovery is driven instead by a SINGLE
	// dedicated `-mode cross -resume` settler loop (drivePendingCrossSessions, via resumeCrossSessions) that
	// re-drives every non-terminal session to claim-or-refund. That is what unstrands a maker cycled by the
	// supervisor between recording the taker's asset leg and claiming it — the stall that blocked the E2E.

	wsURL := "ws" + strings.TrimPrefix(cfg.relay, "http") + "/v1/ws"
	ws := &crossWS{}
	if err := ws.redial(wsURL, o); err != nil {
		fatal("dial ws %s: %v", wsURL, err)
	}
	fmt.Printf("seqob-maker up (CROSS): posted %s offer %s by maker %s\n", cfg.side, o.GetOfferId(), cfg.makerPubHex)
	fmt.Printf("  %d %s <- %d BTC sats  T_btc=+%d T_seq=+%d min-conf=%d\n",
		cfg.assetAmt, cfg.asset, cfg.btcAmt, cfg.btcDelta, cfg.seqDelta, cfg.minBTCConf)
	fmt.Printf("  taker lifts with: seqob-cli xlift -offer-id %s -maker-pubkey %s\n", o.GetOfferId(), cfg.makerPubHex)

	serveCross(ws, wsURL, o, cfg, btcChain, seqChain)
}

// crossWS holds the (re)dialable relay connection: cross sessions run for many
// minutes in their own goroutines, so writes are serialized here and a WS drop
// reconnects instead of killing in-flight ON-CHAIN settlements.
type crossWS struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *crossWS) current() *websocket.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

func (w *crossWS) write(to *seqobv1.To) error {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("ws not connected")
	}
	return w.conn.WriteMessage(websocket.TextMessage, b)
}

// redial dials and, when an offer is given, (re)submits it so the relay
// re-registers this connection as the maker's lift route.
func (w *crossWS) redial(wsURL string, o *seqobv1.Offer) error {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if w.conn != nil {
		_ = w.conn.Close()
	}
	w.conn = conn
	w.mu.Unlock()
	if o != nil {
		return w.write(&seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	}
	return nil
}

func (w *crossWS) redialLoop(wsURL string, o *seqobv1.Offer) {
	for {
		if err := w.redial(wsURL, o); err == nil {
			fmt.Println("relay reconnected")
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// serveCross is the cross-mode event loop: each lift gets its own goroutine
// running RunMakerForward; the loop routes sealed courier frames to the
// session's inbox. Whole-HTLC discipline: ONE lift in flight at a time, and
// the offer is cancelled after its first settlement (no fill accounting exists
// for cross offers, so serving further lifts would oversell the signed size at
// a stale price; restart the maker to re-quote).
func serveCross(ws *crossWS, wsURL string, o *seqobv1.Offer, cfg crossMakerConfig,
	btcChain *xchain.BitcoinChain, seqChain *xchain.Chain) {
	var mu sync.Mutex
	inboxes := make(map[string]chan []byte)
	inFlight := 0
	filled := false
	idle := func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == 0 }
	armRequoteExit(o, idle) // retire for a fresh re-quote before the offer expires

	refuse := func(sid string, cr *client.Crypter, code, msg string) {
		m := &client.XcMsg{Type: client.XcFail, Code: code, Message: msg}
		if sealed, err := m.Seal(cr); err == nil {
			_ = ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
		}
	}

	for {
		conn := ws.current()
		_, data, err := conn.ReadMessage()
		if err != nil {
			mu.Lock()
			done := filled && inFlight == 0
			resubmit := o
			if filled {
				resubmit = nil // never re-advertise a filled offer
			}
			mu.Unlock()
			if done {
				fmt.Println("offer filled and no lift in flight; exiting (restart to re-quote)")
				return
			}
			// The relay EVICTS a maker's offers when its WS drops (an offline maker can't
			// co-sign — ws.go closeConn -> RemoveByMaker), so on reconnect we must re-post.
			// Refresh the offer's created/expiry window + re-sign FIRST: otherwise the relay
			// rejects the stale copy as "invalid offer: offer already expired" and the offer
			// never comes back — the cross book stays empty until a manual fleet restart (the
			// fragility a relay restart exposes). Refreshing revives an expired offer and is a
			// harmless expiry-extension for a still-valid one.
			if resubmit != nil {
				if e := refreshOfferForRequote(resubmit, cfg.expiry, cfg.makerKey); e != nil {
					fmt.Printf("reconnect: re-sign offer failed: %v (relay may reject re-post)\n", e)
				}
			}
			fmt.Printf("ws read error: %v; reconnecting (in-flight settlements continue on-chain)\n", err)
			ws.redialLoop(wsURL, resubmit)
			continue
		}
		var from seqobv1.From
		if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
			continue
		}
		switch {
		case from.GetLiftRequested() != nil:
			lr := from.GetLiftRequested()
			sid := lr.GetSessionId()
			cr, err := client.NewMakerCrypterFromLift(cfg.makerKey, lr.GetTakerSessionPubkey())
			if err != nil {
				fmt.Printf("lift %s: crypter error: %v\n", sid, err)
				continue
			}
			mu.Lock()
			busy, done := inFlight > 0, filled
			var in chan []byte
			if !busy && !done {
				inFlight++
				in = make(chan []byte, 8)
				inboxes[sid] = in
			}
			mu.Unlock()
			if done {
				refuse(sid, cr, "offer_filled", "offer already filled; awaiting re-quote")
				continue
			}
			if busy {
				refuse(sid, cr, "busy", "another lift is in flight (whole-HTLC, one at a time)")
				continue
			}
			if lr.GetTakeAmount() > o.GetBaseAmount() {
				fmt.Printf("lift %s: take %d > offer %d; the driver will reject the over-ask\n",
					sid, lr.GetTakeAmount(), o.GetBaseAmount())
			}
			fmt.Printf("cross lift requested: session %s offer %s (take %d of %d)\n",
				sid, lr.GetOfferId(), lr.GetTakeAmount(), o.GetBaseAmount())

			send := func(sealed []byte) error {
				return ws.write(&seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sid, Ciphertext: sealed}}})
			}
			logf := func(format string, args ...interface{}) { fmt.Printf("session "+sid+": "+format+"\n", args...) }
			newOpsFromHash := func(hashH []byte) (client.XcOps, error) {
				swp := xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLockFromHash(hashH))
				return &client.LiveXcOps{Swap: swp, BTC: btcChain, SEQ: seqChain}, nil
			}
			reverse := o.GetCrossChain().GetDirection() == offer.DirAssetToBTC

			go func(sid string, in chan []byte, reverse bool) {
				settled := false
				var settleTxid string
				var filledSeq, filledBtc uint64
				defer func() {
					// Record the executed fill FIRST, before the partial-remainder re-rest or the
					// requote/cancel mutate/remove the still-resting offer: the relay records the
					// trade (price/size for the BTC pair's last_price + chart) at the ORIGINAL price
					// and decrements by exactly filledSeq (the base atoms actually taken; a cross lift
					// may be partial). Maker-signed + offer-keyed, so it lands with the session gone.
					if settled {
						reportSettledTrade(ws, o, cfg.makerKey, filledSeq, settleTxid)
					}
					// Cross legs are on-chain (no LN channel peers), so the pre-quote "reconnect" is a
					// reachability ping of both backends.
					reconnect := func() {
						if _, err := btcChain.BlockCount(); err != nil {
							fmt.Printf("requote: bitcoind unreachable before re-quote: %v\n", err)
						}
						if _, err := seqChain.BlockCount(); err != nil {
							fmt.Printf("requote: sequentia node unreachable before re-quote: %v\n", err)
						}
					}
					// Partial fill: the taker took only a slice, so reduce the offer to the remainder
					// and re-quote (there is more to trade, regardless of -requote); requoteAfterFill
					// re-signs + re-posts the shrunk offer. A whole fill keeps the old behaviour
					// (-requote re-posts the whole; otherwise the offer is retired). The remainder is
					// priced so the maker NEVER over-commits its offer's capital across a full sweep:
					//   - forward (SELL, maker receives BTC): re-quote WantAmount at the offer's own
					//     rate (ProportionalBtc, ceil) — the maker asks >= its rate on the remainder,
					//     total received >= the offer, capped by the asset it actually gives.
					//   - reverse (BUY, maker gives BTC): SUBTRACT the exact filled sats, so the sum of
					//     partials commits at most the offer's original BTC (a proportional re-quote
					//     would round each partial up and over-commit).
					// Minimum-slice floor on the REMAINDER: mirror the covenant CLOB's
					// 'remainder == 0 OR remainder >= min_fill'. crossReRestRemainder returns
					// reRest=false for a whole fill OR a fill that would leave a sub-min_fill dust
					// remainder — re-resting such a remainder would advertise an order only sub-dust
					// partials could fill, so drop to a full fill (retire / -requote) instead.
					remainAsset, canReRest := crossReRestRemainder(o.GetBaseAmount(), filledSeq, o.GetMinFill())
					partial := settled && canReRest
					if partial && reverse && filledBtc >= o.GetOfferAmount() {
						// Defensive: a partial that somehow consumed the whole BTC budget leaves no
						// capital to re-rest; treat it as a full fill (retire/-requote) rather than
						// re-posting a 0-BTC offer.
						partial = false
					}
					if partial {
						mu.Lock()
						if reverse {
							// BUY offer: OfferAmount=BTC (given), WantAmount=asset (wanted), Base=asset.
							o.OfferAmount = o.GetOfferAmount() - filledBtc
							o.BaseAmount = remainAsset
							o.WantAmount = remainAsset
							// Re-derive min_fill for the shrunk BUY offer (price is ~unchanged, so this
							// stays consistent and never exceeds the new base_amount).
							o.MinFill = client.MinFillBase(o.GetOfferAmount(), remainAsset, cfg.spendFee)
						} else {
							// SELL offer: OfferAmount=asset (given), WantAmount=BTC (wanted), Base=asset.
							// Price the remainder at the offer's OWN rate (ceil), not by subtracting the
							// rounded filledBtc — that subtraction drifts and can zero the want on a
							// low-priced offer. ProportionalBtc rounds up, so remainAsset>0 => want>=1.
							o.WantAmount = client.ProportionalBtc(o.GetWantAmount(), remainAsset, o.GetBaseAmount())
							o.BaseAmount = remainAsset
							o.OfferAmount = remainAsset
							// Re-derive min_fill for the shrunk SELL offer against its new BTC want.
							o.MinFill = client.MinFillBase(o.GetWantAmount(), remainAsset, cfg.spendFee)
						}
						mu.Unlock()
						fmt.Printf("session %s: PARTIAL fill (%d %s, %d sats); re-resting the remainder %d %s (offer %d, want %d)\n",
							sid, filledSeq, o.GetPair().GetBaseAsset(), filledBtc, remainAsset,
							o.GetPair().GetBaseAsset(), o.GetOfferAmount(), o.GetWantAmount())
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, reconnect)
					} else if settled && cfg.requote {
						// -requote: re-post a fresh quote WHILE still holding the in-flight slot, so the
						// serve loop refuses any racing lift as "busy" until the new offer is live.
						requoteAfterFill(ws, wsURL, o, cfg.relay, cfg.makerKey, cfg.expiry, reconnect)
					}
					mu.Lock()
					inFlight--
					delete(inboxes, sid)
					if settled && !partial && !cfg.requote {
						filled = true
					}
					mu.Unlock()
					if settled && !partial && !cfg.requote {
						cancelOffer(cfg.relay, o, cfg.makerKey)
					}
					requoteExitIfPending(idle)
				}()
				if reverse {
					// BUY: the maker holds the secret and funds the BTC leg first.
					p := client.MakerReverseParams{
						NewOps: func(secret []byte) (client.XcOps, error) {
							swp := xchain.NewSwapBitcoin(btcChain, seqChain, xchain.NewHashLock(secret))
							return &client.LiveXcOps{Swap: swp, BTC: btcChain, SEQ: seqChain}, nil
						},
						Crypter: cr, BtcTip: btcChain.BlockCount, SeqTip: seqChain.BlockCount,
						AssetHex: o.GetPair().GetBaseAsset(), SeqAmount: o.GetWantAmount(), BtcAmount: o.GetOfferAmount(),
						BtcLocktimeDelta: cfg.btcDelta, SeqLocktimeDelta: cfg.seqDelta,
						MinBTCConf: cfg.minBTCConf, SpendFeeSats: cfg.spendFee, Log: logf,
						OnUpdate: func(r *client.MakerReverseResult) { persistXSessionReverse(cfg.stateDir, sid, o.GetOfferId(), r) },
					}
					res, err := client.RunMakerReverse(p, in, send)
					if err != nil {
						fmt.Printf("session %s: reverse cross lift ended: %v\n", sid, err)
						if res != nil && res.BtcRefundTx != "" {
							fmt.Printf("session %s: BTC leg refunded in %s\n", sid, res.BtcRefundTx)
						}
						return
					}
					settled = true
					settleTxid = res.SeqClaimTxid
					filledSeq, filledBtc = res.FilledSeq, res.FilledBtc
					fmt.Printf("session %s: REVERSE CROSS SWAP SETTLED: bought %d %s for %d sats, claimed the asset in %s\n",
						sid, res.FilledSeq, o.GetPair().GetBaseAsset(), res.FilledBtc, res.SeqClaimTxid)
					return
				}
				// SELL (forward): the taker pays BTC and holds the secret.
				p := client.MakerForwardParams{
					NewOps: newOpsFromHash, Crypter: cr,
					BtcTip: btcChain.BlockCount, SeqTip: seqChain.BlockCount,
					AssetHex: o.GetPair().GetBaseAsset(), SeqAmount: o.GetOfferAmount(), BtcAmount: o.GetWantAmount(),
					BtcLocktimeDelta: cfg.btcDelta, SeqLocktimeDelta: cfg.seqDelta,
					MinBTCConf: cfg.minBTCConf, SpendFeeSats: cfg.spendFee, Log: logf,
					OnUpdate: func(r *client.MakerForwardResult) { persistXSession(cfg.stateDir, sid, o.GetOfferId(), r) },
				}
				res, err := client.RunMakerForward(p, in, send)
				if err != nil {
					fmt.Printf("session %s: cross lift ended: %v\n", sid, err)
					if res != nil && res.SeqRefundTx != "" {
						fmt.Printf("session %s: SEQ leg refunded in %s\n", sid, res.SeqRefundTx)
					}
					return
				}
				settled = true
				settleTxid = res.BtcClaimTxid
				filledSeq, filledBtc = res.FilledSeq, res.FilledBtc
				fmt.Printf("session %s: CROSS SWAP SETTLED: sold %d %s for %d sats, taker claimed the asset, BTC claimed in %s\n",
					sid, res.FilledSeq, o.GetPair().GetBaseAsset(), res.FilledBtc, res.BtcClaimTxid)
			}(sid, in, reverse)

		case from.GetSwapMsg() != nil:
			sm := from.GetSwapMsg()
			mu.Lock()
			in := inboxes[sm.GetSessionId()]
			mu.Unlock()
			if in == nil {
				fmt.Printf("session %s: swap_msg without a live cross session; ignoring\n", sm.GetSessionId())
				continue
			}
			select {
			case in <- sm.GetCiphertext():
			default:
				fmt.Printf("session %s: inbox full; dropping frame\n", sm.GetSessionId())
			}

		case from.GetOrderStatus() != nil:
			st := from.GetOrderStatus()
			fmt.Printf("order %s status=%s active=%d txid=%s\n",
				st.GetOfferId(), st.GetStatus(), st.GetActiveAmount(), st.GetSettleTxid())

		case from.GetError() != nil:
			e := from.GetError()
			fmt.Printf("relay error %d: %s\n", e.GetCode(), e.GetMessage())
			if offerRejected(e.GetCode(), e.GetMessage()) {
				requoteExitIfIdle("relay rejected our offer ("+e.GetMessage()+")", idle)
			}
		}
	}
}

// xmakerSessionState is the on-disk snapshot of one cross lift: with it, an
// operator can refund the SEQ leg after T_seq or claim the BTC leg with a
// learned secret even if the process died mid-swap.
type xmakerSessionState struct {
	SessionID        string `json:"session_id"`
	OfferID          string `json:"offer_id"`
	Direction        string `json:"direction,omitempty"` // "forward" (sell) | "reverse" (buy)
	HashHex          string `json:"hash_hex,omitempty"`
	BtcClaimPrivHex  string `json:"btc_claim_priv_hex,omitempty"`  // forward: claims taker BTC
	SeqRefundPrivHex string `json:"seq_refund_priv_hex,omitempty"` // forward: refunds our SEQ leg
	// reverse: the maker holds the secret, claims the taker's SEQ leg, refunds its own BTC leg
	SecretForRefundHex string `json:"secret_for_refund_hex,omitempty"` // reverse: same as secret_hex, named for clarity
	SeqClaimPrivHex    string `json:"seq_claim_priv_hex,omitempty"`    // reverse: claims taker SEQ
	BtcRefundPrivHex   string `json:"btc_refund_priv_hex,omitempty"`   // reverse: refunds our BTC leg
	BtcRefundTx        string `json:"btc_refund_tx,omitempty"`         // reverse
	BtcLocktime        uint32 `json:"btc_locktime,omitempty"`
	SeqLocktime        uint32 `json:"seq_locktime,omitempty"`
	BtcLegTxid         string `json:"btc_leg_txid,omitempty"`
	BtcLegVout         uint32 `json:"btc_leg_vout"`
	BtcLegAmount       uint64 `json:"btc_leg_amount,omitempty"`
	BtcLegScriptHex    string `json:"btc_leg_script_hex,omitempty"`
	SeqLegTxid         string `json:"seq_leg_txid,omitempty"`
	SeqLegVout         uint32 `json:"seq_leg_vout"`
	SeqLegAmount       uint64 `json:"seq_leg_amount,omitempty"`
	SeqLegAsset        string `json:"seq_leg_asset,omitempty"`
	SeqLegScriptHex    string `json:"seq_leg_script_hex,omitempty"`
	SeqBlockHash       string `json:"seq_block_hash,omitempty"`
	SecretHex          string `json:"secret_hex,omitempty"`
	BtcClaimTxid       string `json:"btc_claim_txid,omitempty"` // forward: maker claimed the taker's BTC
	SeqRefundTx        string `json:"seq_refund_tx,omitempty"`  // forward: maker refunded its SEQ leg
	SeqClaimTxid       string `json:"seq_claim_txid,omitempty"` // reverse: maker claimed the taker's SEQ
	Settled            bool   `json:"settled"`
	UpdatedAt          string `json:"updated_at"`
}

func persistXSession(dir, sid, offerID string, r *client.MakerForwardResult) {
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "forward",
		BtcLocktime: r.BtcLocktime, SeqLocktime: r.SeqLocktime,
		SeqBlockHash: r.SeqBlockHash,
		BtcClaimTxid: r.BtcClaimTxid, SeqRefundTx: r.SeqRefundTx,
		Settled:   r.Settled,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if r.BtcClaimKey != nil {
		st.BtcClaimPrivHex = hex.EncodeToString(r.BtcClaimKey.Bytes())
	}
	if r.SeqRefundKey != nil {
		st.SeqRefundPrivHex = hex.EncodeToString(r.SeqRefundKey.Bytes())
	}
	if r.BtcLeg != nil && r.BtcLeg.Funded != nil {
		st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount = r.BtcLeg.Funded.TxID, r.BtcLeg.Funded.Vout, r.BtcLeg.Funded.Amount
		st.BtcLegScriptHex = hex.EncodeToString(r.BtcLeg.Script)
	}
	if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
		st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = r.SeqLeg.Funded.TxID, r.SeqLeg.Funded.Vout, r.SeqLeg.Funded.Amount
		st.SeqLegAsset = r.SeqLeg.Funded.AssetID
		st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
	}
	if len(r.Secret) > 0 {
		st.SecretHex = hex.EncodeToString(r.Secret)
	}
	b, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		fmt.Printf("session %s: persist marshal: %v\n", sid, err)
		return
	}
	if err := ioutil.WriteFile(filepath.Join(dir, sid+".json"), b, 0o600); err != nil {
		fmt.Printf("session %s: persist write: %v\n", sid, err)
	}
}

// persistXSessionReverse snapshots a reverse (buy) cross lift. The maker holds
// the secret and funds the BTC leg, so the recovery material is the secret plus
// the seq-claim / btc-refund keys and both legs.
func persistXSessionReverse(dir, sid, offerID string, r *client.MakerReverseResult) {
	st := xmakerSessionState{
		SessionID: sid, OfferID: offerID, Direction: "reverse",
		BtcLocktime: r.BtcLocktime, SeqLocktime: r.SeqLocktime,
		SeqBlockHash: r.SeqBlockHash,
		SeqClaimTxid: r.SeqClaimTxid, BtcRefundTx: r.BtcRefundTx,
		Settled:   r.Settled,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if len(r.HashH) > 0 {
		st.HashHex = hex.EncodeToString(r.HashH)
	}
	if len(r.Secret) > 0 {
		st.SecretHex = hex.EncodeToString(r.Secret)
		st.SecretForRefundHex = st.SecretHex
	}
	if r.SeqClaimKey != nil {
		st.SeqClaimPrivHex = hex.EncodeToString(r.SeqClaimKey.Bytes())
	}
	if r.BtcRefundKey != nil {
		st.BtcRefundPrivHex = hex.EncodeToString(r.BtcRefundKey.Bytes())
	}
	if r.BtcLeg != nil && r.BtcLeg.Funded != nil {
		st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount = r.BtcLeg.Funded.TxID, r.BtcLeg.Funded.Vout, r.BtcLeg.Funded.Amount
		st.BtcLegScriptHex = hex.EncodeToString(r.BtcLeg.Script)
	}
	if r.SeqLeg != nil && r.SeqLeg.Funded != nil {
		st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount = r.SeqLeg.Funded.TxID, r.SeqLeg.Funded.Vout, r.SeqLeg.Funded.Amount
		st.SeqLegAsset = r.SeqLeg.Funded.AssetID
		st.SeqLegScriptHex = hex.EncodeToString(r.SeqLeg.Script)
	}
	b, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		fmt.Printf("session %s: persist marshal: %v\n", sid, err)
		return
	}
	if err := ioutil.WriteFile(filepath.Join(dir, sid+".json"), b, 0o600); err != nil {
		fmt.Printf("session %s: persist write: %v\n", sid, err)
	}
}

// reportSettledTrade tells the relay a CROSS-CHAIN / LIGHTNING lift SETTLED, so the relay
// records the trade (feeding last_price / candles / the trades feed for BTC/LN pairs, which
// otherwise report last_price 0 with an empty chart) and decrements/finalizes the resting
// order. It is the durable-settlement analogue of the same-chain maker's SettleAck, but it is
// MAKER-SIGNED and keyed to the offer instead of a live session: a cross/LN swap can finish
// after the short co-sign session was swept (its on-chain / Lightning leg outlives the courier
// deadline) or after the maker reconnected (dropping the relay-side session role), so a
// session-scoped ack is unreachable at settlement time.
//
// anchorConfs is the Bitcoin-anchor depth the maker declares final for this fill: reporting the
// offer's own min_anchor_depth marks the maker's chosen finality bar met (the maker only reaches
// this settled state after its swap-level confirmation gates hold), so a min_anchor_depth>0 offer
// reaches FILLED; a later anchor orphan re-opens it (handled separately). Pure-LN offers carry
// min_anchor_depth 0, so their fills finalize immediately. Best-effort: a send failure is logged.
func reportSettledTrade(ws *crossWS, o *seqobv1.Offer, key *btcec.PrivateKey, fillBase uint64, settleTxid string) {
	if fillBase == 0 {
		return
	}
	st := &seqobv1.SettledTrade{
		OfferId:     o.GetOfferId(),
		MakerPubkey: o.GetMakerPubkey(),
		FillBase:    fillBase,
		SettleTxid:  settleTxid,
		AnchorConfs: o.GetMinAnchorDepth(),
		Nonce:       uint64(time.Now().UnixNano()),
	}
	if err := offer.SignSettledTrade(st, key); err != nil {
		fmt.Printf("settled-trade: sign failed: %v (trade NOT recorded)\n", err)
		return
	}
	if err := ws.write(&seqobv1.To{Msg: &seqobv1.To_SettledTrade{SettledTrade: st}}); err != nil {
		fmt.Printf("settled-trade: relay write failed: %v (trade NOT recorded)\n", err)
	}
}

// cancelOffer removes the filled resting offer from the book (signed cancel).
// This is the NON-requote terminal path: the maker quotes once and exits.
func cancelOffer(relay string, o *seqobv1.Offer, key *btcec.PrivateKey) {
	if err := postCancel(relay, o, key); err != nil {
		fmt.Printf("cancel offer: %v\n", err)
		return
	}
	fmt.Printf("offer %s cancelled after fill (restart the maker to re-quote)\n", o.GetOfferId())
}

// postCancel signs and POSTs an offer cancel to the relay, returning only when the
// relay has processed it (HTTP 200 => the offer is removed from the book). The
// nonce is the wall-clock nanosecond, which is strictly increasing across calls,
// so each cancel beats the store's replay guard and a re-posted same offer_id can
// be cancelled again on the next fill.
func postCancel(relay string, o *seqobv1.Offer, key *btcec.PrivateKey) error {
	c := &seqobv1.OfferCancel{OfferId: o.GetOfferId(), Nonce: uint64(time.Now().UnixNano())}
	if err := offer.SignCancel(c, key); err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	b, err := jsonMarshal.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	resp, err := http.Post(relay+"/v1/offers/cancel", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// refreshOfferForRequote rolls an offer forward for a fresh quote: it moves the
// created/expires window to [now, now+expiry) and re-signs it (the signature
// covers those fields), leaving the offer_id, pair, amounts and settlement terms
// untouched. Returns the error from re-signing, if any. It is deliberately pure
// (no I/O) so the re-quote refresh is unit-testable on its own.
func refreshOfferForRequote(o *seqobv1.Offer, expiry time.Duration, key *btcec.PrivateKey) error {
	now := time.Now()
	o.CreatedAtUnix = uint64(now.Unix())
	o.ExpiresAtUnix = uint64(now.Add(expiry).Unix())
	return offer.SignOffer(o, key)
}

// requoteAfterFill keeps a maker's quote continuously live after a settled fill,
// replacing the old cancel-and-exit behaviour when -requote is set. It is called
// from a settle goroutine's defer WHILE that goroutine still holds the single
// in-flight slot, so the serve loop refuses any new lift as "busy" until the fresh
// quote is up: the maker can neither double-post (there is never a second offer_id)
// nor oversell (only one offer rests at a time, at full size). Steps:
//
//  1. reconnect any dropped channel peers so the next leg can actually route
//     (the maker's asset/BTC LN nodes silently drop peers between fills);
//  2. refresh the offer's created/expiry window and re-sign it;
//  3. cancel the just-filled resting offer, then re-submit the SAME offer_id.
//
// Cancel-then-resubmit (rather than an in-place edit) is used on purpose: it is
// robust even if the relay's expiry sweeper already reaped the offer between the
// lift and settlement — the cancel then simply no-ops and the submit re-adds it —
// and the submit path re-registers this connection as the maker's lift endpoint.
// The cancel is synchronous (returns only after HTTP 200 => removed), so the
// submit that follows can never collide with the old resting copy.
func requoteAfterFill(ws *crossWS, wsURL string, o *seqobv1.Offer, relay string, key *btcec.PrivateKey, expiry time.Duration, reconnect func()) {
	if reconnect != nil {
		reconnect()
	}
	if err := refreshOfferForRequote(o, expiry, key); err != nil {
		fmt.Printf("requote: re-sign failed: %v (offer NOT re-posted)\n", err)
		return
	}
	// Remove the filled copy first (idempotent if already swept), then re-post.
	if err := postCancel(relay, o, key); err != nil {
		fmt.Printf("requote: cancel of filled offer failed (continuing to re-post): %v\n", err)
	}
	if err := ws.write(&seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}}); err != nil {
		fmt.Printf("requote: re-submit failed: %v; reconnecting relay + re-posting\n", err)
		ws.redialLoop(wsURL, o)
	}
	fmt.Printf("re-quoted: offer %s live again (%d %s), fresh expiry; awaiting the next lift\n",
		o.GetOfferId(), o.GetBaseAmount(), o.GetOfferAsset())
}

// rpcFromURL parses http://user:pass@host:port into an xchain RPC client.
func rpcFromURL(raw string) (*xchain.RPC, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.User == nil {
		return nil, fmt.Errorf("expected http://user:pass@host:port, got %q", raw)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return nil, fmt.Errorf("bad port in %q", raw)
	}
	pass, _ := u.User.Password()
	return xchain.NewRPC(u.Hostname(), port, u.User.Username(), pass), nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}
