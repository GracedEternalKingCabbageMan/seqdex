package main

import (
	"encoding/hex"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/offer"
)

func pubHex(k *btcec.PrivateKey) string {
	return hex.EncodeToString(k.PubKey().SerializeCompressed())
}

// refreshOfferForRequote rolls the offer's window forward and re-signs it while
// leaving identity/terms intact, so the same offer_id can be re-posted for a fresh
// quote after a fill. This is the load-bearing "re-derive/re-sign" step.
func TestRefreshOfferForRequote(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	o := buildCrossOffer("gold", "sell", 100, 45, "", time.Hour, 0, "seqaddr", pubHex(key), "stable-offer-id")
	if err := offer.SignOffer(o, key); err != nil {
		t.Fatal(err)
	}
	origID := o.GetOfferId()
	origBase := o.GetBaseAmount()
	origOfferAsset := o.GetOfferAsset()
	origSig := append([]byte(nil), o.GetMakerSig()...)

	// Simulate an offer near expiry (as it would be after resting through a swap).
	o.CreatedAtUnix = uint64(time.Now().Add(-50 * time.Minute).Unix())
	o.ExpiresAtUnix = uint64(time.Now().Add(1 * time.Minute).Unix())

	if err := refreshOfferForRequote(o, 2*time.Hour, key); err != nil {
		t.Fatalf("refreshOfferForRequote: %v", err)
	}

	// Identity + terms unchanged (no double-quote of a different offer).
	if o.GetOfferId() != origID {
		t.Errorf("offer id changed: %s -> %s", origID, o.GetOfferId())
	}
	if o.GetBaseAmount() != origBase {
		t.Errorf("base amount changed: %d -> %d", origBase, o.GetBaseAmount())
	}
	if o.GetOfferAsset() != origOfferAsset {
		t.Errorf("offer asset changed: %s -> %s", origOfferAsset, o.GetOfferAsset())
	}
	// Expiry pushed comfortably into the future.
	if o.GetExpiresAtUnix() <= uint64(time.Now().Add(time.Hour).Unix()) {
		t.Errorf("expiry not refreshed forward: %d", o.GetExpiresAtUnix())
	}
	// Re-signed (a new signature over the new window) and still valid.
	if hex.EncodeToString(o.GetMakerSig()) == hex.EncodeToString(origSig) {
		t.Errorf("signature not refreshed after rolling the window")
	}
	if err := offer.VerifyOffer(o); err != nil {
		t.Errorf("refreshed offer fails verification: %v", err)
	}
}

// postCancel signs a cancel and POSTs it, returning nil only on HTTP 200. The
// requote path relies on it being synchronous so the follow-up submit cannot race
// the old resting copy.
func TestPostCancel(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	o := buildCrossOffer("gold", "sell", 100, 45, "", time.Hour, 0, "seqaddr", pubHex(key), "oid")
	if err := offer.SignOffer(o, key); err != nil {
		t.Fatal(err)
	}

	var got seqobv1.OfferCancel
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		if err := jsonUnmarshal.Unmarshal(body, &got); err != nil {
			t.Errorf("relay could not parse cancel: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	if err := postCancel(ok.URL, o, key); err != nil {
		t.Fatalf("postCancel on 200: %v", err)
	}
	if got.GetOfferId() != o.GetOfferId() {
		t.Errorf("cancel offer id = %q, want %q", got.GetOfferId(), o.GetOfferId())
	}
	if err := offer.VerifyCancel(&got); err != nil {
		t.Errorf("posted cancel has an invalid signature: %v", err)
	}

	// Two cancels of the same offer id carry strictly increasing nonces so the
	// store's replay guard admits both (a re-posted same offer_id can be
	// re-cancelled on its next fill).
	var got2 seqobv1.OfferCancel
	ok2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		_ = jsonUnmarshal.Unmarshal(body, &got2)
		w.WriteHeader(http.StatusOK)
	}))
	defer ok2.Close()
	if err := postCancel(ok2.URL, o, key); err != nil {
		t.Fatalf("postCancel second: %v", err)
	}
	if got2.GetNonce() <= got.GetNonce() {
		t.Errorf("second cancel nonce %d not > first %d", got2.GetNonce(), got.GetNonce())
	}

	// A non-200 relay response is surfaced as an error (requote logs and presses on).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("offer not found"))
	}))
	defer bad.Close()
	if err := postCancel(bad.URL, o, key); err == nil {
		t.Errorf("want an error on HTTP 409, got nil")
	}
}
