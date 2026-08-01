package settler

import (
	"bytes"
	"testing"

	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// btcAsset is a synthetic pegged "bitcoin" asset (internal order), distinct from
// the test's X (bytes 0..31) and Y (bytes 32..63) so a gap output never carries a
// sold asset.
var btcAsset = func() [32]byte {
	var b [32]byte
	for i := range b {
		b[i] = 0xee
	}
	return b
}()

// gapScript is a stand-in settler-owned bitcoin scriptPubKey for gap outputs.
var gapScript = append([]byte{0x00, 0x14}, bytes.Repeat([]byte{0x99}, 20)...)

// stubFunder is a node-free FeeFunder for the structural test: it appends a
// synthetic bitcoin input (index 2) and a change + fee output WITHOUT signing, so
// the test can verify the covenant witnesses inject onto inputs 0,1 and the
// covenant-read outputs stay fixed at indices 0-3 after funding.
type stubFunder struct{}

func (stubFunder) GapSPK() ([]byte, error) { return gapScript, nil }

func (stubFunder) FundAndSign(skeletonHex string, gapTotal uint64) (string, error) {
	tx, err := transaction.NewTxFromHex(skeletonHex)
	if err != nil {
		return "", err
	}
	h, _ := elementsutil.TxIDToBytes(repeat("cc", 32))
	tx.AddInput(transaction.NewTxInput(h, 0)) // synthetic settler bitcoin input (index 2)
	btc := assetCommit(btcAsset)
	cv, _ := elementsutil.ValueToBytes(100000) // change
	tx.AddOutput(transaction.NewTxOutput(btc, cv, gapScript))
	fv, _ := elementsutil.ValueToBytes(5000) // explicit fee
	tx.AddOutput(transaction.NewTxOutput(btc, fv, []byte{}))
	return tx.ToHex()
}

// jointMatch builds a both-covenant match: order0 sells X wanting Y at 3 Y/X,
// order1 sells Y wanting X at 1 X/3 Y (the regtest counterparties).
func jointMatch() matcher.Match {
	var x, y [32]byte
	for i := 0; i < 32; i++ {
		x[i] = byte(i)
		y[i] = byte(32 + i)
	}
	prog0 := bytes.Repeat([]byte{0x11}, 32)
	prog1 := bytes.Repeat([]byte{0x44}, 32)
	mx0 := bytes.Repeat([]byte{0x22}, 32)
	mx1 := bytes.Repeat([]byte{0x33}, 32)
	return matcher.Match{
		RestingCovenant:  terms(x, y, 3, 1, 5, prog0, mx0, repeat("aa", 32), 0),
		IncomingCovenant: terms(y, x, 1, 3, 5, prog1, mx1, repeat("bb", 32), 1),
	}
}

func assembleForTest(t *testing.T, lockedA, fillA, lockedB, fillB uint64) (*Settlement, *transaction.Transaction) {
	t.Helper()
	s, err := Plan(jointMatch(), lockedA, fillA, lockedB, fillB)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	asm := &NodeAssembler{Bitcoin: btcAsset, GapValue: 10000, Funder: stubFunder{}}
	rawHex, err := asm.Assemble(s)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	tx, err := transaction.NewTxFromHex(rawHex)
	if err != nil {
		t.Fatalf("parse assembled tx: %v", err)
	}
	return s, tx
}

func assertCovenantWitnesses(t *testing.T, s *Settlement, tx *transaction.Transaction) {
	t.Helper()
	if len(tx.Inputs) != 3 {
		t.Fatalf("inputs = %d, want 3 (2 covenant + 1 settler bitcoin)", len(tx.Inputs))
	}
	for i, leg := range []struct {
		leaf, control []byte
	}{
		{s.Leg0.FillLeaf, s.Leg0.ControlBlock},
		{s.Leg1.FillLeaf, s.Leg1.ControlBlock},
	} {
		w := tx.Inputs[i].Witness
		if len(w) != 2 {
			t.Fatalf("input %d witness has %d items, want 2 [leaf, control_block]", i, len(w))
		}
		if !bytes.Equal(w[0], leg.leaf) {
			t.Errorf("input %d witness[0] != fill_leaf", i)
		}
		if !bytes.Equal(w[1], leg.control) {
			t.Errorf("input %d witness[1] != control_block", i)
		}
	}
	// The settler's own bitcoin input carries no covenant witness (unsigned by the stub).
	if len(tx.Inputs[2].Witness) != 0 {
		t.Errorf("settler bitcoin input unexpectedly has a %d-item witness", len(tx.Inputs[2].Witness))
	}
}

func assertOutput(t *testing.T, out *transaction.TxOutput, wantAsset []byte, wantValue uint64, wantSPK []byte, label string) {
	t.Helper()
	if !bytes.Equal(out.Asset, wantAsset) {
		t.Errorf("%s: asset = %x, want %x", label, out.Asset, wantAsset)
	}
	v, err := elementsutil.ValueFromBytes(out.Value)
	if err != nil {
		t.Fatalf("%s: value decode: %v", label, err)
	}
	if v != wantValue {
		t.Errorf("%s: value = %d, want %d", label, v, wantValue)
	}
	if !bytes.Equal(out.Script, wantSPK) {
		t.Errorf("%s: spk = %x, want %x", label, out.Script, wantSPK)
	}
}

// TestAssembleBothFullFill proves the exact-cross layout: two full fills, both
// 2k+1 slots settler bitcoin gaps (GapSlots [1,3]), credits at 0 and 2.
func TestAssembleBothFullFill(t *testing.T) {
	s, tx := assembleForTest(t, 30, 30, 90, 90)

	if got := []uint32(s.GapSlots); len(got) != 2 {
		t.Fatalf("gap slots = %v, want [1 3]", got)
	}
	assertCovenantWitnesses(t, s, tx)

	// Four covenant-read outputs stay at 0-3, then the stub's change + fee (6 total).
	if len(tx.Outputs) != 6 {
		t.Fatalf("outputs = %d, want 6 (4 covenant + change + fee)", len(tx.Outputs))
	}

	var x, y [32]byte
	for i := 0; i < 32; i++ {
		x[i] = byte(i)
		y[i] = byte(32 + i)
	}
	// Output 0: maker0 credited all Y (90) at its own payout spk.
	assertOutput(t, tx.Outputs[0], assetCommit(y), 90, s.Leg0.CreditSPK, "credit0")
	// Output 2: maker1 credited all X (30) at its own payout spk.
	assertOutput(t, tx.Outputs[2], assetCommit(x), 30, s.Leg1.CreditSPK, "credit1")
	// Outputs 1 and 3: settler bitcoin gaps (full fills have no remainder).
	assertOutput(t, tx.Outputs[1], assetCommit(btcAsset), 10000, gapScript, "gap-slot-1")
	assertOutput(t, tx.Outputs[3], assetCommit(btcAsset), 10000, gapScript, "gap-slot-3")
}

// TestAssemblePartialFill proves the partial-cross layout: maker1 over-supplies,
// so leg1's 2k+1 slot re-rests the remainder covenant while leg0 (full) still uses
// a bitcoin gap. Only leg0 needs a gap (GapSlots [1]).
func TestAssemblePartialFill(t *testing.T) {
	s, tx := assembleForTest(t, 30, 30, 180, 90)

	if !s.Leg1.Partial || s.Leg0.Partial {
		t.Fatalf("expected leg0 full + leg1 partial, got leg0.partial=%v leg1.partial=%v", s.Leg0.Partial, s.Leg1.Partial)
	}
	if got := []uint32(s.GapSlots); len(got) != 1 || got[0] != 1 {
		t.Fatalf("gap slots = %v, want [1] (only leg0 full)", got)
	}
	assertCovenantWitnesses(t, s, tx)

	var x, y [32]byte
	for i := 0; i < 32; i++ {
		x[i] = byte(i)
		y[i] = byte(32 + i)
	}
	// Output 0: maker0 credited the Y taken (90).
	assertOutput(t, tx.Outputs[0], assetCommit(y), 90, s.Leg0.CreditSPK, "credit0")
	// Output 2: maker1 credited all X (30).
	assertOutput(t, tx.Outputs[2], assetCommit(x), 30, s.Leg1.CreditSPK, "credit1")
	// Output 1: leg0 full -> settler bitcoin gap.
	assertOutput(t, tx.Outputs[1], assetCommit(btcAsset), 10000, gapScript, "gap-slot-1")
	// Output 3: leg1 partial -> remainder covenant re-rested to leg1's own spk in Y.
	assertOutput(t, tx.Outputs[3], assetCommit(y), 90, s.Leg1.OrderSPK, "remainder-slot-3")
}
