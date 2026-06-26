package client

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/thanhpk/randstr"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// StubWallet is an IN-MEMORY, NON-CHAIN Wallet for Phase-1 testing and the CLI
// demo. It produces structurally-correct SwapRequest/SwapAccept/SwapComplete
// messages (with placeholder PSET strings) so the lift -> courier -> settle
// MESSAGE FLOW can be exercised without a live Ocean/LWK wallet. It performs NO
// blinding, signing, or broadcasting. The production path is LiveWallet (live.go).
type StubWallet struct {
	// Name disambiguates the placeholder PSET/txid (e.g. "maker" / "taker").
	Name string
}

// proRata returns offer/want amounts scaled to takeBase base atoms, preserving
// the authoritative want/offer ratio (want rounded up so the maker is never
// short-changed).
func proRata(o *seqobv1.Offer, takeBase uint64) (recvOfferAsset uint64, payWantAsset uint64, err error) {
	base := o.GetBaseAmount()
	if base == 0 || takeBase == 0 || takeBase > base {
		return 0, 0, fmt.Errorf("invalid take amount %d (base %d)", takeBase, base)
	}
	// recv = offer_amount * takeBase / base  (floor)
	recvOfferAsset = o.GetOfferAmount() * takeBase / base
	// pay = ceil(want_amount * takeBase / base)
	num := o.GetWantAmount() * takeBase
	payWantAsset = num / base
	if num%base != 0 {
		payWantAsset++
	}
	if recvOfferAsset == 0 || payWantAsset == 0 {
		return 0, 0, fmt.Errorf("take amount too small for a non-zero fill")
	}
	return recvOfferAsset, payWantAsset, nil
}

// ProposerBuildRequest builds the taker's SwapRequest. The taker is the proposer:
// it PAYS want_asset (AssetP) and RECEIVES offer_asset (AssetR).
func (w *StubWallet) ProposerBuildRequest(o *seqobv1.Offer, takeBase uint64, takerFeeAsset string) (*seqdexv1.SwapRequest, error) {
	recv, pay, err := proRata(o, takeBase)
	if err != nil {
		return nil, err
	}
	return &seqdexv1.SwapRequest{
		Id:      randstr.Hex(8),
		AssetP:  o.GetWantAsset(),
		AmountP: pay,
		AssetR:  o.GetOfferAsset(),
		AmountR: recv,
		Transaction: fmt.Sprintf("STUB-PSETV2:%s:proposer:take=%d:fee=%s",
			w.Name, takeBase, takerFeeAsset),
		UnblindedInputs: []*seqdexv1.UnblindedInput{{
			Index:         0,
			Asset:         o.GetWantAsset(),
			Amount:        pay,
			AssetBlinder:  "stub-asset-blinder",
			AmountBlinder: "stub-amount-blinder",
		}},
	}, nil
}

// ResponderComplete is the maker side. The production impl runs the real
// CompleteSwap; the stub just echoes a "maker-signed" transaction.
func (w *StubWallet) ResponderComplete(req *seqdexv1.SwapRequest) (*seqdexv1.SwapAccept, error) {
	if req == nil {
		return nil, fmt.Errorf("nil swap request")
	}
	return &seqdexv1.SwapAccept{
		Id:              randstr.Hex(8),
		RequestId:       req.GetId(),
		Transaction:     req.GetTransaction() + "|maker-signed",
		UnblindedInputs: req.GetUnblindedInputs(),
	}, nil
}

// ProposerFinalize is the taker side. The production impl signs+broadcasts; the
// stub derives a deterministic placeholder txid.
func (w *StubWallet) ProposerFinalize(acc *seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error) {
	if acc == nil {
		return nil, "", fmt.Errorf("nil swap accept")
	}
	finalTx := acc.GetTransaction() + "|taker-signed"
	sum := sha256.Sum256([]byte(finalTx))
	txid := hex.EncodeToString(sum[:])
	return &seqdexv1.SwapComplete{
		Id:          randstr.Hex(8),
		AcceptId:    acc.GetId(),
		Transaction: finalTx,
	}, txid, nil
}
