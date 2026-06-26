package client

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
	"github.com/aejkcs50/seqdex/daemon/pkg/swap"
)

// LiveWallet is the production Wallet. It REUSES the proven seqdex same-chain
// settlement verbatim and only supplies the chain-specific PSET ops via function
// fields, so this file contains NO settlement logic of its own:
//
//   - the SwapRequest/SwapAccept/SwapComplete envelopes are built by the existing
//     pkg/swap.{Request,Accept,Complete} (imported and called below);
//   - BuildPSET is wired upstream to pkg/trade.NewSwapTx + the Ocean/LWK wallet's
//     SelectUtxos (daemon/pkg/trade/wallet.go, NewSwapTx);
//   - CompleteSwap is wired to wallet.Service.CompleteSwap
//     (daemon/internal/core/application/wallet/service.go:227-422), which already
//     does SelectUtxos, change, the any-asset fee vout (Principle 4), UpdatePset,
//     BlindPset with the taker's unblinded inputs, and SignPset of own inputs;
//   - SignProposerInputs / Broadcast wrap pkg/trade.Wallet.Sign and
//     BroadcastTransaction.
//
// PHASE-1 STATUS: the function fields are left for the deployment to wire to a
// live wallet; the Phase-1 tests and CLI use StubWallet instead. With nil fields
// every method returns errNotWired so the type still compiles and documents the
// exact reuse seams.
type LiveWallet struct {
	// BuildPSET builds the proposer (taker) PSETv2 paying the maker its want_asset
	// and receiving offer_asset for takeBase base atoms, returning the base64 PSET
	// and the taker's unblinded inputs plus the resolved pay/recv legs.
	BuildPSET func(o *seqobv1.Offer, takeBase uint64, feeAsset string) (psetB64 string, unblinded []swap.UnblindedInput, payAsset string, payAmt uint64, recvAsset string, recvAmt uint64, err error)

	// CompleteSwap runs the responder (maker) CompleteSwap against the taker's
	// request, returning the maker-signed PSET and the combined unblinded inputs.
	CompleteSwap func(req *seqdexv1.SwapRequest) (signedPSET string, combinedUnblinded []swap.UnblindedInput, err error)

	// SignProposerInputs signs the taker's own inputs on the maker-signed PSET.
	SignProposerInputs func(psetB64 string) (signedPSET string, err error)

	// Broadcast publishes the fully co-signed transaction and returns its txid.
	Broadcast func(signedPSET string) (txid string, err error)
}

var errNotWired = errors.New("live wallet op not wired (Phase-1 uses StubWallet)")

// ProposerBuildRequest builds the PSET (BuildPSET) then the SwapRequest envelope
// via pkg/swap.Request.
func (w *LiveWallet) ProposerBuildRequest(o *seqobv1.Offer, takeBase uint64, takerFeeAsset string) (*seqdexv1.SwapRequest, error) {
	if w.BuildPSET == nil {
		return nil, errNotWired
	}
	psetB64, unblinded, payAsset, payAmt, recvAsset, recvAmt, err := w.BuildPSET(o, takeBase, takerFeeAsset)
	if err != nil {
		return nil, err
	}
	reqBytes, err := swap.Request(swap.RequestOpts{
		AssetToSend:     payAsset,
		AmountToSend:    payAmt,
		AssetToReceive:  recvAsset,
		AmountToReceive: recvAmt,
		Transaction:     psetB64,
		UnblindedInputs: unblinded,
		FeeAsset:        takerFeeAsset,
	})
	if err != nil {
		return nil, fmt.Errorf("swap.Request: %w", err)
	}
	var req seqdexv1.SwapRequest
	if err := proto.Unmarshal(reqBytes, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ResponderComplete runs CompleteSwap then builds the SwapAccept via
// pkg/swap.Accept.
func (w *LiveWallet) ResponderComplete(req *seqdexv1.SwapRequest) (*seqdexv1.SwapAccept, error) {
	if w.CompleteSwap == nil {
		return nil, errNotWired
	}
	signedPSET, combined, err := w.CompleteSwap(req)
	if err != nil {
		return nil, err
	}
	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	_, accBytes, err := swap.Accept(swap.AcceptOpts{
		Message:         reqBytes,
		Transaction:     signedPSET,
		UnblindedInputs: combined,
	})
	if err != nil {
		return nil, fmt.Errorf("swap.Accept: %w", err)
	}
	var acc seqdexv1.SwapAccept
	if err := proto.Unmarshal(accBytes, &acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// ProposerFinalize signs the taker's inputs, validates via pkg/swap.Complete, and
// broadcasts.
func (w *LiveWallet) ProposerFinalize(acc *seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error) {
	if w.SignProposerInputs == nil || w.Broadcast == nil {
		return nil, "", errNotWired
	}
	signed, err := w.SignProposerInputs(acc.GetTransaction())
	if err != nil {
		return nil, "", err
	}
	accBytes, err := proto.Marshal(acc)
	if err != nil {
		return nil, "", err
	}
	_, completeBytes, err := swap.Complete(swap.CompleteOpts{
		Message:     accBytes,
		Transaction: signed,
	})
	if err != nil {
		return nil, "", fmt.Errorf("swap.Complete: %w", err)
	}
	var complete seqdexv1.SwapComplete
	if err := proto.Unmarshal(completeBytes, &complete); err != nil {
		return nil, "", err
	}
	txid, err := w.Broadcast(signed)
	if err != nil {
		return nil, "", err
	}
	return &complete, txid, nil
}
