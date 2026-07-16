package registry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const sampleIndex = `{
  "aaaa": ["issuer.example", "GOLD", "Gold", 8, 1],
  "bbbb": ["issuer.example", "ZERO", "Zero decimals", 0, 0]
}`

func TestPrecisionFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sampleIndex))
		}),
	)
	defer srv.Close()

	c := NewClient(srv.URL)

	if p, ok := c.PrecisionFor("aaaa"); !ok || p != 8 {
		t.Fatalf("aaaa: got (%d,%v), want (8,true)", p, ok)
	}
	// A genuine 0-decimal asset must resolve to 0 (not be treated as missing).
	if p, ok := c.PrecisionFor("bbbb"); !ok || p != 0 {
		t.Fatalf("bbbb: got (%d,%v), want (0,true)", p, ok)
	}
	// Unknown asset -> caller falls back.
	if p, ok := c.PrecisionFor("cccc"); ok || p != 0 {
		t.Fatalf("cccc: got (%d,%v), want (0,false)", p, ok)
	}
}

func TestNilClientAndUnreachableAreSoft(t *testing.T) {
	// Empty url -> nil client, safe to call.
	var nilClient *Client = NewClient("")
	if nilClient != nil {
		t.Fatal("empty url must yield a nil client")
	}
	if p, ok := nilClient.PrecisionFor("aaaa"); ok || p != 0 {
		t.Fatalf("nil client: got (%d,%v), want (0,false)", p, ok)
	}

	// Unreachable registry must never error, only report not-found.
	dead := NewClient("http://127.0.0.1:1/index.minimal.json")
	if p, ok := dead.PrecisionFor("aaaa"); ok || p != 0 {
		t.Fatalf("unreachable: got (%d,%v), want (0,false)", p, ok)
	}
}
