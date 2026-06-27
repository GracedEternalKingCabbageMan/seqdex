package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/client"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offerstore"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/session"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/validator"
)

func newServer(t *testing.T) (*httptest.Server, *offerstore.Store) {
	t.Helper()
	store := offerstore.New(nil)
	v := validator.New(validator.DefaultConfig(), nil)
	router := session.NewRouter(session.Options{Deadline: time.Minute})
	srv := New(store, v, router, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func mkSignedOffer(t *testing.T, k *btcec.PrivateKey, id string) *seqobv1.Offer {
	t.Helper()
	o := &seqobv1.Offer{
		OfferId:       id,
		SchemaVersion: 1,
		Pair:          &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:      seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:    100,
		OfferAmount:   100,
		OfferAsset:    "gold",
		WantAmount:    45,
		WantAsset:     "usdx",
		AllowPartial:  true,
		CreatedAtUnix: uint64(time.Now().Unix()),
		ExpiresAtUnix: uint64(time.Now().Add(time.Hour).Unix()),
		Settlement:    &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr"}},
	}
	if err := offer.SignOffer(o, k); err != nil {
		t.Fatal(err)
	}
	return o
}

func postProto(t *testing.T, url string, m proto.Message) *http.Response {
	t.Helper()
	b, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func key(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	k, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRESTSubmitMarketsOrderbook(t *testing.T) {
	ts, _ := newServer(t)
	k := key(t)
	o := mkSignedOffer(t, k, "aaaa")

	resp := postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Markets lists the pair.
	var ml seqobv1.MarketList
	getProto(t, ts.URL+"/v1/markets", &ml)
	if len(ml.GetMarkets()) != 1 || ml.GetMarkets()[0].GetPair().GetBaseAsset() != "gold" {
		t.Fatalf("markets = %+v", ml.GetMarkets())
	}

	// Orderbook shows the offer.
	var book seqobv1.PublicBook
	getProto(t, ts.URL+"/v1/market/gold/usdx/orderbook", &book)
	if len(book.GetOffers()) != 1 || book.GetOffers()[0].GetOfferId() != "aaaa" {
		t.Fatalf("orderbook = %+v", book.GetOffers())
	}

	// Own orders.
	var own seqobv1.PublicBook
	getProto(t, ts.URL+"/v1/offers?maker_pubkey="+o.GetMakerPubkey(), &own)
	if len(own.GetOffers()) != 1 {
		t.Fatalf("own offers = %+v", own.GetOffers())
	}
}

func TestRESTRejectsBadSignature(t *testing.T) {
	ts, _ := newServer(t)
	k := key(t)
	o := mkSignedOffer(t, k, "aaaa")
	o.WantAmount = 1 // break the signature
	resp := postProto(t, ts.URL+"/v1/offers", o)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad signature, got %d", resp.StatusCode)
	}
}

// TestRESTReplayIsNoOp is the ITEM A integration regression: replaying a genuine,
// byte-identical offer must be a no-op (HTTP 200, not a 409 conflict) and must not
// duplicate the book entry; proving api.New wired the book into the validator so
// a replay never reaches store.Submit (and never charged the maker's budget).
func TestRESTReplayIsNoOp(t *testing.T) {
	ts, store := newServer(t)
	k := key(t)
	o := mkSignedOffer(t, k, "aaaa")

	resp := postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first submit = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postProto(t, ts.URL+"/v1/offers", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay submit = %d, want 200 (no-op, not a conflict)", resp.StatusCode)
	}
	resp.Body.Close()

	if got := len(store.SnapshotMaker(o.GetMakerPubkey())); got != 1 {
		t.Fatalf("book holds %d offers after replay, want exactly 1", got)
	}
}

func TestWSSnapshotAndDelta(t *testing.T) {
	ts, _ := newServer(t)
	k := key(t)

	// Pre-existing offer should appear in the WS snapshot.
	resp := postProto(t, ts.URL+"/v1/offers", mkSignedOffer(t, k, "aaaa"))
	resp.Body.Close()

	c := dialWS(t, ts)
	defer c.Close()
	sendTo(t, c, &seqobv1.To{Msg: &seqobv1.To_MarketSubscribe{MarketSubscribe: &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"}}})

	snap := readFrom(t, c)
	if snap.GetPublicBook() == nil || len(snap.GetPublicBook().GetOffers()) != 1 {
		t.Fatalf("expected snapshot with 1 offer, got %+v", snap)
	}

	// New offer arrives as a created delta.
	resp = postProto(t, ts.URL+"/v1/offers", mkSignedOffer(t, k, "bbbb"))
	resp.Body.Close()
	delta := readFrom(t, c)
	if delta.GetPublicOrderCreated() == nil || delta.GetPublicOrderCreated().GetOfferId() != "bbbb" {
		t.Fatalf("expected created delta for bbbb, got %+v", delta)
	}
}

func TestWSOpaqueCourierWithLiftNotification(t *testing.T) {
	ts, _ := newServer(t)
	mk := key(t) // maker's offer key doubles as its E2E session key
	o := mkSignedOffer(t, mk, "aaaa")

	// Maker connects via WS and submits the offer (registers as reachable).
	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if st := readFrom(t, makerConn); st.GetOrderStatus() == nil {
		t.Fatalf("expected order_status after maker submit, got %+v", st)
	}

	// Taker connects and lifts the offer with its ephemeral session pubkey.
	tk := key(t)
	takerConn := dialWS(t, ts)
	defer takerConn.Close()
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: tk.PubKey().SerializeCompressed(),
	}}})

	// Maker is notified via lift_requested carrying the TAKER's session pubkey.
	mn := readFrom(t, makerConn)
	lr := mn.GetLiftRequested()
	if lr == nil {
		t.Fatalf("expected lift_requested at maker, got %+v", mn)
	}
	if !bytes.Equal(lr.GetTakerSessionPubkey(), tk.PubKey().SerializeCompressed()) {
		t.Fatalf("lift_requested did not carry the taker session pubkey")
	}
	sessionID := lr.GetSessionId()

	// Taker gets lift_accepted carrying the maker's (offer) session pubkey.
	la := readFrom(t, takerConn)
	if la.GetLiftAccepted() == nil || la.GetLiftAccepted().GetSessionId() != sessionID {
		t.Fatalf("expected lift_accepted, got %+v", la)
	}
	if !bytes.Equal(la.GetLiftAccepted().GetMakerSessionPubkey(), mk.PubKey().SerializeCompressed()) {
		t.Fatalf("lift_accepted did not carry the maker offer pubkey as session key")
	}

	// Real E2E: taker seals to the maker's offer key; the relay couriers opaque
	// bytes; the maker opens with its offer key + the taker's session pubkey.
	makerPub, _ := btcec.ParsePubKey(la.GetLiftAccepted().GetMakerSessionPubkey())
	takerCrypter, _ := client.NewCrypter(tk, makerPub)
	takerPub, _ := btcec.ParsePubKey(lr.GetTakerSessionPubkey())
	makerCrypter, _ := client.NewCrypter(mk, takerPub)

	plaintext := []byte("confidential SwapRequest plaintext")
	sealed, err := takerCrypter.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sessionID, Ciphertext: sealed}}})

	relayed := readFrom(t, makerConn)
	if relayed.GetSwapMsg() == nil || relayed.GetSwapMsg().GetSessionId() != sessionID {
		t.Fatalf("maker did not receive the courier frame, got %+v", relayed)
	}
	// Relay carried only ciphertext (it never saw the plaintext).
	if bytes.Equal(relayed.GetSwapMsg().GetCiphertext(), plaintext) {
		t.Fatalf("relay must courier ciphertext, not plaintext")
	}
	opened, err := makerCrypter.Open(relayed.GetSwapMsg().GetCiphertext())
	if err != nil {
		t.Fatalf("maker could not open the couriered payload: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("E2E round-trip mismatch: %q", opened)
	}
}

// --- helpers ---

func dialWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return c
}

func sendTo(t *testing.T, c *websocket.Conn, to *seqobv1.To) {
	t.Helper()
	b, err := protojson.Marshal(to)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatal(err)
	}
}

func readFrom(t *testing.T, c *websocket.Conn) *seqobv1.From {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read From: %v", err)
	}
	var from seqobv1.From
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &from); err != nil {
		t.Fatalf("unmarshal From: %v", err)
	}
	return &from
}

func getProto(t *testing.T, url string, m proto.Message) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d", url, resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(buf.Bytes(), m); err != nil {
		t.Fatalf("unmarshal %s: %v", url, err)
	}
}
