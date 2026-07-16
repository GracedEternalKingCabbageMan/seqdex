package client

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/bits"

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
	// Both factors are asset atoms (up to ~2.1e15 for a 21M-supply 8-dp asset), so
	// factor*takeBase overflows uint64 for realistic sizes (e.g. 5e10 * 5e10 =
	// 2.5e21 > 1.8e19). Compute the 128-bit product and divide it down so the legs
	// stay exact. This is the authoritative price scaling; overflow here silently
	// garbled every non-trivial fill.
	// recv = floor(offer_amount * takeBase / base)
	recvOfferAsset, _ = mulDiv64(o.GetOfferAmount(), takeBase, base)
	// pay = ceil(want_amount * takeBase / base)
	q, rem := mulDiv64(o.GetWantAmount(), takeBase, base)
	payWantAsset = q
	if rem != 0 {
		payWantAsset++
	}
	if recvOfferAsset == 0 || payWantAsset == 0 {
		return 0, 0, fmt.Errorf("take amount too small for a non-zero fill")
	}
	return recvOfferAsset, payWantAsset, nil
}

// mulDiv64 returns floor(a*b/d) and the remainder, computing a*b in 128 bits so
// it never overflows uint64. The proRata callers guarantee takeBase <= base, so
// the true quotient is <= the (uint64) offer/want amount and bits.Div64 (which
// panics only when the quotient would exceed 64 bits) is always safe.
func mulDiv64(a, b, d uint64) (q, rem uint64) {
	hi, lo := bits.Mul64(a, b)
	return bits.Div64(hi, lo, d)
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
