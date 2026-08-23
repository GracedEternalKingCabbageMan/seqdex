package wallet

import "testing"

// Ocean reports an explicit coin's blinders empty or all zero; anything else is
// a real blinding factor and the coin is confidential.
func TestIsConfidentialBlinder(t *testing.T) {
	zero := "0000000000000000000000000000000000000000000000000000000000000000"
	real := "0a1b000000000000000000000000000000000000000000000000000000000001"
	for _, c := range []struct {
		in   string
		want bool
	}{{"", false}, {zero, false}, {real, true}, {"00000001", true}} {
		if got := isConfidentialBlinder(c.in); got != c.want {
			t.Fatalf("isConfidentialBlinder(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
