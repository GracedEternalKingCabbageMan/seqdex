package client

import (
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// TestProRataNoOverflow guards the uint64 overflow that silently garbled every
// non-trivial fill: offer_amount*takeBase overflows uint64 for realistic atom
// sizes (5e10 * 5e10 = 2.5e21 > 1.8e19). A full take of a 500-SEQ-for-50-USDX
// offer must yield exactly recv=500 SEQ, pay=50 USDX.
func TestProRataNoOverflow(t *testing.T) {
	o := &seqobv1.Offer{
		BaseAmount:  50000000000, // 500 SEQ
		OfferAmount: 50000000000, // gives 500 SEQ
		WantAmount:  5000000000,  // wants 50 USDX
	}
	recv, pay, err := proRata(o, 50000000000) // full take
	if err != nil {
		t.Fatalf("proRata: %v", err)
	}
	if recv != 50000000000 {
		t.Errorf("recv = %d, want 50000000000 (overflow regression: got the wrapped 193791000?)", recv)
	}
	if pay != 5000000000 {
		t.Errorf("pay = %d, want 5000000000 (overflow regression: got the wrapped 203846541?)", pay)
	}
	// Half take -> exact half.
	recv, pay, err = proRata(o, 25000000000)
	if err != nil {
		t.Fatalf("proRata half: %v", err)
	}
	if recv != 25000000000 || pay != 2500000000 {
		t.Errorf("half take: recv=%d pay=%d, want 25000000000/2500000000", recv, pay)
	}
}
