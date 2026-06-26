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

func TestWSOpaqueCourier(t *testing.T) {
	ts, _ := newServer(t)
	mk := key(t)
	o := mkSignedOffer(t, mk, "aaaa")

	// Maker connects via WS and submits the offer (registers as reachable).
	makerConn := dialWS(t, ts)
	defer makerConn.Close()
	sendTo(t, makerConn, &seqobv1.To{Msg: &seqobv1.To_OfferSubmit{OfferSubmit: o}})
	if st := readFrom(t, makerConn); st.GetOrderStatus() == nil {
		t.Fatalf("expected order_status after maker submit, got %+v", st)
	}

	// Taker connects and lifts the offer.
	takerConn := dialWS(t, ts)
	defer takerConn.Close()
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_StartLift{StartLift: &seqobv1.StartLift{
		OfferId: "aaaa", MakerPubkey: o.GetMakerPubkey(), TakeAmount: 50,
		TakerSessionPubkey: []byte{9, 9, 9},
	}}})
	la := readFrom(t, takerConn)
	if la.GetLiftAccepted() == nil {
		t.Fatalf("expected lift_accepted, got %+v", la)
	}
	sessionID := la.GetLiftAccepted().GetSessionId()

	// Maker should be notified of the new session.
	mn := readFrom(t, makerConn)
	if mn.GetLiftAccepted() == nil || mn.GetLiftAccepted().GetSessionId() != sessionID {
		t.Fatalf("expected maker lift notification, got %+v", mn)
	}

	// Taker couriers an opaque ciphertext; maker must receive it verbatim.
	cipher := []byte("opaque-sealed-swap-request")
	sendTo(t, takerConn, &seqobv1.To{Msg: &seqobv1.To_SwapMsg{SwapMsg: &seqobv1.SwapMsg{SessionId: sessionID, Ciphertext: cipher}}})
	relayed := readFrom(t, makerConn)
	if relayed.GetSwapMsg() == nil || !bytes.Equal(relayed.GetSwapMsg().GetCiphertext(), cipher) {
		t.Fatalf("maker did not receive the opaque ciphertext, got %+v", relayed)
	}
	if relayed.GetSwapMsg().GetSessionId() != sessionID {
		t.Fatalf("courier session id mismatch")
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
