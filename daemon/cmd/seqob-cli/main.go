// Command seqob-cli drives the SeqOB relay: post a signed offer, list a market's
// book, and lift an offer. The lift path drives internal/seqob/client (the
// taker proposer + the E2E courier) against a running seqobd. Phase-1 settlement
// uses the in-memory StubWallet, so a lift builds and couriers an encrypted
// SwapRequest; completing it requires a maker process (see internal/seqob/client).
package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"github.com/thanhpk/randstr"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

var jsonMarshal = protojson.MarshalOptions{UseProtoNames: true}
var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "post":
		cmdPost(os.Args[2:])
	case "book":
		cmdBook(os.Args[2:])
	case "lift":
		cmdLift(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `seqob-cli <command> [flags]

commands:
  post   post a signed offer        (flags: -relay -priv -base -quote -dir -base-amount -quote-amount -expiry -fee-asset -recv-addr -id)
  book   list a market's order book (flags: -relay -base -quote)
  lift   lift a resting offer       (flags: -relay -base -quote -offer-id -maker-pubkey -amount -priv -fee-asset)`)
	os.Exit(2)
}

// --- post ---

func cmdPost(args []string) {
	fs := newFlagSet("post")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	priv := fs.String("priv", "", "maker secret key (32-byte hex); generated if empty")
	base := fs.String("base", "gold", "base asset id")
	quote := fs.String("quote", "usdx", "quote asset id")
	dir := fs.String("dir", "sell", "trade direction: sell|buy")
	baseAmt := fs.Uint64("base-amount", 100, "base size (base atoms)")
	quoteAmt := fs.Uint64("quote-amount", 45, "quote size (quote atoms)")
	expiry := fs.Duration("expiry", time.Hour, "time until the offer expires")
	feeAsset := fs.String("fee-asset", "", "preferred fee asset hint (any-asset fee market)")
	recvAddr := fs.String("recv-addr", "el1qq-demo-recv-addr", "maker confidential receive address")
	id := fs.String("id", "", "offer id (random 16-byte hex if empty)")
	_ = fs.Parse(args)

	k := loadOrGenKey(*priv)
	o := &seqobv1.Offer{
		OfferId:       orDefault(*id, randstr.Hex(16)),
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: *base, QuoteAsset: *quote},
		BaseAmount:    *baseAmt,
		AllowPartial:  true,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(*expiry).Unix()),
		FeeAssetHint:  *feeAsset,
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: *recvAddr}},
	}
	switch strings.ToLower(*dir) {
	case "sell":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_SELL
		o.OfferAsset, o.OfferAmount = *base, *baseAmt
		o.WantAsset, o.WantAmount = *quote, *quoteAmt
	case "buy":
		o.TradeDir = seqobv1.TradeDir_TRADE_DIR_BUY
		o.OfferAsset, o.OfferAmount = *quote, *quoteAmt
		o.WantAsset, o.WantAmount = *base, *baseAmt
	default:
		fatal("dir must be sell or buy")
	}

	if err := offer.SignOffer(o, k); err != nil {
		fatal("sign offer: %v", err)
	}

	var status seqobv1.OrderStatus
	if err := postJSON(*relay+"/v1/offers", o, &status); err != nil {
		fatal("submit: %v", err)
	}
	fmt.Printf("posted offer %s by maker %s (status %s, active %d)\n",
		status.GetOfferId(), status.GetMakerPubkey(), status.GetStatus(), status.GetActiveAmount())
}

// --- book ---

func cmdBook(args []string) {
	fs := newFlagSet("book")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	base := fs.String("base", "gold", "base asset id")
	quote := fs.String("quote", "usdx", "quote asset id")
	_ = fs.Parse(args)

	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *base, *quote), &book); err != nil {
		fatal("get book: %v", err)
	}
	fmt.Printf("order book %s/%s (%d offers)\n", *base, *quote, len(book.GetOffers()))
	for _, o := range book.GetOffers() {
		fmt.Printf("  %s  dir=%s base=%d  give %d %s  want %d %s  maker=%s\n",
			o.GetOfferId(), shortDir(o.GetTradeDir()), o.GetBaseAmount(),
			o.GetOfferAmount(), o.GetOfferAsset(), o.GetWantAmount(), o.GetWantAsset(),
			short(o.GetMakerPubkey()))
	}
}

// --- lift ---

func cmdLift(args []string) {
	fs := newFlagSet("lift")
	relay := fs.String("relay", "http://127.0.0.1:9955", "relay base URL")
	base := fs.String("base", "gold", "base asset id")
	quote := fs.String("quote", "usdx", "quote asset id")
	offerID := fs.String("offer-id", "", "offer id to lift")
	makerPub := fs.String("maker-pubkey", "", "maker pubkey of the offer")
	amount := fs.Uint64("amount", 0, "base atoms to take (<= active)")
	priv := fs.String("priv", "", "taker session secret key (32-byte hex); generated if empty")
	feeAsset := fs.String("fee-asset", "", "taker fee asset (any-asset fee market)")
	_ = fs.Parse(args)

	if *offerID == "" || *makerPub == "" {
		fatal("lift requires -offer-id and -maker-pubkey (see `seqob-cli book`)")
	}

	// Fetch the full offer so the taker can build the proposer SwapRequest.
	var book seqobv1.PublicBook
	if err := getJSON(fmt.Sprintf("%s/v1/market/%s/%s/orderbook", *relay, *base, *quote), &book); err != nil {
		fatal("get book: %v", err)
	}
	var target *seqobv1.Offer
	for _, o := range book.GetOffers() {
		if o.GetOfferId() == *offerID && o.GetMakerPubkey() == *makerPub {
			target = o
			break
		}
	}
	if target == nil {
		fatal("offer %s by %s not found in %s/%s", *offerID, short(*makerPub), *base, *quote)
	}
	take := *amount
	if take == 0 {
		take = target.GetBaseAmount()
	}

	takerKey := loadOrGenKey(*priv)

	// Open the lift session over WS so this connection is bound as the taker, then
	// courier the encrypted SwapRequest. The relay never sees the plaintext.
	wsURL := "ws" + strings.TrimPrefix(*relay, "http") + "/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer conn.Close()

	startLift := &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId:            *offerID,
		MakerPubkey:        *makerPub,
		TakeAmount:         take,
		TakerFeeAsset:      *feeAsset,
		TakerSessionPubkey: takerKey.PubKey().SerializeCompressed(),
	}}}
	writeWS(conn, startLift)

	la := readWS(conn)
	if la.GetLiftAccepted() == nil {
		fatal("expected lift_accepted, got %s", la.String())
	}
	sessionID := la.GetLiftAccepted().GetSessionId()
	fmt.Printf("lift session %s opened for offer %s\n", sessionID, *offerID)

	// Resolve the peer key for E2E: the maker's ephemeral session key if provided,
	// else fall back to the maker's static offer key (still relay-opaque).
	peerPub := la.GetLiftAccepted().GetMakerSessionPubkey()
	if len(peerPub) == 0 {
		pb, err := hex.DecodeString(*makerPub)
		if err != nil {
			fatal("bad maker pubkey: %v", err)
		}
		peerPub = pb
		fmt.Println("(no live maker session key; sealing to the maker's offer key)")
	}
	pk, err := btcec.ParsePubKey(peerPub)
	if err != nil {
		fatal("parse maker pubkey: %v", err)
	}
	crypter, err := client.NewCrypter(takerKey, pk)
	if err != nil {
		fatal("crypter: %v", err)
	}

	taker := &client.Taker{Wallet: &client.StubWallet{Name: "taker"}}
	sealed, req, err := taker.Propose(target, take, *feeAsset, crypter)
	if err != nil {
		fatal("build request: %v", err)
	}
	fmt.Printf("proposer legs: pay %d %s, receive %d %s (taker is proposer)\n",
		req.GetAmountP(), req.GetAssetP(), req.GetAmountR(), req.GetAssetR())

	writeWS(conn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sessionID, Ciphertext: sealed}}})
	fmt.Printf("couriered encrypted SwapRequest (%d bytes) to session %s\n", len(sealed), sessionID)
	fmt.Println("awaiting maker co-sign (run a maker process to complete settlement)")
}

// --- helpers ---

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

func loadOrGenKey(hexKey string) *btcec.PrivateKey {
	if hexKey == "" {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			fatal("gen key: %v", err)
		}
		fmt.Printf("generated key: priv=%s pub=%s\n",
			hex.EncodeToString(k.Serialize()), hex.EncodeToString(k.PubKey().SerializeCompressed()))
		return k
	}
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		fatal("priv must be 32-byte hex")
	}
	k, _ := btcec.PrivKeyFromBytes(b)
	return k
}

func postJSON(url string, in, out proto.Message) error {
	b, err := jsonMarshal.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return jsonUnmarshal.Unmarshal(body, out)
}

func getJSON(url string, out proto.Message) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return jsonUnmarshal.Unmarshal(body, out)
}

func writeWS(c *websocket.Conn, to *seqobv1.To) {
	b, err := jsonMarshal.Marshal(to)
	if err != nil {
		fatal("marshal To: %v", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		fatal("ws write: %v", err)
	}
}

func readWS(c *websocket.Conn) *seqobv1.From {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		fatal("ws read: %v", err)
	}
	var from seqobv1.From
	if err := jsonUnmarshal.Unmarshal(data, &from); err != nil {
		fatal("unmarshal From: %v", err)
	}
	return &from
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:6] + ".." + s[len(s)-6:]
}

func shortDir(d seqobv1.TradeDir) string {
	switch d {
	case seqobv1.TradeDir_TRADE_DIR_SELL:
		return "SELL"
	case seqobv1.TradeDir_TRADE_DIR_BUY:
		return "BUY"
	default:
		return "?"
	}
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
