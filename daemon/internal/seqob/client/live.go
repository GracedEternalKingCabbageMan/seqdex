package client

import (
	"errors"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// LiveWallet is the production Wallet. It is a THIN decision layer: it computes
// the proposal legs and the blinded-vs-explicit choice, then delegates every
// chain operation to a Backend (realbackend.go wires that to the proven seqdex
// settlement; tests use a mock Backend). No settlement logic lives here.
//
// The confidentiality flags describe THIS wallet's posture (Sequentia
// confidential txs are opt-in); they are not assumed. As taker it declares
// whether it funds with confidential UTXOs and receives confidentially; as maker
// it declares whether its own outputs are confidential. The maker's per-swap
// blind decision additionally inspects the proposer's half (real input blinders
// or a confidential output), so a confidential taker is always honored.
type LiveWallet struct {
	Backend Backend

	TakerInputsConfidential  bool
	TakerRecvConfidential    bool
	MakerOutputsConfidential bool
}

var errNotWired = errors.New("live wallet backend not wired (Phase-1 tests use StubWallet / a mock Backend)")

// ProposerBuildRequest (taker) computes the legs and confidentiality, then builds
// the SwapRequest via the backend.
func (w *LiveWallet) ProposerBuildRequest(o *seqobv1.Offer, takeBase uint64, takerFeeAsset string) (*seqdexv1.SwapRequest, error) {
	if w.Backend == nil {
		return nil, errNotWired
	}
	recv, pay, err := proRata(o, takeBase)
	if err != nil {
		return nil, err
	}
	conf := LegConfidentiality{
		TakerInputsConfidential: w.TakerInputsConfidential,
		TakerRecvConfidential:   w.TakerRecvConfidential,
		// The maker asked to receive confidentially iff it published a blinding key.
		MakerRecvConfidential: o.GetSameChain().GetMakerBlindingPub() != "",
	}
	return w.Backend.ProposerBuildRequest(ProposalReq{
		Offer:         o,
		TakeBase:      takeBase,
		TakerFeeAsset: takerFeeAsset,
		PayAsset:      o.GetWantAsset(),
		PayAmount:     pay,
		RecvAsset:     o.GetOfferAsset(),
		RecvAmount:    recv,
	}, conf)
}

// ResponderComplete (maker) decides whether the swap must be blinded, then runs
// the responder via the backend. blind is true if the proposer revealed any
// confidential input, or its half has a confidential output, or this maker's own
// outputs are confidential; otherwise the swap is finalized EXPLICIT.
func (w *LiveWallet) ResponderComplete(req *seqdexv1.SwapRequest) (*seqdexv1.SwapAccept, error) {
	if w.Backend == nil {
		return nil, errNotWired
	}
	blind := requestIsConfidential(req) ||
		psetHasConfidentialOutput(req.GetTransaction()) ||
		w.MakerOutputsConfidential
	return w.Backend.ResponderComplete(req, blind)
}

// ProposerFinalize (taker) delegates sign+validate+broadcast to the backend.
func (w *LiveWallet) ProposerFinalize(acc *seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error) {
	if w.Backend == nil {
		return nil, "", errNotWired
	}
	return w.Backend.ProposerFinalize(acc)
}
