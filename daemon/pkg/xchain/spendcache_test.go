package xchain

import (
	"encoding/json"
	"errors"
	"testing"
)

type countingBroadcaster struct {
	sent []string
	fail bool
}

func (b *countingBroadcaster) Broadcast(raw string) (string, error) {
	b.sent = append(b.sent, raw)
	if b.fail {
		return "", errors.New("timeout")
	}
	return "txid-" + raw, nil
}

// A retry after a lost broadcast reply must re-send the SAME transaction, never a
// second, conflicting spend of the outpoint.
func TestSpendCacheRebroadcastsTheSameSpend(t *testing.T) {
	var c spendCache
	b := &countingBroadcaster{fail: true}
	builds := 0
	build := func() (string, error) { builds++; return "raw1", nil }
	if _, err := c.once(b, "aa", 0, "claim", build); err == nil {
		t.Fatal("first broadcast should have failed")
	}
	b.fail = false
	txid, err := c.once(b, "aa", 0, "claim", func() (string, error) { builds++; return "raw2", nil })
	if err != nil || txid != "txid-raw1" {
		t.Fatalf("retry: txid=%q err=%v (want the first raw re-sent)", txid, err)
	}
	if builds != 1 || len(b.sent) != 2 || b.sent[1] != "raw1" {
		t.Fatalf("builds=%d sent=%v", builds, b.sent)
	}
	// A different kind on the same outpoint is its own spend.
	if _, err := c.once(b, "aa", 0, "refund", func() (string, error) { return "raw3", nil }); err != nil || b.sent[2] != "raw3" {
		t.Fatalf("refund kind: %v %v", err, b.sent)
	}
}

func TestCoinsToAtomsExact(t *testing.T) {
	cases := map[string]uint64{
		"0":                  0,
		"1":                  100_000_000,
		"0.00000001":         1,
		"123456789.12345678": 12345678912345678,
		"92233720368.54775807": 9223372036854775807, // > 2^53 atoms, exact
		"-1":                 0,
		"1e8":                0,
	}
	for in, want := range cases {
		if got := coinsToAtoms(json.Number(in)); got != want {
			t.Fatalf("%s: got %d want %d", in, got, want)
		}
	}
}

func TestInjectSecretRequires32Bytes(t *testing.T) {
	s := NewSwap(nil, nil, NewHashLock(make([]byte, 32)))
	if err := s.InjectSecret(make([]byte, 33)); !errors.Is(err, ErrBadPreimageLen) {
		t.Fatalf("33-byte secret: %v", err)
	}
}
