package client

import (
	"github.com/vulpemventures/go-elements/psetv2"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// LegConfidentiality classifies which legs of a same-chain swap carry blinding
// material. Sequentia confidential transactions are OPT-IN: a swap may be fully
// confidential, fully explicit, or mixed, and settlement MUST work in all three.
// NeedsBlinding drives whether the responder runs BlindPset or finalizes explicit.
type LegConfidentiality struct {
	TakerInputsConfidential bool // taker funds with confidential UTXOs
	TakerRecvConfidential   bool // taker receives into a confidential address
	MakerInputsConfidential bool // maker funds with confidential UTXOs
	MakerRecvConfidential   bool // maker receives into a confidential address
}

// NeedsBlinding reports whether any leg is confidential (so the tx must be blinded).
func (l LegConfidentiality) NeedsBlinding() bool {
	return l.TakerInputsConfidential || l.TakerRecvConfidential ||
		l.MakerInputsConfidential || l.MakerRecvConfidential
}

// Mode returns "explicit", "confidential", or "mixed".
func (l LegConfidentiality) Mode() string {
	if !l.NeedsBlinding() {
		return "explicit"
	}
	if l.TakerInputsConfidential && l.TakerRecvConfidential &&
		l.MakerInputsConfidential && l.MakerRecvConfidential {
		return "confidential"
	}
	return "mixed"
}

// ProposalReq carries the resolved taker proposal legs. The taker is the proposer:
// it PAYS PayAsset (the maker's want_asset) and RECEIVES RecvAsset (the offer_asset).
type ProposalReq struct {
	Offer         *seqobv1.Offer
	TakeBase      uint64
	TakerFeeAsset string
	PayAsset      string
	PayAmount     uint64
	RecvAsset     string
	RecvAmount    uint64
}

// Backend is the chain-side settlement plumbing the LiveWallet drives. The real
// implementation (realbackend.go) wires these to pkg/trade.NewSwapTx +
// pkg/swap.{Request,Accept,Complete} + the Ocean/LWK wallet CompleteSwap; the
// mock implementation in tests records the calls so the blinded vs explicit paths
// can be asserted without a live node. Settlement logic is NOT reimplemented here.
type Backend interface {
	// ProposerBuildRequest selects the taker's UTXOs and builds the proposer
	// SwapRequest. conf says which legs are confidential (so the proposer's output
	// is blinded or explicit, and its inputs reveal real or zero blinders).
	ProposerBuildRequest(req ProposalReq, conf LegConfidentiality) (*seqdexv1.SwapRequest, error)
	// ResponderComplete runs the maker CompleteSwap responder: add own UTXOs,
	// change, the any-asset fee vout, then BlindPset IFF blind (else finalize
	// explicit), then sign the maker's own inputs.
	ResponderComplete(req *seqdexv1.SwapRequest, blind bool) (*seqdexv1.SwapAccept, error)
	// ProposerFinalize signs the taker's inputs, validates, and broadcasts.
	ProposerFinalize(acc *seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error)
}

// zeroBlinder is the 32-byte all-zero blinder an EXPLICIT (non-confidential) UTXO
// carries; a confidential UTXO reveals a real, non-zero blinder.
const zeroBlinder = "0000000000000000000000000000000000000000000000000000000000000000"

func hasRealBlinder(b string) bool { return b != "" && b != zeroBlinder }

// requestIsConfidential reports whether a SwapRequest reveals any confidential
// (real-blinder) input — the proposer signalling that its side is blinded.
func requestIsConfidential(req *seqdexv1.SwapRequest) bool {
	for _, in := range req.GetUnblindedInputs() {
		if hasRealBlinder(in.GetAssetBlinder()) || hasRealBlinder(in.GetAmountBlinder()) {
			return true
		}
	}
	return false
}

// psetHasConfidentialOutput parses a PSETv2 and reports whether any output is
// blinded (carries a blinding pubkey). The maker uses this as the authoritative
// check on the proposer's half: it catches a taker that funds with explicit
// inputs yet still wants a confidential receive output. On a non-PSET/stub
// string it returns false (the other signals decide).
func psetHasConfidentialOutput(psetB64 string) bool {
	ptx, err := psetv2.NewPsetFromBase64(psetB64)
	if err != nil {
		return false
	}
	for _, out := range ptx.Outputs {
		if len(out.BlindingPubkey) > 0 {
			return true
		}
	}
	return false
}
