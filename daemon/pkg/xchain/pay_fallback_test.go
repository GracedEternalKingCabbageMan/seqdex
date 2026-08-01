package xchain

import (
	"encoding/hex"
	"testing"
)

// A PAYEE WITH ONLY PRIVATE CHANNELS MUST STILL BE PAYABLE.
//
// CLN's `pay` plans from the gossip graph plus the invoice's routehints, so it cannot reach a
// payee whose only channels are unannounced -- it gives up with "not reachable directly and all
// routehints were unusable" even when that payee is our own direct peer. Every wallet-side node
// in this system is exactly that: JIT channels, never announced.
//
// That broke the entire pure-LN rail. The maker registered its hold correctly, then failed to pay
// the taker's asset invoice and cancelled the hold, so the taker's payment was refused with
// nothing said about the real cause. It reproduced four times running against the same taker node.
//
// Pay now falls back to PayHash, which already routes asset-aware and drops to a single hop over
// our own channel. payFallbackPlan is the decision that fallback rests on.
func TestPayFallbackPlan(t *testing.T) {
	const payee = "02f9ee2cc4eb03c20eb2e8f20967fd6b87438ce0ac5e8e3d52ce7144965e495775"
	const secretHex = "aa" + "bb" + "cc" + "dd"

	t.Run("a complete invoice yields a usable plan", func(t *testing.T) {
		gotPayee, secret, amt, ok := payFallbackPlan(payee, secretHex, 20000, 0)
		if !ok {
			t.Fatal("a payee with a payment_secret is payable by hash; refusing wastes the only route that works")
		}
		if gotPayee != payee {
			t.Fatalf("payee = %q, want %q", gotPayee, payee)
		}
		want, _ := hex.DecodeString(secretHex)
		if string(secret) != string(want) {
			t.Fatalf("secret = %x, want %x", secret, want)
		}
		if amt != 20000 {
			t.Fatalf("amount = %d, want the invoice's 20000", amt)
		}
	})

	t.Run("the quoted amount wins over the invoice amount", func(t *testing.T) {
		// Pay already verified the quote against the invoice before reaching the fallback, so the
		// quote is the authoritative figure; an amountless invoice reports 0 and must not win.
		if _, _, amt, _ := payFallbackPlan(payee, secretHex, 20000, 19999); amt != 19999 {
			t.Fatalf("amount = %d, want the quoted 19999", amt)
		}
		if _, _, amt, _ := payFallbackPlan(payee, secretHex, 20000, 0); amt != 20000 {
			t.Fatalf("amount = %d, want the invoice's 20000 for an amountless quote", amt)
		}
	})

	t.Run("REFUSE rather than send a malformed payment", func(t *testing.T) {
		// Without these there is no well-formed payment to make. Reporting the ORIGINAL pay error
		// is the point: a second, more confusing error from a request that could never have worked
		// would bury the real cause.
		cases := []struct {
			name                    string
			payee, secret           string
			invoiceMsat, quotedMsat uint64
		}{
			{"no payee", "", secretHex, 20000, 0},
			{"no payment_secret", payee, "", 20000, 0},
			{"unparsable secret", payee, "zzzz", 20000, 0},
			{"empty secret", payee, "", 20000, 0},
			{"no amount anywhere", payee, secretHex, 0, 0},
		}
		for _, c := range cases {
			if _, _, _, ok := payFallbackPlan(c.payee, c.secret, c.invoiceMsat, c.quotedMsat); ok {
				t.Errorf("%s: planned a fallback it cannot execute", c.name)
			}
		}
	})

	t.Run("a zero amount is never sent", func(t *testing.T) {
		// Sending 0 msat would be silently wrong -- it would "succeed" while delivering nothing.
		if _, _, amt, ok := payFallbackPlan(payee, secretHex, 0, 0); ok || amt != 0 {
			t.Fatalf("planned a 0-amount payment (ok=%v amt=%d)", ok, amt)
		}
	})
}
