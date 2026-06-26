package swap

import (
	"bytes"
	"fmt"

	"github.com/vulpemventures/go-elements/confidential"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/psetv2"
	"github.com/vulpemventures/go-elements/transaction"
)

// ValidateProposerReceiveV2Opts configures ValidateProposerReceiveV2.
type ValidateProposerReceiveV2Opts struct {
	// ProposerPsetBase64 is the proposer's OWN request PSETv2 (the half it built
	// before the responder co-signed). Its inputs are the proposer's funding
	// inputs; they must survive into the final tx unchanged.
	ProposerPsetBase64 string
	// FinalPsetBase64 is the responder-returned co-signed PSETv2 (the
	// SwapAccept.transaction the proposer is about to sign).
	FinalPsetBase64 string
	// RecvScript is the proposer's own receive scriptPubKey (the script its
	// receive output must pay to).
	RecvScript []byte
	// RecvBlindingKey is the proposer's 32-byte private blinding key, used to
	// unblind a confidential receive output. May be nil for an explicit receive.
	RecvBlindingKey []byte
	// AssetR / AmountR are the asset and exact amount the proposer must receive.
	AssetR  string
	AmountR uint64
}

// ValidateProposerReceiveV2 is the PSETv2 sibling of ValidateCompletePset, scoped
// to protect the PROPOSER (the seqob taker) before it signs a responder-returned
// swap. ValidateCompletePset itself cannot be used here: it parses with the
// PSETv0 pset.NewPsetFromBase64 and matches a tdexv1.SwapRequest's amounts
// ANYWHERE in the tx (not to the proposer's own script), whereas the seqob path
// is PSETv2 and the theft this guards against is the responder reducing,
// removing, or redirecting the proposer's receive output (or swapping out the
// proposer's funding inputs) right before the proposer signs.
//
// It asserts:
//
//  1. the final tx contains an output to RecvScript paying EXACTLY AmountR of
//     AssetR (unblinding a confidential receive with RecvBlindingKey); and
//  2. every input the proposer contributed (by outpoint) is still present in the
//     final tx, so the responder cannot substitute the proposer's inputs.
func ValidateProposerReceiveV2(opts ValidateProposerReceiveV2Opts) error {
	if opts.AmountR == 0 || opts.AssetR == "" {
		return fmt.Errorf("missing expected receive leg (asset_r/amount_r)")
	}
	if len(opts.RecvScript) == 0 {
		return fmt.Errorf("missing proposer receive script")
	}

	final, err := psetv2.NewPsetFromBase64(opts.FinalPsetBase64)
	if err != nil {
		return fmt.Errorf("parse final pset: %w", err)
	}
	finalTx, err := final.UnsignedTx()
	if err != nil {
		return fmt.Errorf("final unsigned tx: %w", err)
	}

	// (1) receive leg: an output to the proposer's OWN script paying exactly
	// AmountR of AssetR.
	paid, err := outputToScriptPaysExactly(
		finalTx.Outputs, opts.RecvScript, opts.AssetR, opts.AmountR, opts.RecvBlindingKey,
	)
	if err != nil {
		return err
	}
	if !paid {
		return fmt.Errorf(
			"co-signed tx does not pay the proposer %d of asset %s to its own script",
			opts.AmountR, opts.AssetR,
		)
	}

	// (2) the proposer's funding inputs must all survive into the final tx.
	proposer, err := psetv2.NewPsetFromBase64(opts.ProposerPsetBase64)
	if err != nil {
		return fmt.Errorf("parse proposer pset: %w", err)
	}
	present := make(map[string]bool, len(final.Inputs))
	for _, in := range final.Inputs {
		present[outpointKey(in.PreviousTxid, in.PreviousTxIndex)] = true
	}
	for _, in := range proposer.Inputs {
		if !present[outpointKey(in.PreviousTxid, in.PreviousTxIndex)] {
			return fmt.Errorf(
				"co-signed tx is missing proposer input %x:%d",
				in.PreviousTxid, in.PreviousTxIndex,
			)
		}
	}
	return nil
}

func outpointKey(txid []byte, vout uint32) string {
	return fmt.Sprintf("%x:%d", txid, vout)
}

// outputToScriptPaysExactly reports whether outputs contains an output to script
// paying exactly amount of asset (asset given as the display asset id). A
// confidential output is unblinded with blindingKey (the recipient's 32-byte
// private blinding key); an explicit output is read directly.
func outputToScriptPaysExactly(
	outputs []*transaction.TxOutput, script []byte, asset string, amount uint64, blindingKey []byte,
) (bool, error) {
	for _, out := range outputs {
		if !bytes.Equal(out.Script, script) {
			continue
		}
		if out.IsConfidential() {
			if len(blindingKey) == 0 {
				return false, fmt.Errorf("missing blinding key to verify a confidential receive output")
			}
			unblinded, err := confidential.UnblindOutputWithKey(out, blindingKey)
			if err != nil {
				// A blinding key that cannot open this output just means it is not
				// the proposer's receive output; keep scanning.
				continue
			}
			// UnblindOutputResult.Asset is the 32-byte (no-prefix) asset; its
			// display id is the reverse-hex of those bytes.
			if unblinded.Value == amount &&
				elementsutil.TxIDFromBytes(unblinded.Asset) == asset {
				return true, nil
			}
			continue
		}
		// Explicit output: out.Asset carries the 0x01 prefix, out.Value is the
		// 9-byte explicit value.
		outValue, err := elementsutil.ValueFromBytes(out.Value)
		if err != nil {
			continue
		}
		if outValue == amount && elementsutil.AssetHashFromBytes(out.Asset) == asset {
			return true, nil
		}
	}
	return false, nil
}
