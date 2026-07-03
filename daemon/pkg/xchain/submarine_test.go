package xchain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// fakeLNLeg is a stand-in LNLeg for tests that must not touch a real node. Any
// method that a test reaches unexpectedly fails loudly, so a test only exercises
// the code paths it means to.
type fakeLNLeg struct{ t *testing.T }

func (f *fakeLNLeg) NodeID() (string, error) { f.t.Fatal("NodeID unexpected"); return "", nil }
func (f *fakeLNLeg) Pay(string, []byte, uint64) ([]byte, error) {
	f.t.Fatal("Pay unexpected")
	return nil, nil
}
func (f *fakeLNLeg) CreateHoldInvoice([]byte, uint64, uint32, string, string) (string, error) {
	f.t.Fatal("CreateHoldInvoice unexpected")
	return "", nil
}
func (f *fakeLNLeg) WaitHeld([]byte, time.Duration) (uint64, error) {
	f.t.Fatal("WaitHeld unexpected")
	return 0, nil
}
func (f *fakeLNLeg) SettleHold([]byte, []byte) error { f.t.Fatal("SettleHold unexpected"); return nil }
func (f *fakeLNLeg) CancelHold([]byte) error         { f.t.Fatal("CancelHold unexpected"); return nil }

var _ LNLeg = (*fakeLNLeg)(nil)

func newTestHashLock(t *testing.T) *HashLock {
	t.Helper()
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return NewHashLock(secret)
}

func TestHashEqualsPreimage(t *testing.T) {
	secret := []byte("the-swap-preimage-value-32-bytes")
	sum := sha256.Sum256(secret)
	if !hashEqualsPreimage(sum[:], secret) {
		t.Fatal("correct preimage must hash to H")
	}
	if hashEqualsPreimage(sum[:], []byte("wrong-preimage")) {
		t.Fatal("a wrong preimage must NOT match H")
	}
	// A truncated/garbage hash must not match.
	if hashEqualsPreimage([]byte{0x00}, secret) {
		t.Fatal("a malformed hash must not match")
	}
}

func TestHexEq(t *testing.T) {
	b := []byte{0xde, 0xad, 0xbe, 0xef}
	if !hexEq("deadbeef", b) {
		t.Fatal("matching hex must compare equal")
	}
	if hexEq("deadbeff", b) {
		t.Fatal("different hex must not compare equal")
	}
	if hexEq("not-hex", b) {
		t.Fatal("malformed hex must be treated as not-equal, never a match")
	}
	if hexEq(hex.EncodeToString(b)+"00", b) {
		t.Fatal("longer hex must not match")
	}
}

// The submarine cross-leg point is unsafe at min_anchor_depth == 1 (design §3.2):
// both flows must refuse a depth < 2 BEFORE doing any I/O (so the fakeLNLeg's
// Fatal guards are never reached).
func TestRunNormalRejectsShallowAnchorDepth(t *testing.T) {
	prim := newTestHashLock(t)
	m := NewSubmarineSwap(nil, &fakeLNLeg{t}, prim)
	for _, depth := range []int64{0, 1} {
		_, err := m.RunNormal(NormalParams{HashH: prim.Hash, MinAnchorDepth: depth}, nil, 0)
		if !errors.Is(err, ErrLNLegInvalid) {
			t.Fatalf("RunNormal(min_anchor_depth=%d) must reject with ErrLNLegInvalid, got %v", depth, err)
		}
	}
}

func TestRunReverseRejectsShallowAnchorDepth(t *testing.T) {
	prim := newTestHashLock(t)
	m := NewSubmarineSwap(nil, &fakeLNLeg{t}, prim)
	for _, depth := range []int64{0, 1} {
		_, err := m.RunReverse(ReverseParams{HashH: prim.Hash, MinAnchorDepth: depth})
		if !errors.Is(err, ErrLNLegInvalid) {
			t.Fatalf("RunReverse(min_anchor_depth=%d) must reject with ErrLNLegInvalid, got %v", depth, err)
		}
	}
}
