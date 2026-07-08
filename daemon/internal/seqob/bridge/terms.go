package bridge

import (
	"encoding/hex"
	"fmt"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// Outpoint is the funded covenant order UTXO the bridge fills.
type Outpoint struct {
	Txid string
	Vout uint32
}

// OrderFromTerms rebuilds a covenant.Order (and its funding outpoint) from the
// relay's advertised CovenantTerms. Asset ids/keys are internal-order hex,
// matching on-chain introspection exactly as the maker advertised them; the
// bridge re-derives the taptree and re-checks it against the on-chain
// scriptPubKey (covenant.Order.VerifyAgainstSPK) before spending, so a lying
// relay cannot point it at a UTXO that pays anyone but the maker. This mirrors
// the settler's orderFromTerms (both consume the same wire terms); the two are
// intentionally independent thin adapters, not a shared fork of pkg/covenant.
func OrderFromTerms(t *seqobv1.CovenantTerms) (covenant.Order, error) {
	var o covenant.Order
	a, err := hex32(t.GetAssetA())
	if err != nil {
		return o, fmt.Errorf("asset_a: %w", err)
	}
	b, err := hex32(t.GetAssetB())
	if err != nil {
		return o, fmt.Errorf("asset_b: %w", err)
	}
	if len(t.GetMakerProg()) != 32 {
		return o, fmt.Errorf("maker_prog must be 32 bytes")
	}
	var mx, ik [32]byte
	if len(t.GetMakerX()) != 32 {
		return o, fmt.Errorf("maker_x must be 32 bytes")
	}
	copy(mx[:], t.GetMakerX())
	if ikb := t.GetInternalKey(); len(ikb) == 32 {
		copy(ik[:], ikb)
	} else {
		ik = covenant.NUMS
	}
	o = covenant.Order{
		AssetA:         a,
		AssetB:         b,
		RateNum:        t.GetRateNum(),
		RateDen:        t.GetRateDen(),
		MakerProg:      t.GetMakerProg(),
		MakerVer:       int(t.GetMakerProgVer()),
		MinLot:         t.GetMinLot(),
		ExpiryLocktime: int64(t.GetExpiryLocktime()),
		MakerX:         mx,
		InternalKey:    ik,
	}
	return o, nil
}

// OutpointFromTerms returns the covenant funding outpoint the bridge spends.
func OutpointFromTerms(t *seqobv1.CovenantTerms) Outpoint {
	return Outpoint{Txid: t.GetCovenantTxid(), Vout: t.GetCovenantVout()}
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("want 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}
