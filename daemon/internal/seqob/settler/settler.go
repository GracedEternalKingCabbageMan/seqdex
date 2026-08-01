// Package settler is the always-online, non-custodial SETTLER for the
// fully-passive case of the SeqOB passive CLOB: two covenant-funded resting
// orders whose makers are BOTH offline, matched by the relay and settled against
// each other in ONE transaction.
//
// Where it lives / why a distinct process. The settler is deliberately NOT part
// of seqobd (the relay). The relay is a keyless, fundless message bus that
// matches offers and advertises fills; folding a fee-funded, chain-broadcasting
// actor into it would give the relay a hot wallet and a single point of trust.
// The settler instead runs as its own process (cmd/seqob-settler) that
// (a) subscribes to the relay's both-covenant matches, (b) reads the two
// covenant UTXOs from the chain, (c) plans the joint fill, (d) assembles one tx
// spending both covenants via their FILL leaves plus its OWN bitcoin fee input,
// and (e) broadcasts. Anyone may run one; settlers compete permissionlessly and
// recoup costs from the price spread (a plain fee input suffices for the proof).
//
// Trust model (non-custodial). The settler never holds user funds and cannot
// alter a payout. Every maker-facing output — asset, scriptPubKey, minimum value
// — is fixed by that maker's own FILL leaf and re-checked by the script
// interpreter. A settler that underpays a maker, redirects a credit to itself,
// or aliases both credits onto one output builds a transaction the covenants
// reject. Its only powers are to assemble a CONFORMING tx or not; it is a
// courier, not a custodian. See covenant.PlanJointSettlement for the enforced
// invariants and feature_seqob_joint_covenant.py for the on-chain proof.
package settler

import (
	"encoding/hex"
	"fmt"

	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/seqob/matcher"
	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
)

// UTXO identifies one covenant order output on-chain.
type UTXO struct {
	Txid string
	Vout uint32
}

// Settlement is a ready-to-assemble both-offline joint fill: the enforced
// recipe plus the two covenant outpoints to spend.
type Settlement struct {
	*covenant.JointSettlement
	In0 UTXO // covenant input 0 == orderA (sells asset X)
	In1 UTXO // covenant input 1 == orderB (sells asset Y)
}

// orderFromTerms rebuilds a covenant.Order from the relay's CovenantTerms. Asset
// ids are internal-order hex (matching on-chain introspection), exactly as the
// maker advertised them; the settler re-derives the taptree and re-checks it
// against the on-chain scriptPubKey before spending (VerifyAgainstSPK).
func orderFromTerms(t *seqobv1.CovenantTerms) (covenant.Order, UTXO, error) {
	var o covenant.Order
	var u UTXO
	a, err := hex32(t.GetAssetA())
	if err != nil {
		return o, u, fmt.Errorf("asset_a: %w", err)
	}
	b, err := hex32(t.GetAssetB())
	if err != nil {
		return o, u, fmt.Errorf("asset_b: %w", err)
	}
	if len(t.GetMakerProg()) != 32 {
		return o, u, fmt.Errorf("maker_prog must be 32 bytes")
	}
	var mx, ik [32]byte
	if len(t.GetMakerX()) != 32 {
		return o, u, fmt.Errorf("maker_x must be 32 bytes")
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
	u = UTXO{Txid: t.GetCovenantTxid(), Vout: t.GetCovenantVout()}
	return o, u, nil
}

// Plan reconstructs both orders from a both-covenant Match and builds the joint
// settlement. The resting covenant is treated as orderA (sells asset X = its
// AssetA), the incoming covenant as orderB (sells asset Y); lockedA/lockedB are
// the atoms currently locked in each covenant UTXO and fillA/fillB the atoms
// this settlement takes from each. PlanJointSettlement fails closed unless the
// two orders are genuine counterparties and each maker clears its ceil-price
// floor, so a bad advisory cross from the matcher can never produce a spendable
// (theft) tx.
func Plan(m matcher.Match, lockedA, fillA, lockedB, fillB uint64) (*Settlement, error) {
	if !m.BothCovenant() {
		return nil, fmt.Errorf("settler only handles both-covenant matches")
	}
	orderA, in0, err := orderFromTerms(m.RestingCovenant)
	if err != nil {
		return nil, fmt.Errorf("resting covenant: %w", err)
	}
	orderB, in1, err := orderFromTerms(m.IncomingCovenant)
	if err != nil {
		return nil, fmt.Errorf("incoming covenant: %w", err)
	}
	js, err := covenant.PlanJointSettlement(orderA, lockedA, fillA, orderB, lockedB, fillB)
	if err != nil {
		return nil, err
	}
	return &Settlement{JointSettlement: js, In0: in0, In1: in1}, nil
}

// Broadcaster submits a fully-assembled joint-fill transaction to a node and
// returns its txid. Implemented against elementsd's sendrawtransaction.
type Broadcaster interface {
	Broadcast(rawTxHex string) (txid string, err error)
}

// Assembler turns a planned Settlement into a signed raw Elements transaction:
// it spends In0/In1 with the introspection-only FILL witnesses, lays out the
// credit/remainder/gap outputs, and adds the settler's own bitcoin fee input and
// fee/change outputs. The covenant inputs need no signature; only the settler's
// fee input is signed by the settler's key.
type Assembler interface {
	Assemble(s *Settlement) (rawTxHex string, err error)
}

// Handle plans, assembles, and broadcasts one both-covenant match. This is the
// body of the always-online loop (cmd/seqob-settler run): for each BothCovenant
// match the relay surfaces, resolve the current locked amounts + cross from the
// offers/UTXO set, then Handle.
func Handle(m matcher.Match, lockedA, fillA, lockedB, fillB uint64, asm Assembler, bc Broadcaster) (txid string, err error) {
	s, err := Plan(m, lockedA, fillA, lockedB, fillB)
	if err != nil {
		return "", fmt.Errorf("plan: %w", err)
	}
	raw, err := asm.Assemble(s)
	if err != nil {
		return "", fmt.Errorf("assemble: %w", err)
	}
	return bc.Broadcast(raw)
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
