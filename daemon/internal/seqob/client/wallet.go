package client

import (
	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// Wallet is the settlement contract the lift handshake drives. It is expressed in
// terms of the EXISTING seqdex swap wire messages (seqdexv1.SwapRequest /
// SwapAccept / SwapComplete) so the production implementation is a thin adapter
// over the proven primitives — it does NOT reimplement settlement:
//
//   - ProposerBuildRequest  -> pkg/trade.NewSwapTx + pkg/swap.Request
//   - ResponderComplete     -> wallet.Service.CompleteSwap + pkg/swap.Accept
//   - ProposerFinalize      -> pkg/trade.Wallet.Sign + pkg/swap.Complete + broadcast
//
// See live.go for the concrete reuse skeleton, and stubwallet.go for the
// in-memory implementation used by the Phase-1 tests and CLI.
type Wallet interface {
	// ProposerBuildRequest (taker side): build the proposer PSET that pays the
	// maker its want_asset and receives the offer_asset, for takeBase base atoms
	// (partial fills allowed). takerFeeAsset selects the open-market fee asset.
	ProposerBuildRequest(o *seqobv1.Offer, takeBase uint64, takerFeeAsset string) (*seqdexv1.SwapRequest, error)

	// ResponderComplete (maker side): run the existing CompleteSwap responder
	// path against the taker's SwapRequest -> SwapAccept.
	ResponderComplete(req *seqdexv1.SwapRequest) (*seqdexv1.SwapAccept, error)

	// ProposerFinalize (taker side): blind+sign own inputs, validate, broadcast ->
	// SwapComplete plus the broadcast txid.
	ProposerFinalize(acc *seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error)
}
