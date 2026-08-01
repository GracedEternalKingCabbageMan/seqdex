package xchain

import (
	"errors"
	"testing"
)

// "FIRST PEER NOT READY" IS A DROPPED TRANSPORT, NOT A DEAD ROUTE.
//
// CLN keeps the channel but tears down a peer's TCP/Noise connection when it goes idle (a peer
// restart drops it too). The channel is fine and the balance is fine -- the first hop simply has no
// live connection, and sendpay refuses with WIRE_TEMPORARY_CHANNEL_FAILURE.
//
// ReconnectPeers already existed for exactly this and was called from nowhere, so a wallet whose node
// had been idle failed its swap outright: "pure-LN swap ended: pay hold: sendpay: rpc error 204:
// failed: WIRE_TEMPORARY_CHANNEL_FAILURE (First peer not ready)". PayHash now reconnects and retries
// once on this error and only this one -- retrying a payment for the wrong reason is precisely the
// kind of thing that must not be loose.
func TestIsPeerNotReady(t *testing.T) {
	t.Run("the wire error that actually happened", func(t *testing.T) {
		err := errors.New("sendpay: rpc error 204: failed: WIRE_TEMPORARY_CHANNEL_FAILURE (First peer not ready)")
		if !isPeerNotReady(err) {
			t.Fatal("this is the reconnect case; failing it strands a perfectly good channel")
		}
	})

	t.Run("case does not matter", func(t *testing.T) {
		if !isPeerNotReady(errors.New("First Peer Not Ready")) {
			t.Fatal("CLN's capitalisation is not a contract")
		}
	})

	t.Run("a temporary channel failure ABOUT A PEER also counts", func(t *testing.T) {
		if !isPeerNotReady(errors.New("wire_temporary_channel_failure: peer is offline")) {
			t.Fatal("same condition, different wording")
		}
	})

	t.Run("REAL routing and liquidity failures must NOT retry", func(t *testing.T) {
		for _, m := range []string{
			"WIRE_TEMPORARY_NODE_FAILURE (reply from remote)",
			"WIRE_INCORRECT_OR_UNKNOWN_PAYMENT_DETAILS",
			"no route found",
			"insufficient funds",
			"WIRE_TEMPORARY_CHANNEL_FAILURE", // no peer mention: could be capacity downstream
		} {
			if isPeerNotReady(errors.New(m)) {
				t.Errorf("would retry a payment on %q, which is not a transport problem", m)
			}
		}
	})

	t.Run("nil is not an error to recover from", func(t *testing.T) {
		if isPeerNotReady(nil) {
			t.Fatal("nil must never trigger a reconnect-and-resend")
		}
	})
}
