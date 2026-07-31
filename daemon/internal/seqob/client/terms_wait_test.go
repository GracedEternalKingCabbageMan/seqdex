package client

import (
	"testing"
	"time"
)

// A MAKER MUST NOT HOLD ITS ONLY LIFT SLOT LONGER THAN THE TAKER WILL WAIT.
//
// XcTermsRequest is the taker's very first frame after its lift is accepted: it arrives in
// milliseconds or it is never coming. A cross maker serves ONE lift at a time, so every second spent
// waiting for it is a second every OTHER taker is refused with "busy".
//
// At the old two-minute default that was catastrophic against a wallet whose own patience for terms
// is 30 SECONDS: each abandoned take parked a maker for four times as long as the taker was willing
// to wait, so a handful of retries wedged the whole fleet and every price level answered "busy,
// another lift is in flight". Nothing was broken — everything was occupied by takers who had left.
func TestTermsReqWaitFreesTheSlotBeforeTheTakerGivesUp(t *testing.T) {
	// The wallet's pre-lock patience for terms (xswap.js PRELOCK_TERMS_MS).
	const takerPatience = 30 * time.Second

	var timing XcTiming
	timing.setDefaults()

	if timing.TermsReqWait <= 0 {
		t.Fatal("TermsReqWait must have a default; 0 would block forever on a taker that never speaks")
	}
	if timing.TermsReqWait >= takerPatience {
		t.Fatalf("TermsReqWait %v >= the taker's %v patience: an abandoned lift parks this maker on"+
			" \"busy\" for longer than the taker was ever willing to wait, which is what wedged the fleet",
			timing.TermsReqWait, takerPatience)
	}
	// And not so short that a momentarily slow-but-live taker is dropped mid-handshake.
	if timing.TermsReqWait < 10*time.Second {
		t.Fatalf("TermsReqWait %v is too tight for a live taker on a slow link", timing.TermsReqWait)
	}
}

func TestTermsReqWaitExplicitValueIsRespected(t *testing.T) {
	// Tests and operators override this; the default must not stomp an explicit choice.
	timing := XcTiming{TermsReqWait: 2 * time.Second}
	timing.setDefaults()
	if timing.TermsReqWait != 2*time.Second {
		t.Fatalf("explicit TermsReqWait was overwritten: got %v", timing.TermsReqWait)
	}
}
