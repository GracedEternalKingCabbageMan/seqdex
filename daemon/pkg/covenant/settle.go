package covenant

import (
	"bytes"
	"fmt"
)

// This file adds the fully-passive settlement recipe on top of PlanJointFill:
// the complete, broadcast-ready JOINT-FILL tx layout for the case where BOTH
// makers are offline and an untrusted, always-online settler assembles one tx
// that spends BOTH covenant UTXOs against each other.
//
// PlanJointFill gives the two per-input FILL plans (the fixed 2k/2k+1 index
// map). PlanJointSettlement completes them into an assemblable transaction:
//
//   - it proves the two orders are genuine counterparties (orderA sells asset X
//     wanting Y; orderB sells Y wanting X), so orderA's locked X pays orderB's
//     maker and vice versa;
//   - it fixes every output slot the two covenants read, including the two
//     RESERVED slots at 2k+1. When a side is a FULL fill it has no remainder,
//     but its 2k+1 slot is still inspected by the leaf (output 2 always exists,
//     so index 1 and 3 are always < num_outputs). The leaf reads that slot's
//     asset and, ONLY if it equals the covenant's own sold asset, treats it as
//     an underpaid remainder. So a full-fill side's 2k+1 slot must carry an
//     EXPLICIT, non-(sold-asset) output — a settler-funded bitcoin gap output.
//     PlanJointSettlement surfaces exactly which slots need that (GapSlots).
//   - it computes each maker's credit value from asset conservation (maker0 is
//     paid ALL the Y taken from covB; maker1 ALL the X taken from covA) and
//     asserts each credit clears its own covenant's ceil-price floor.
//
// Trust model: the settler holds NO user funds and cannot alter a payout. The
// only thing it contributes is a bitcoin fee/gap input and the broadcast; every
// maker-facing output (asset, scriptPubKey, min value) is fixed by that maker's
// own FILL leaf and re-checked by the interpreter, so a settler that underpays
// a maker, redirects a credit, or aliases both credits onto one output produces
// a tx the covenant rejects (script-verify-flag-failed). Non-custodial by
// construction.

// CovLeg is one covenant input's complete contribution to the joint tx: its
// introspection-only witness, the maker-credit output it forces, and its
// remainder (self-replicated covenant) output for a partial fill.
type CovLeg struct {
	InputIndex uint32 // k: consensus input index (0 or 1)

	CreditIndex uint32   // 2k: the maker-credit output
	CreditAsset [32]byte // asset the maker receives (== this order's AssetB)
	CreditSPK   []byte   // the maker payout scriptPubKey (OP_1 <maker_prog>)
	CreditValue uint64   // atoms to pay the maker == the OTHER covenant's fill
	CreditFloor uint64   // the covenant's own ceil-price minimum (CreditValue >= this)

	RemainderIndex uint32   // 2k+1: remainder slot (self-replicated covenant, or a gap)
	Partial        bool     // true => remainder output; false => a gap output is required here
	Remainder      uint64   // asset-A (sold-asset) atoms re-paid to the covenant (partial only)
	RemainderAsset [32]byte // == this order's AssetA (the sold asset)
	OrderSPK       []byte   // this covenant's scriptPubKey (utxo spk AND partial remainder dest)

	FillLeaf     []byte // witness item 0
	ControlBlock []byte // witness item 1
}

// Witness is the covenant input's [leaf, control_block] stack.
func (l *CovLeg) Witness() [][]byte { return [][]byte{l.FillLeaf, l.ControlBlock} }

// JointSettlement is the complete both-offline settlement recipe: two covenant
// legs settled against each other in one tx, plus the reserved slots the settler
// must fill with its own bitcoin.
type JointSettlement struct {
	Leg0 CovLeg // input 0: orderA, sells asset X
	Leg1 CovLeg // input 1: orderB, sells asset Y

	// GapSlots are the output indices (a subset of {1,3}) that belong to a
	// FULL-fill leg. The settler MUST place an explicit output there whose asset
	// is NOT that leg's sold asset (a bitcoin gap output), so the leaf reads a
	// zero remainder. Omitting the slot, blinding it, or paying the sold asset
	// there makes the covenant reject the tx.
	GapSlots []uint32

	// MinOutputs is the number of output slots the two covenants read (always 4:
	// credits at 0,2 and reserved slots at 1,3). The settler appends its fee and
	// change after these.
	MinOutputs uint32
}

// makerCreditSPK is the v1-taproot maker payout scriptPubKey OP_1 <32-byte prog>.
func makerCreditSPK(o Order) ([]byte, error) {
	if o.MakerVer != 1 || len(o.MakerProg) != 32 {
		return nil, fmt.Errorf("maker payout must be a v1 taproot program (32 bytes)")
	}
	return append([]byte{op1, 0x20}, o.MakerProg...), nil
}

// PlanJointSettlement builds the complete both-offline joint-fill recipe.
//
// orderA sells asset X (== orderA.AssetA) and wants Y (== orderA.AssetB); it has
// lockedA atoms of X locked, of which fillA are taken by this settlement.
// orderB is its counterparty: it sells Y (== orderB.AssetA == orderA.AssetB) and
// wants X (== orderB.AssetB == orderA.AssetA), lockedB of Y locked, fillB taken.
//
// The settler contributes only a bitcoin fee/gap input; it cannot change any
// maker payout. PlanJointSettlement fails closed if the orders are not genuine
// counterparties, if a fill is out of range or below a covenant floor, or if the
// chosen cross does not pay each maker its ceil price.
func PlanJointSettlement(orderA Order, lockedA, fillA uint64, orderB Order, lockedB, fillB uint64) (*JointSettlement, error) {
	// (1) The two orders must be exact counterparties, else X taken from covA
	// cannot pay covB's maker (and vice versa) and the joint spend is meaningless.
	if orderA.AssetA != orderB.AssetB {
		return nil, fmt.Errorf("not counterparties: orderA sells %x but orderB does not want it (%x)", orderA.AssetA, orderB.AssetB)
	}
	if orderA.AssetB != orderB.AssetA {
		return nil, fmt.Errorf("not counterparties: orderB sells %x but orderA does not want it (%x)", orderB.AssetA, orderA.AssetB)
	}

	// (2) Per-input FILL plans on the fixed index map (covA=input 0, covB=input 1).
	jp, err := PlanJointFill(orderA, lockedA, fillA, orderB, lockedB, fillB)
	if err != nil {
		return nil, err
	}

	// (3) Asset conservation fixes each maker's credit VALUE: all Y taken from
	// covB (fillB) pays maker0; all X taken from covA (fillA) pays maker1. The
	// covenant floors are jp.A.RequiredB (Y maker0 needs) and jp.B.RequiredB
	// (X maker1 needs). The cross is valid iff the value actually delivered
	// clears each floor.
	if fillB < jp.A.RequiredB {
		return nil, fmt.Errorf("cross underpays maker0: pays %d Y but covenant needs %d (ceil price)", fillB, jp.A.RequiredB)
	}
	if fillA < jp.B.RequiredB {
		return nil, fmt.Errorf("cross underpays maker1: pays %d X but covenant needs %d (ceil price)", fillA, jp.B.RequiredB)
	}

	spk0, err := makerCreditSPK(orderA)
	if err != nil {
		return nil, fmt.Errorf("maker0: %w", err)
	}
	spk1, err := makerCreditSPK(orderB)
	if err != nil {
		return nil, fmt.Errorf("maker1: %w", err)
	}

	leg0 := CovLeg{
		InputIndex:     jp.A.InputIndex,
		CreditIndex:    jp.A.CreditIndex,
		CreditAsset:    orderA.AssetB, // maker0 receives Y
		CreditSPK:      spk0,
		CreditValue:    fillB, // all Y taken from covB
		CreditFloor:    jp.A.RequiredB,
		RemainderIndex: jp.A.RemainderIndex,
		Partial:        jp.A.Partial,
		Remainder:      jp.A.Remainder,
		RemainderAsset: orderA.AssetA, // X re-paid to covA
		OrderSPK:       jp.A.OrderSPK,
		FillLeaf:       jp.A.FillLeaf,
		ControlBlock:   jp.A.ControlBlock,
	}
	leg1 := CovLeg{
		InputIndex:     jp.B.InputIndex,
		CreditIndex:    jp.B.CreditIndex,
		CreditAsset:    orderB.AssetB, // maker1 receives X
		CreditSPK:      spk1,
		CreditValue:    fillA, // all X taken from covA
		CreditFloor:    jp.B.RequiredB,
		RemainderIndex: jp.B.RemainderIndex,
		Partial:        jp.B.Partial,
		Remainder:      jp.B.Remainder,
		RemainderAsset: orderB.AssetA, // Y re-paid to covB
		OrderSPK:       jp.B.OrderSPK,
		FillLeaf:       jp.B.FillLeaf,
		ControlBlock:   jp.B.ControlBlock,
	}

	// (4) The anti-aliasing bijection: the two maker credits land on DISTINCT
	// outputs by construction (0 and 2). Assert it — a collision here would let
	// one payment satisfy both covenants.
	if leg0.CreditIndex == leg1.CreditIndex {
		return nil, fmt.Errorf("credit outputs alias (%d) — index map broken", leg0.CreditIndex)
	}
	// The credit and remainder slots must also be pairwise distinct.
	slots := []uint32{leg0.CreditIndex, leg0.RemainderIndex, leg1.CreditIndex, leg1.RemainderIndex}
	seen := map[uint32]bool{}
	for _, s := range slots {
		if seen[s] {
			return nil, fmt.Errorf("output slot %d reused — index map broken", s)
		}
		seen[s] = true
	}

	// (5) The reserved 2k+1 slots of a FULL-fill leg must be settler gap outputs.
	var gaps []uint32
	if !leg0.Partial {
		gaps = append(gaps, leg0.RemainderIndex)
	}
	if !leg1.Partial {
		gaps = append(gaps, leg1.RemainderIndex)
	}

	return &JointSettlement{
		Leg0:       leg0,
		Leg1:       leg1,
		GapSlots:   gaps,
		MinOutputs: 4,
	}, nil
}

// GapAsset reports whether `asset` is safe to place at a full-fill gap slot for
// the given leg: it must be explicit and NOT the leg's sold asset (else the leaf
// reads it as an underpaid remainder). Used by an assembler to pick a bitcoin
// gap output. `asset` is the 32-byte internal-order id the gap output carries.
func (l *CovLeg) GapAsset(asset [32]byte) error {
	if bytes.Equal(asset[:], l.RemainderAsset[:]) {
		return fmt.Errorf("gap slot %d must not carry the sold asset %x", l.RemainderIndex, l.RemainderAsset)
	}
	return nil
}
