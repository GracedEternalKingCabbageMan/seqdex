package xchain

import "testing"

// Cross-chain timelock arithmetic takes the chain that must be slow as slow and the
// one that must be fast as fast, so the numbers err on the side of the payer.
func TestCltvCoverAndCap(t *testing.T) {
	// A BTC invoice with 18 final + 24 route blocks (42 * 900 s slow) plus a 20-minute
	// margin needs (37800+1200)/60 = 650 Sequentia blocks of hold.
	if got := CoverDelay(42, BTCTiming, SeqTiming, 1200); got != 650 {
		t.Fatalf("cover: got %d, want 650", got)
	}
	// 650 Sequentia blocks left (fast, 60 s) less the 20-minute margin allow
	// (39000-1200)/900 = 42 BTC blocks of route.
	if got := CapDelay(650, SeqTiming, BTCTiming, 1200); got != 42 {
		t.Fatalf("cap: got %d, want 42", got)
	}
	// An 18-block Sequentia hold (18*60 fast) cannot cover any BTC route once the
	// margin is taken: 1080 s < 1200 s.
	if got := CapDelay(18, SeqTiming, BTCTiming, 1200); got != 0 {
		t.Fatalf("cap of a short hold: got %d, want 0", got)
	}
	// Round trip: covering a delay and capping against the cover never yields less.
	for _, d := range []uint32{1, 18, 42, 144} {
		c := CoverDelay(d, BTCTiming, SeqTiming, 600)
		if back := CapDelay(c, SeqTiming, BTCTiming, 600); back < d {
			t.Fatalf("delay %d: cover %d caps back to %d", d, c, back)
		}
	}
	// The sub-asset SELL taker's 18-block asset hold needs T_btc at least
	// (18*90+1800)/150 = 23 BTC blocks out, not the old 6.
	if got := CoverDelay(18, SeqTiming, BTCTiming, 1800); got != 23 {
		t.Fatalf("sell-taker window: got %d, want 23", got)
	}
}
