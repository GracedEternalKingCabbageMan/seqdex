package settler

import (
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/covenant"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/transaction"
)

// FeeFunder contributes the settler's OWN bitcoin to a covenant skeleton tx: it
// appends the settler's fee input(s), a bitcoin change output, and the explicit
// Elements fee output, and signs the bitcoin input(s). The covenant inputs (0,1)
// are left UNSIGNED — the assembler injects their introspection-only FILL
// witnesses afterwards. It also sources settler-owned scriptPubKeys for the
// full-fill gap outputs (bitcoin the settler pays straight back to itself).
//
// Splitting this out keeps NodeAssembler's covenant layout (the part the two
// covenants enforce) pure and node-free: in production the funder binds to a node
// wallet (listunspent + signrawtransactionwithwallet, see NodeFunder); a test
// stubs it so the structural assembly can be exercised with no node.
type FeeFunder interface {
	// GapSPK returns a settler-owned bitcoin scriptPubKey to receive a full-fill
	// gap output. The settler funds it and gets it straight back, so it must pay a
	// script the settler controls.
	GapSPK() ([]byte, error)

	// FundAndSign appends the settler's bitcoin fee input(s), a change output and
	// the explicit Elements fee output to the serialized skeleton (covenant inputs
	// 0,1 plus the four covenant-read outputs, of which gapTotal atoms are already
	// committed to settler bitcoin gap outputs), signs the bitcoin input(s), and
	// returns the serialized tx. It only APPENDS — output indices 0-3 (the
	// covenant-read slots) are preserved so the covenants' fixed 2k/2k+1 index map
	// still holds.
	FundAndSign(skeletonHex string, gapTotal uint64) (signedHex string, err error)
}

// NodeAssembler implements Assembler. It builds the two covenant inputs and the
// four covenant-read outputs (the parts the covenant FILL leaves enforce) from
// the already-validated JointSettlement, delegates the bitcoin fee/gap funding +
// signing to a FeeFunder, then injects the two introspection-only FILL witnesses.
//
// Non-custodial by construction: every maker-facing output (credit asset,
// scriptPubKey, value; remainder) comes straight from covenant.PlanJointSettlement
// and is re-checked by the script interpreter. A wrong assembly can only produce a
// tx the covenant script REJECTS on broadcast, never a theft or a fund loss, so
// the assembler builds conservatively and lets the chain be the backstop.
type NodeAssembler struct {
	// Bitcoin is the chain's pegged "bitcoin" asset id in INTERNAL byte order
	// (32 bytes, i.e. the display id reversed). It denominates the gap/change/fee
	// outputs and is validated against each full-fill leg's sold asset (a gap must
	// never carry the sold asset, else the leaf reads it as an underpaid remainder).
	Bitcoin [32]byte
	// GapValue is the atom value of each full-fill gap output (settler-funded
	// bitcoin returned to the settler; a small dust-safe amount, e.g. 10000).
	GapValue uint64
	// Funder contributes the settler's bitcoin fee/gap and signs it (a node wallet
	// in production; a stub in tests).
	Funder FeeFunder
}

// Assemble builds and returns the serialized, broadcast-ready joint-fill tx hex.
func (a *NodeAssembler) Assemble(s *Settlement) (string, error) {
	if a.Funder == nil {
		return "", fmt.Errorf("assembler has no fee funder")
	}
	if s.JointSettlement == nil {
		return "", fmt.Errorf("nil joint settlement")
	}

	// A full-fill leg needs a settler-owned bitcoin gap output at its 2k+1 slot;
	// fetch one settler script if any leg is a full fill (both gaps reuse it —
	// duplicate output scripts are permitted and both are settler-owned).
	var gapSPK []byte
	if len(s.GapSlots) > 0 {
		var err error
		gapSPK, err = a.Funder.GapSPK()
		if err != nil {
			return "", fmt.Errorf("gap scriptPubKey: %w", err)
		}
	}

	outs, gapTotal, err := a.buildCovenantOutputs(s, gapSPK)
	if err != nil {
		return "", err
	}

	// Skeleton: covenant inputs 0,1 (unsigned) + the four covenant-read outputs.
	tx := transaction.NewTx(2)
	in0, err := covInput(s.In0)
	if err != nil {
		return "", fmt.Errorf("covenant0 input: %w", err)
	}
	in1, err := covInput(s.In1)
	if err != nil {
		return "", fmt.Errorf("covenant1 input: %w", err)
	}
	tx.AddInput(in0)
	tx.AddInput(in1)
	for _, o := range outs {
		tx.AddOutput(o)
	}
	skeletonHex, err := tx.ToHex()
	if err != nil {
		return "", fmt.Errorf("serialize skeleton: %w", err)
	}

	// Fund the bitcoin side (fee + gap change) and sign the settler's bitcoin
	// input(s). The covenant inputs stay unsigned; the funder only appends.
	signedHex, err := a.Funder.FundAndSign(skeletonHex, gapTotal)
	if err != nil {
		return "", fmt.Errorf("fund+sign: %w", err)
	}

	// Inject the two introspection-only FILL witnesses. The funder only appended
	// its input(s) after index 1, so the covenant inputs are still 0 and 1.
	ftx, err := transaction.NewTxFromHex(signedHex)
	if err != nil {
		return "", fmt.Errorf("parse funded tx: %w", err)
	}
	if len(ftx.Inputs) < 2 {
		return "", fmt.Errorf("funded tx has %d inputs, want >= 2 covenant inputs", len(ftx.Inputs))
	}
	ftx.Inputs[0].Witness = s.Leg0.Witness()
	ftx.Inputs[1].Witness = s.Leg1.Witness()
	return ftx.ToHex()
}

// buildCovenantOutputs lays out the four covenant-read outputs from the enforced
// plan: the two maker credits at 0,2 and each leg's 2k+1 slot (a self-replicated
// remainder covenant for a partial fill, else a settler bitcoin gap output). It
// returns the outputs and the total bitcoin the settler commits to gap slots.
// Pure — no node access; the gap scriptPubKey is supplied by the caller.
func (a *NodeAssembler) buildCovenantOutputs(s *Settlement, gapSPK []byte) ([]*transaction.TxOutput, uint64, error) {
	// The two covenants always read 4 slots: credits at 0,2 and reserved slots at
	// 1,3 (covenant.PlanJointSettlement fixes MinOutputs = 4).
	n := int(s.MinOutputs)
	if n < 4 {
		return nil, 0, fmt.Errorf("min_outputs %d < 4", n)
	}
	outs := make([]*transaction.TxOutput, n)
	btcCommit := assetCommit(a.Bitcoin)

	var gapTotal uint64
	for _, leg := range []*covenant.CovLeg{&s.Leg0, &s.Leg1} {
		// Maker credit (asset B credited to the maker at output 2k).
		if int(leg.CreditIndex) >= n {
			return nil, 0, fmt.Errorf("credit index %d out of range (%d outputs)", leg.CreditIndex, n)
		}
		creditVal, err := elementsutil.ValueToBytes(leg.CreditValue)
		if err != nil {
			return nil, 0, err
		}
		outs[leg.CreditIndex] = transaction.NewTxOutput(assetCommit(leg.CreditAsset), creditVal, leg.CreditSPK)

		// The 2k+1 slot: a remainder covenant (partial) or a bitcoin gap (full).
		if int(leg.RemainderIndex) >= n {
			return nil, 0, fmt.Errorf("remainder index %d out of range (%d outputs)", leg.RemainderIndex, n)
		}
		if leg.Partial {
			remVal, err := elementsutil.ValueToBytes(leg.Remainder)
			if err != nil {
				return nil, 0, err
			}
			// Re-rest the leftover sold-asset (asset A) back to the covenant's own spk.
			outs[leg.RemainderIndex] = transaction.NewTxOutput(assetCommit(leg.RemainderAsset), remVal, leg.OrderSPK)
		} else {
			// Full fill: the slot must be an explicit, NON-(sold-asset) output, else
			// the leaf reads it as an underpaid remainder and rejects the tx.
			if err := leg.GapAsset(a.Bitcoin); err != nil {
				return nil, 0, err
			}
			if gapSPK == nil {
				return nil, 0, fmt.Errorf("full-fill leg needs a gap scriptPubKey but none was provided")
			}
			gapVal, err := elementsutil.ValueToBytes(a.GapValue)
			if err != nil {
				return nil, 0, err
			}
			outs[leg.RemainderIndex] = transaction.NewTxOutput(btcCommit, gapVal, gapSPK)
			gapTotal += a.GapValue
		}
	}

	// Every covenant-read slot must be filled; a nil would mean the index map broke.
	for i, o := range outs {
		if o == nil {
			return nil, 0, fmt.Errorf("output slot %d unfilled — index map broken", i)
		}
	}
	return outs, gapTotal, nil
}

// assetCommit builds the on-chain explicit asset commitment (0x01 || 32-byte
// internal-order id) for an internal-order asset id.
func assetCommit(internal [32]byte) []byte {
	out := make([]byte, 0, 33)
	out = append(out, 0x01)
	out = append(out, internal[:]...)
	return out
}

// covInput builds an unsigned tx input spending a covenant outpoint.
func covInput(u UTXO) (*transaction.TxInput, error) {
	h, err := elementsutil.TxIDToBytes(u.Txid)
	if err != nil {
		return nil, fmt.Errorf("txid %q: %w", u.Txid, err)
	}
	return transaction.NewTxInput(h, u.Vout), nil
}
