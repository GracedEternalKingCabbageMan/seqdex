package xchain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fakeRatesNode answers just enough JSON-RPC for FeeExchangeRate: it returns the
// given getfeeexchangerates map and an empty dumpassetlabels. Any other method
// returns a null result (harmless for these tests).
func fakeRatesNode(t *testing.T, rates map[string]int64) *Chain {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result interface{}
		switch req.Method {
		case "getfeeexchangerates":
			result = rates
		case "dumpassetlabels":
			result = map[string]string{}
		default:
			result = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": result, "error": nil})
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	return NewChain(NewRPC(host, port, "u", "p"), "")
}

// TestSizeSeqSpendFee proves the submarine claim-fee sizing: a VALUABLE asset pays
// FEWER atoms (so the native-equivalent value stays inside maxfeerate), the native
// asset is an identity, and the fee is clamped to half the leg. This is the fix for
// the GOLD claim being rejected by sendrawtransaction (maxfeerate).
func TestSizeSeqSpendFee(t *testing.T) {
	const goldHex = "3a0f9192aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const nativeHex = "c8eccacfbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// GOLD rate ~5e12 (asset value in native-sats x 1e8). A 1000-native-sat target
	// converts to ceil(1000*1e8 / 5e12) = 1 atom — a raw 1000 GOLD atoms would be
	// worth ~5e7 native sats and trip maxfeerate.
	gold := fakeRatesNode(t, map[string]int64{goldHex: 5_000_000_000_000})
	s := &Swap{seq: gold}
	if got := s.sizeSeqSpendFee(goldHex, 1_000_000, 1000); got != 1 {
		t.Fatalf("GOLD claim fee: got %d atoms, want 1", got)
	}

	// The native/policy asset publishes rate 1e8 -> the sizing is an identity.
	native := fakeRatesNode(t, map[string]int64{nativeHex: 100_000_000})
	s = &Swap{seq: native}
	if got := s.sizeSeqSpendFee(nativeHex, 1_000_000, 1000); got != 1000 {
		t.Fatalf("native claim fee: got %d atoms, want 1000", got)
	}

	// No rate published for the asset -> fall back to the flat native target.
	if got := s.sizeSeqSpendFee(goldHex, 1_000_000, 1000); got != 1000 {
		t.Fatalf("unpriced asset fee: got %d atoms, want 1000 (flat fallback)", got)
	}

	// Clamp to half the leg so a tiny leg can never go negative on the fee output.
	if got := s.sizeSeqSpendFee(nativeHex, 100, 1000); got != 50 {
		t.Fatalf("small-leg clamp: got %d atoms, want 50 (half of 100)", got)
	}

	// A nil SEQ chain (unit-test / no node) must not panic: flat target, clamped.
	bare := &Swap{}
	if got := bare.sizeSeqSpendFee(goldHex, 1_000_000, 1000); got != 1000 {
		t.Fatalf("nil-chain fee: got %d atoms, want 1000", got)
	}
}
