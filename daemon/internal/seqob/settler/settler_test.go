package settler

import (
	"bytes"
	"encoding/hex"
	"testing"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// terms builds CovenantTerms for a covenant that sells assetA wanting assetB.
func terms(assetA, assetB [32]byte, num, den, minlot uint64, prog, makerx []byte, txid string, vout uint32) *seqobv1.CovenantTerms {
	return &seqobv1.CovenantTerms{
		CovenantTxid: txid, CovenantVout: vout,
		AssetA: hex.EncodeToString(assetA[:]), AssetB: hex.EncodeToString(assetB[:]),
		RateNum: num, RateDen: den, MinLot: minlot,
		MakerProg: prog, MakerProgVer: 1,
		ExpiryLocktime: 400, MakerX: makerx, InternalKey: covenant.NUMS[:],
	}
}

func TestPlanFromBothCovenantMatch(t *testing.T) {
	var x, y [32]byte
	for i := 0; i < 32; i++ {
		x[i] = byte(i)
		y[i] = byte(32 + i)
	}
	prog0 := bytes.Repeat([]byte{0x11}, 32)
	prog1 := bytes.Repeat([]byte{0x44}, 32)
	mx0 := bytes.Repeat([]byte{0x22}, 32)
	mx1 := bytes.Repeat([]byte{0x33}, 32)

	m := matcher.Match{
		RestingCovenant:  terms(x, y, 3, 1, 5, prog0, mx0, repeat("aa", 32), 0),
		IncomingCovenant: terms(y, x, 1, 3, 5, prog1, mx1, repeat("bb", 32), 1),
	}
	if !m.BothCovenant() {
		t.Fatal("expected both-covenant match")
	}
	s, err := Plan(m, 30, 30, 90, 90)
	if err != nil {
		t.Fatal(err)
	}
	if s.In0.Txid != repeat("aa", 32) || s.In1.Vout != 1 {
		t.Fatalf("outpoints not carried through: %+v %+v", s.In0, s.In1)
	}
	if s.Leg0.CreditValue != 90 || s.Leg1.CreditValue != 30 {
		t.Fatalf("credits %d/%d, want 90/30", s.Leg0.CreditValue, s.Leg1.CreditValue)
	}
	// maker0 (resting) receives Y; maker1 (incoming) receives X.
	if !bytes.Equal(s.Leg0.CreditAsset[:], y[:]) || !bytes.Equal(s.Leg1.CreditAsset[:], x[:]) {
		t.Fatal("credit assets wrong")
	}
}

func TestPlanRejectsSingleCovenant(t *testing.T) {
	var x, y [32]byte
	m := matcher.Match{RestingCovenant: terms(x, y, 1, 1, 1, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), repeat("aa", 32), 0)}
	if _, err := Plan(m, 1, 1, 1, 1); err == nil {
		t.Fatal("expected rejection of non-both-covenant match")
	}
}

func repeat(h string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += h
	}
	return out
}
