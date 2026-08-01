package main

import "testing"

// A TAKER THAT WALKS AWAY MUST NOT PARK A MAKER ON "BUSY".
//
// This maker serves ONE lift at a time and holds that slot for the whole driver run. A taker that
// abandons a lift leaves the relay answering our next frame with 409 "the taker is not attached to
// session <id>". We used to print that and keep waiting out the driver's 2-minute TermsRequest
// deadline -- while the wallet gives up after 30s -- so every abandoned take parked a maker, and a
// handful of them wedged the entire fleet: every offer answering "busy, another lift is in flight".
//
// sessionFromNotAttached is what turns that error into the specific slot to release, so it must
// identify the session exactly and match NOTHING else -- it frees a live lift.
func TestSessionFromNotAttached(t *testing.T) {
	const sid = "986a5f5082e63876e038dda3ccda1d5b"

	t.Run("the taker-detached 409 yields its session", func(t *testing.T) {
		got := sessionFromNotAttached(409, "courier: the taker is not attached to session "+sid)
		if got != sid {
			t.Fatalf("got %q, want %q", got, sid)
		}
	})

	t.Run("either role, since the same relay path serves both", func(t *testing.T) {
		if got := sessionFromNotAttached(409, "courier: the maker is not attached to session "+sid); got != sid {
			t.Fatalf("got %q, want %q", got, sid)
		}
	})

	t.Run("trailing whitespace is not part of the id", func(t *testing.T) {
		if got := sessionFromNotAttached(409, "courier: the taker is not attached to session "+sid+"  \n"); got != sid {
			t.Fatalf("got %q, want %q", got, sid)
		}
	})

	t.Run("MATCHES NOTHING ELSE — it releases a live lift", func(t *testing.T) {
		cases := []struct {
			name string
			code uint32
			msg  string
		}{
			{"a different 409", 409, "courier: offer not found or not open"},
			{"the right text on the wrong code", 400, "courier: the taker is not attached to session " + sid},
			{"a 403", 403, "not a participant in this session"},
			{"empty", 409, ""},
			{"the phrase without an id", 409, "the taker is not attached to session "},
		}
		for _, c := range cases {
			got := sessionFromNotAttached(c.code, c.msg)
			if c.name == "the phrase without an id" {
				if got != "" {
					t.Errorf("%s: got %q, want empty (no id to release)", c.name, got)
				}
				continue
			}
			if got != "" {
				t.Errorf("%s: released session %q on an unrelated error", c.name, got)
			}
		}
	})
}
