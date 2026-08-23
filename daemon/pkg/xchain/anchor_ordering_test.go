package xchain_test

// anchor_ordering_test.go unit-tests the REAL VerifySeqLegSafe predicate against
// a mock Sequentia node (no regtest harness), using the exact numbers from the
// live testnet4 runs of 2026-07-25.
//
// It exists mainly as a REGRESSION GUARD: it pins that the gate reads the anchor
// of the block that CONFIRMED THE LEG, and that a later, better-anchored block
// burying that leg does NOT rescue it. Reorg invalidation propagates to
// DESCENDANTS, never to ANCESTORS, so orphaning the burying block's anchor leaves
// the funding output confirmed and claimable while the counterparty's BTC leg is
// gone. Anyone tempted to make the gate consult a burying block or the chain tip
// (both of which advance, so the failing wait would "pass" in ~90 seconds) is
// deleting the protection, not fixing the latency.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// The failed live run: the taker's BTC HTLC confirmed at Bitcoin height 145609,
// and the maker's asset leg landed in a Sequentia block anchored at 145607
// (the committee's -anchoravoidcontested backdown). 90 seconds later the next
// Sequentia block carried anchor 145609 and buried the leg.
const (
	liveBTCLegHeight = int64(145609)
	liveLegBlock     = "dbfd4a076e3b40264f655fd78bc0dfec301781017942777b3b743ce45d1c06f3"
	liveLegAnchor    = int64(145607)
	liveBuryBlock    = "c15de1b5000000000000000000000000000000000000000000000000000000ff"
	liveBuryAnchor   = int64(145609)
)

type mockBlock struct {
	anchor    int64
	certified bool
	orphaned  bool // known header, no longer on the active chain (confirmations -1)
}

// mockSeqNode serves the three reads VerifySeqLegSafe makes: getblockheader
// (anchorheight + poscertified) and getanchorstatus.
type mockSeqNode struct {
	blocks    map[string]mockBlock
	tipAnchor int64
	tipStatus string
}

func (m *mockSeqNode) chain(t *testing.T) *xchain.Chain {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(v interface{}) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": v, "error": nil, "id": "t"})
		}
		switch req.Method {
		case "getblockheader":
			hash, _ := req.Params[0].(string)
			b, ok := m.blocks[hash]
			if !ok {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"result": nil, "error": map[string]interface{}{"code": -5, "message": "Block not found"}, "id": "t"})
				return
			}
			confs := 1
			if b.orphaned {
				confs = -1
			}
			reply(map[string]interface{}{
				"hash": hash, "anchorheight": b.anchor, "poscertified": b.certified,
				"poscountersigs": 15, "posquorum": 14, "confirmations": confs,
			})
		case "getanchorstatus":
			reply(map[string]interface{}{
				"validateanchor": true, "tipheight": 47958,
				"anchorheight": m.tipAnchor, "anchorstatus": m.tipStatus,
			})
		default:
			t.Errorf("unexpected RPC %q", req.Method)
			reply(nil)
		}
	}))
	t.Cleanup(srv.Close)
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return xchain.NewChain(xchain.NewRPC(host, port, "u", "p"), "")
}

func (m *mockSeqNode) swap(t *testing.T) *xchain.Swap {
	t.Helper()
	// Only the SEQ side is exercised; the BTC backend is never called.
	return xchain.NewSwap(nil, m.chain(t), xchain.NewHashLockFromHash(make([]byte, 32)))
}

// A header is served for an orphaned block too, and its anchorheight then commits
// nothing. The gate must refuse such a block (retryably: the caller re-derives the
// confirming block from the leg's txid) rather than read a number the chain no
// longer stands behind.
func TestVerifySeqLegSafeRefusesOrphanedBlock(t *testing.T) {
	node := &mockSeqNode{
		blocks:    map[string]mockBlock{liveBuryBlock: {anchor: liveBuryAnchor, certified: true, orphaned: true}},
		tipAnchor: liveBuryAnchor, tipStatus: "ok",
	}
	_, err := node.swap(t).VerifySeqLegSafe(liveBuryBlock, liveBTCLegHeight)
	if err == nil {
		t.Fatal("gate accepted an orphaned block whose anchor would otherwise satisfy the ordering")
	}
	if !errors.Is(err, xchain.ErrAnchorOrdering) || errors.Is(err, xchain.ErrAnchorOrderingTerminal) {
		t.Fatalf("want retryable ErrAnchorOrdering, got %v", err)
	}
}

// TestVerifySeqLegSafeRejectsUnderAnchoredLeg replays the FAILED run. The refusal
// was a TRUE POSITIVE: with the leg anchored at 145607 and the BTC leg at 145609,
// any Bitcoin reorg to a fork point P with 145607 <= P < 145609 orphans the BTC
// funding (the taker double-spends it) while leaving the asset leg's block
// canonical and its HTLC output claimable. The asset giver would lose both legs.
func TestVerifySeqLegSafeRejectsUnderAnchoredLeg(t *testing.T) {
	node := &mockSeqNode{
		blocks:    map[string]mockBlock{liveLegBlock: {anchor: liveLegAnchor, certified: true}},
		tipAnchor: liveLegAnchor, tipStatus: "ok",
	}
	ev, err := node.swap(t).VerifySeqLegSafe(liveLegBlock, liveBTCLegHeight)
	if err == nil {
		t.Fatalf("gate accepted a leg anchored at %d below the BTC-leg height %d", liveLegAnchor, liveBTCLegHeight)
	}
	if !errors.Is(err, xchain.ErrAnchorOrdering) {
		t.Fatalf("want ErrAnchorOrdering, got %v", err)
	}
	// The ordering conjunct is a committed header field, so it is TERMINAL: the
	// caller must refund now rather than re-read it for its whole timeout.
	if !errors.Is(err, xchain.ErrAnchorOrderingTerminal) {
		t.Fatalf("want ErrAnchorOrderingTerminal (the value can never rise), got %v", err)
	}
	if ev == nil || ev.OK {
		t.Fatalf("evidence must report NOT ok: %+v", ev)
	}
	// The node's own anchor view belongs in the message: without it the live log
	// could not distinguish "our node is behind" from "this block is behind".
	if !strings.Contains(err.Error(), "node anchorheight=") {
		t.Fatalf("error must report the node's anchor height for diagnosis: %v", err)
	}
}

// TestBuryingBlockDoesNotRescueUnderAnchoredLeg is the counterexample to the
// "consult a later/burying block" proposal, on the failed run's real chain.
//
// The leg sits in a block anchored 145607, buried by a block anchored 145609.
// A gate pointed at the burying block PASSES and the claim proceeds. Now Bitcoin
// reorgs to fork point 145608: the burying block's anchor is orphaned so it and
// its descendants are invalidated, but the LEG's block survives (145607 is still
// canonical), its funding output stays confirmed, and the claim simply re-mines
// against it — while the counterparty's BTC leg, confirmed at 145609, is orphaned
// and double-spent. Only the leg block's OWN anchor decides whether the funding
// output survives.
func TestBuryingBlockDoesNotRescueUnderAnchoredLeg(t *testing.T) {
	node := &mockSeqNode{
		blocks: map[string]mockBlock{
			liveLegBlock:  {anchor: liveLegAnchor, certified: true},
			liveBuryBlock: {anchor: liveBuryAnchor, certified: true},
		},
		// The chain has moved on: the live view (and the burying block) now clear
		// the BTC-leg height, which is exactly what made the relaxed gate look
		// attractive — it would have "passed" ~90 seconds after the failure.
		tipAnchor: liveBuryAnchor, tipStatus: "ok",
	}
	swap := node.swap(t)

	// A gate aimed at the burying block passes...
	if _, err := swap.VerifySeqLegSafe(liveBuryBlock, liveBTCLegHeight); err != nil {
		t.Fatalf("setup: the burying block should clear the gate: %v", err)
	}
	// ...but the LEG's own block must still be refused. If this ever passes, the
	// fund-loss case above is live.
	if _, err := swap.VerifySeqLegSafe(liveLegBlock, liveBTCLegHeight); err == nil {
		t.Fatalf("REGRESSION: the gate accepted an under-anchored leg because a LATER block anchors higher; " +
			"invalidation propagates to descendants, never to ancestors, so the leg's funding output survives a reorg that kills the BTC leg")
	}
}

// TestVerifySeqLegSafeRetryableWhenOnlyStatusFlaps: an unhealthy anchor status is
// the OPPOSITE case — it genuinely clears on its own (a parent-RPC hiccup), so it
// must NOT be reported as terminal or the caller would refund a recoverable swap.
func TestVerifySeqLegSafeRetryableWhenOnlyStatusFlaps(t *testing.T) {
	node := &mockSeqNode{
		blocks:    map[string]mockBlock{liveBuryBlock: {anchor: liveBuryAnchor, certified: true}},
		tipAnchor: liveBuryAnchor, tipStatus: "no_connection",
	}
	swap := node.swap(t)
	_, err := swap.VerifySeqLegSafe(liveBuryBlock, liveBTCLegHeight)
	if err == nil {
		t.Fatalf("gate must refuse while the node's anchor status is unhealthy")
	}
	if !errors.Is(err, xchain.ErrAnchorOrdering) {
		t.Fatalf("want ErrAnchorOrdering, got %v", err)
	}
	if errors.Is(err, xchain.ErrAnchorOrderingTerminal) {
		t.Fatalf("a flapping anchorstatus is TRANSIENT and must stay retryable, not terminal: %v", err)
	}

	// It clears once the parent connection is back — the ordering term was fine all along.
	node.tipStatus = "ok"
	if _, err := node.swap(t).VerifySeqLegSafe(liveBuryBlock, liveBTCLegHeight); err != nil {
		t.Fatalf("gate should pass once the anchor status recovers: %v", err)
	}
}

// TestVerifySeqLegSafeRequiresQuorumCertification pins the third conjunct: a leg
// block that is anchored high enough but NOT quorum-certified is still refused,
// and that refusal is retryable (countersignatures accrue).
func TestVerifySeqLegSafeRequiresQuorumCertification(t *testing.T) {
	node := &mockSeqNode{
		blocks:    map[string]mockBlock{liveBuryBlock: {anchor: liveBuryAnchor, certified: false}},
		tipAnchor: liveBuryAnchor, tipStatus: "ok",
	}
	_, err := node.swap(t).VerifySeqLegSafe(liveBuryBlock, liveBTCLegHeight)
	if err == nil {
		t.Fatalf("gate must refuse a leg block that is not quorum-certified")
	}
	if errors.Is(err, xchain.ErrAnchorOrderingTerminal) {
		t.Fatalf("certification accrues over time, so this must stay retryable: %v", err)
	}
}
