package xchain

// Tests for the VerifyBTCLeg propagation-lag poll: a taker funds its on-chain
// leg and announces within seconds, so the maker's node can be asked to verify
// a tx it has not SEEN yet (getrawtransaction -5 "No such mempool or blockchain
// transaction"). That not-found class must be POLLED out (bounded), while a tx
// that IS found but mismatches the quote must still fail immediately — the
// fund-safety check is never retried into passing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// --- poll-wrapper tests against a fake backend ------------------------------

// fakeNotSeenBackend scripts btcBackend.VerifyBTCLeg: the first `misses` calls
// answer the not-found class (misses < 0 => forever); after that it succeeds,
// unless `fail` is set, in which case EVERY call fails with that error (the
// found-but-mismatched case).
type fakeNotSeenBackend struct {
	misses int
	fail   error
	calls  int
}

func (f *fakeNotSeenBackend) HTLCScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	return nil, nil
}
func (f *fakeNotSeenBackend) LockBTCLeg(script []byte, amountCoins string, locktime uint32) (*LegLock, int64, error) {
	return nil, 0, nil
}
func (f *fakeNotSeenBackend) ClaimBTCLeg(leg *LegLock, claimKey *Key, fee uint64) (string, error) {
	return "", nil
}
func (f *fakeNotSeenBackend) RefundBTCLeg(leg *LegLock, refundKey *Key, nLockTime uint32, fee uint64) (string, error) {
	return "", nil
}
func (f *fakeNotSeenBackend) VerifyBTCLeg(
	hashH, makerClaimPub, takerRefundPub, providedScript []byte,
	btcLocktime uint32,
	txid string, vout uint32, amount uint64, assetID string,
	minConf int,
) (*VerifiedBTCLeg, error) {
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	if f.misses < 0 || f.calls <= f.misses {
		return nil, fmt.Errorf("%w: funding tx %s: getrawtransaction: rpc error -5: No such mempool or blockchain transaction",
			ErrBTCLegNotSeen, txid)
	}
	return &VerifiedBTCLeg{
		Leg:           &LegLock{Funded: &FundedHTLC{TxID: txid, Vout: vout, Amount: amount}, Locktime: btcLocktime},
		Confirmations: 1,
	}, nil
}

func notSeenSwap(b btcBackend, wait time.Duration) *Swap {
	s := &Swap{
		btcBackend: b,
		hash:       NewHashLockFromHash(bytes.Repeat([]byte{0xab}, 32)),
	}
	s.SetVerifyNotSeenWait(wait)
	return s
}

// A leg whose funding tx shows up after a few polls (normal 0-conf propagation
// lag) must verify, not instant-fail the session.
func TestVerifyBTCLegPollsOutNotSeen(t *testing.T) {
	fake := &fakeNotSeenBackend{misses: 2}
	s := notSeenSwap(fake, 10*time.Second)
	leg, err := s.VerifyBTCLeg(s.hash.Hash, nil, nil, nil, 500, "aa", 1, 250000, "", 1)
	if err != nil {
		t.Fatalf("VerifyBTCLeg after propagation lag: %v", err)
	}
	if leg == nil || leg.Leg.Funded.TxID != "aa" {
		t.Fatalf("wrong verified leg: %+v", leg)
	}
	if fake.calls != 3 {
		t.Fatalf("backend called %d times, want 3 (2 misses + 1 hit)", fake.calls)
	}
}

// A tx the node NEVER sees exhausts the bounded poll and fails terminally
// (ErrBTCLegInvalid, so driver loops stop) with the honest not-seen message.
func TestVerifyBTCLegNotSeenExhaustsHonestly(t *testing.T) {
	fake := &fakeNotSeenBackend{misses: -1}
	s := notSeenSwap(fake, 700*time.Millisecond)
	_, err := s.VerifyBTCLeg(s.hash.Hash, nil, nil, nil, 500, "bb", 1, 250000, "", 1)
	if err == nil {
		t.Fatal("VerifyBTCLeg succeeded on a never-seen tx")
	}
	if !errors.Is(err, ErrBTCLegInvalid) {
		t.Fatalf("exhausted poll must be terminal (ErrBTCLegInvalid), got: %v", err)
	}
	if !strings.Contains(err.Error(), "not seen on this node within") {
		t.Fatalf("exhausted poll must say the tx was not SEEN (not proven mismatched), got: %v", err)
	}
	if fake.calls < 2 {
		t.Fatalf("backend called %d times, want at least 2 (it must have retried)", fake.calls)
	}
}

// A tx that IS found but mismatches the quote is the fund-safety check: it must
// fail immediately, with no retries.
func TestVerifyBTCLegMismatchFailsImmediately(t *testing.T) {
	fake := &fakeNotSeenBackend{fail: fmt.Errorf("%w: btc-leg value %d != quoted %d", ErrBTCLegInvalid, 1, 2)}
	s := notSeenSwap(fake, 10*time.Second)
	start := time.Now()
	_, err := s.VerifyBTCLeg(s.hash.Hash, nil, nil, nil, 500, "cc", 1, 2, "", 1)
	if !errors.Is(err, ErrBTCLegInvalid) {
		t.Fatalf("want ErrBTCLegInvalid, got: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("mismatched leg retried: backend called %d times, want exactly 1", fake.calls)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("mismatched leg took %s to fail; must not sleep", el)
	}
}

// Unconfirmed (found, just not deep enough) is NOT the not-seen class: it flows
// straight back to the caller (the driver loops own that retry), no inner poll.
func TestVerifyBTCLegUnconfirmedPassesThrough(t *testing.T) {
	fake := &fakeNotSeenBackend{fail: fmt.Errorf("%w: btc-leg has 0 confirmations, need 1", ErrBTCLegUnconfirmed)}
	s := notSeenSwap(fake, 10*time.Second)
	_, err := s.VerifyBTCLeg(s.hash.Hash, nil, nil, nil, 500, "dd", 1, 250000, "", 1)
	if !errors.Is(err, ErrBTCLegUnconfirmed) {
		t.Fatalf("want ErrBTCLegUnconfirmed passed through, got: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("unconfirmed leg hit the inner poll: %d calls, want exactly 1", fake.calls)
	}
}

// --- not-found classifier ---------------------------------------------------

func TestIsTxNotFoundRPC(t *testing.T) {
	notFound := fmt.Errorf("getrawtransaction: %w",
		&rpcErr{Code: -5, Message: "No such mempool or blockchain transaction. Use gettransaction for wallet transactions."})
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rpc -5 wrapped", notFound, true},
		{"rpc -5 rewrapped", fmt.Errorf("fetch: %w", notFound), true},
		{"rpc -8 (other rpc error)", fmt.Errorf("getrawtransaction: %w", &rpcErr{Code: -8, Message: "parameter 2 must be a boolean"}), false},
		{"flattened canonical message", errors.New("rpc error -5: No such mempool or blockchain transaction"), true},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
	}
	for _, c := range cases {
		if got := isTxNotFoundRPC(c.err); got != c.want {
			t.Errorf("%s: isTxNotFoundRPC = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- backend classification against a scripted JSON-RPC node ----------------

// notSeenRPCServer serves a JSON-RPC node whose getrawtransaction answers -5
// for the first `misses` calls, then the given verbose result. getblockcount is
// always served (the bitcoin backend reads it after a successful verify), and
// decodescript answers a fixed p2sh (the elements backend's script-address
// derivation). Returns host RPC + a counter of getrawtransaction calls.
func notSeenRPCServer(t *testing.T, misses int, rawHex string, confirmations int) (*RPC, *int) {
	t.Helper()
	rawCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var (
			result interface{}
			errObj interface{}
		)
		switch req.Method {
		case "getrawtransaction":
			rawCalls++
			if rawCalls <= misses || misses < 0 {
				errObj = map[string]interface{}{"code": -5,
					"message": "No such mempool or blockchain transaction. Use gettransaction for wallet transactions."}
			} else {
				result = map[string]interface{}{"hex": rawHex, "confirmations": confirmations}
			}
		case "getblockcount":
			result = 100
		case "decodescript":
			result = map[string]interface{}{"p2sh": "2N6ZbXfg1RQ4nGyBv1K1nHCrhLbLbRcMkPn"}
		default:
			errObj = map[string]interface{}{"code": -32601, "message": "method not scripted: " + req.Method}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": result, "error": errObj})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	return NewRPC(host, port, "u", "p"), &rawCalls
}

// End-to-end through the REAL bitcoin backend: the node misses the funding tx
// twice (-5), then serves it; the Swap-level poll rides it out and the verify
// succeeds against the genuine HTLC fixture.
func TestBitcoinBackendVerifyRidesOutPropagation(t *testing.T) {
	script, _, txid, rawHex, amount := htlcFundingFixture(t)
	rpc, rawCalls := notSeenRPCServer(t, 2, rawHex, 1)
	chain := NewBitcoinChain(rpc, "w", &chaincfg.RegressionNetParams)

	prim := NewHashLockFromHash(bytes.Repeat([]byte{0xab}, 32)) // the fixture's H
	s := NewSwapBitcoin(chain, nil, prim)
	s.SetVerifyNotSeenWait(10 * time.Second)

	claim := KeyFromBytes(bytes.Repeat([]byte{0x11}, 32)).PubKey()  // fixture claim key
	refund := KeyFromBytes(bytes.Repeat([]byte{0x22}, 32)).PubKey() // fixture refund key
	leg, err := s.VerifyBTCLeg(prim.Hash, claim, refund, script, 500, txid, 1, amount, "", 1)
	if err != nil {
		t.Fatalf("VerifyBTCLeg after 2 node misses: %v", err)
	}
	if leg.Leg.Funded.TxID != txid || leg.Leg.Funded.Vout != 1 || leg.Leg.Funded.Amount != amount {
		t.Fatalf("wrong verified leg: %+v", leg.Leg.Funded)
	}
	if *rawCalls != 3 {
		t.Fatalf("getrawtransaction called %d times, want 3 (2 misses + 1 hit)", *rawCalls)
	}
}

// The bitcoin backend classifies the node's -5 as ErrBTCLegNotSeen (retryable),
// NOT ErrBTCLegInvalid (terminal) — the exact live instant-fail bug.
func TestBitcoinBackendClassifiesNotFound(t *testing.T) {
	script, _, txid, _, amount := htlcFundingFixture(t)
	rpc, _ := notSeenRPCServer(t, -1, "", 0)
	chain := NewBitcoinChain(rpc, "w", &chaincfg.RegressionNetParams)
	prim := NewHashLockFromHash(bytes.Repeat([]byte{0xab}, 32))
	b := newBitcoinBTCBackend(chain, prim)

	claim := KeyFromBytes(bytes.Repeat([]byte{0x11}, 32)).PubKey()
	refund := KeyFromBytes(bytes.Repeat([]byte{0x22}, 32)).PubKey()
	_, err := b.VerifyBTCLeg(prim.Hash, claim, refund, script, 500, txid, 1, amount, "", 1)
	if !errors.Is(err, ErrBTCLegNotSeen) {
		t.Fatalf("want ErrBTCLegNotSeen, got: %v", err)
	}
	if errors.Is(err, ErrBTCLegInvalid) {
		t.Fatalf("a not-yet-seen tx must NOT be classed invalid: %v", err)
	}
}

// Same classification through the elements backend (OutputAt's lookup).
func TestElementsBackendClassifiesNotFound(t *testing.T) {
	rpc, _ := notSeenRPCServer(t, -1, "", 0)
	chain := NewChain(rpc, "w")
	prim := NewHashLockFromHash(bytes.Repeat([]byte{0xab}, 32))
	b := newElementsBTCBackend(chain, prim)

	claim := KeyFromBytes(bytes.Repeat([]byte{0x11}, 32)).PubKey()
	refund := KeyFromBytes(bytes.Repeat([]byte{0x22}, 32)).PubKey()
	script, err := b.HTLCScript(claim, refund, 500)
	if err != nil {
		t.Fatalf("HTLCScript: %v", err)
	}
	_, err = b.VerifyBTCLeg(prim.Hash, claim, refund, script, 500, "ee", 0, 250000, "", 1)
	if !errors.Is(err, ErrBTCLegNotSeen) {
		t.Fatalf("want ErrBTCLegNotSeen, got: %v", err)
	}
	if errors.Is(err, ErrBTCLegInvalid) {
		t.Fatalf("a not-yet-seen tx must NOT be classed invalid: %v", err)
	}
}

// A tx the node DOES serve but whose amount mismatches the quote fails
// immediately through the full Swap path — one lookup, no retries.
func TestBitcoinBackendMismatchNoRetry(t *testing.T) {
	script, _, txid, rawHex, amount := htlcFundingFixture(t)
	rpc, rawCalls := notSeenRPCServer(t, 0, rawHex, 1)
	chain := NewBitcoinChain(rpc, "w", &chaincfg.RegressionNetParams)
	prim := NewHashLockFromHash(bytes.Repeat([]byte{0xab}, 32))
	s := NewSwapBitcoin(chain, nil, prim)
	s.SetVerifyNotSeenWait(10 * time.Second)

	claim := KeyFromBytes(bytes.Repeat([]byte{0x11}, 32)).PubKey()
	refund := KeyFromBytes(bytes.Repeat([]byte{0x22}, 32)).PubKey()
	_, err := s.VerifyBTCLeg(prim.Hash, claim, refund, script, 500, txid, 1, amount+1, "", 1)
	if !errors.Is(err, ErrBTCLegInvalid) {
		t.Fatalf("want ErrBTCLegInvalid on amount mismatch, got: %v", err)
	}
	if *rawCalls != 1 {
		t.Fatalf("mismatched leg retried: getrawtransaction called %d times, want exactly 1", *rawCalls)
	}
}
