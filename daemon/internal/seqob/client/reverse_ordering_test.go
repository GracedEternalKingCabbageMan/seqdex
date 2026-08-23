package client

import "testing"

// The maker's BTC refund must come after our asset refund under the conservative
// slow-SEQ / fast-BTC ratio, plus our claim runway: otherwise it can take its BTC
// back at T_btc and still claim our asset with the secret it holds.
func TestReverseMinBtcDelta(t *testing.T) {
	// The live fleet: T_seq delta 240 needs ceil(240*90/150)=144 + 6 = 150; the
	// fleet's 260 clears it, the old 100 does not.
	if got := reverseMinBtcDelta(240, 6); got != 150 {
		t.Fatalf("delta 240: got %d, want 150", got)
	}
	if got := reverseMinBtcDelta(1, 6); got != 7 {
		t.Fatalf("delta 1: got %d, want 7 (ceil)", got)
	}
	if got := reverseMinBtcDelta(480, 6); got != 294 {
		t.Fatalf("delta 480: got %d, want 294", got)
	}
}
