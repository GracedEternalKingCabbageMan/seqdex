package client

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	seqobv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqob/v1"
)

// mockBackend implements Backend without a chain, recording the confidentiality
// decisions so the blinded vs explicit branching can be asserted.
type mockBackend struct {
	lastConf      LegConfidentiality
	lastBlind     bool
	blindRecorded bool
}

func (m *mockBackend) ProposerBuildRequest(req ProposalReq, conf LegConfidentiality) (*seqdexv1.SwapRequest, error) {
	m.lastConf = conf
	blinder := zeroBlinder
	if conf.TakerInputsConfidential {
		blinder = strings.Repeat("a", 64) // a real, non-zero blinder
	}
	return &seqdexv1.SwapRequest{
		Id:      "req1",
		AssetP:  req.PayAsset,
		AmountP: req.PayAmount,
		AssetR:  req.RecvAsset,
		AmountR: req.RecvAmount,
		// A stub (non-PSET) string: psetHasConfidentialOutput returns false on it,
		// so only the real signals (input blinders / maker config) drive `blind`.
		Transaction: "STUB-PSET",
		UnblindedInputs: []*seqdexv1.UnblindedInput{{
			Index: 0, Asset: req.PayAsset, Amount: req.PayAmount,
			AssetBlinder: blinder, AmountBlinder: blinder,
		}},
	}, nil
}

func (m *mockBackend) ResponderComplete(req *seqdexv1.SwapRequest, blind bool) (*seqdexv1.SwapAccept, error) {
	m.lastBlind = blind
	m.blindRecorded = true
	return &seqdexv1.SwapAccept{
		Id:              "acc1",
		RequestId:       req.GetId(),
		Transaction:     req.GetTransaction() + "|maker-signed",
		UnblindedInputs: req.GetUnblindedInputs(),
	}, nil
}

func (m *mockBackend) ProposerFinalize(acc *seqdexv1.SwapAccept) (*seqdexv1.SwapComplete, string, error) {
	return &seqdexv1.SwapComplete{Id: "cmp1", AcceptId: acc.GetId(), Transaction: acc.GetTransaction() + "|taker-signed"}, "txid-" + acc.GetId(), nil
}

func offerWithBlinding(makerBlindingPub string) *seqobv1.Offer {
	return &seqobv1.Offer{
		OfferId:      "aaaa",
		Pair:         &seqobv1.AssetPair{BaseAsset: "gold", QuoteAsset: "usdx"},
		TradeDir:     seqobv1.TradeDir_TRADE_DIR_SELL,
		BaseAmount:   100,
		OfferAmount:  100,
		OfferAsset:   "gold",
		WantAmount:   45,
		WantAsset:    "usdx",
		AllowPartial: true,
		Settlement:   &seqobv1.Offer_SameChain{SameChain: &seqobv1.SameChainTerms{MakerRecvAddress: "addr", MakerBlindingPub: makerBlindingPub}},
	}
}

// runLift drives the full encrypted handshake through two LiveWallets over a
// mock backend, returning the maker's recorded blind decision and the txid.
func runLift(t *testing.T, takerLW, makerLW *LiveWallet, o *seqobv1.Offer, makerMock *mockBackend) (blind bool, txid string) {
	t.Helper()
	tk, _ := btcec.NewPrivateKey()
	mk, _ := btcec.NewPrivateKey()
	tc, _ := NewCrypter(tk, mk.PubKey())
	mc, _ := NewCrypter(mk, tk.PubKey())

	taker := &Taker{Wallet: takerLW}
	maker := &Maker{Wallet: makerLW}

	sealedReq, _, err := taker.Propose(o, 50, "gold", tc)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	sealedAcc, err := maker.HandleRequest(sealedReq, mc)
	if err != nil {
		t.Fatalf("maker handle: %v", err)
	}
	_, txid, err = taker.Finalize(sealedAcc, tc)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return makerMock.lastBlind, txid
}

func TestBlindedSwapSettles(t *testing.T) {
	takerMock := &mockBackend{}
	makerMock := &mockBackend{}
	takerLW := &LiveWallet{Backend: takerMock, TakerInputsConfidential: true, TakerRecvConfidential: true}
	makerLW := &LiveWallet{Backend: makerMock, MakerOutputsConfidential: true}
	o := offerWithBlinding("02blindingpubkey") // maker wants a confidential receive

	blind, txid := runLift(t, takerLW, makerLW, o, makerMock)
	if !blind {
		t.Fatalf("confidential swap must blind")
	}
	if txid == "" {
		t.Fatalf("expected a txid")
	}
	// The taker resolved the legs it can know (its own posture + the maker's
	// published receive intent). The maker's own input confidentiality is the
	// maker's concern, decided in ResponderComplete (asserted via blind above).
	c := takerMock.lastConf
	if !c.NeedsBlinding() || !c.TakerInputsConfidential || !c.TakerRecvConfidential || !c.MakerRecvConfidential {
		t.Fatalf("expected the taker's known legs to be confidential, got %+v", c)
	}
}

func TestExplicitSwapSettles(t *testing.T) {
	takerMock := &mockBackend{}
	makerMock := &mockBackend{}
	takerLW := &LiveWallet{Backend: takerMock} // explicit inputs + explicit receive
	makerLW := &LiveWallet{Backend: makerMock}  // explicit outputs
	o := offerWithBlinding("")                   // no maker blinding key => explicit receive

	blind, txid := runLift(t, takerLW, makerLW, o, makerMock)
	if blind {
		t.Fatalf("fully explicit swap must NOT blind")
	}
	if txid == "" {
		t.Fatalf("expected a txid")
	}
	if takerMock.lastConf.Mode() != "explicit" {
		t.Fatalf("expected explicit mode, got %s", takerMock.lastConf.Mode())
	}
	if !makerMock.blindRecorded {
		t.Fatalf("maker should have been invoked")
	}
}

func TestMixedSwapSettlesAndBlinds(t *testing.T) {
	// Taker funds with confidential UTXOs; maker keeps explicit outputs. Because a
	// confidential input is present the swap MUST still be blinded.
	takerMock := &mockBackend{}
	makerMock := &mockBackend{}
	takerLW := &LiveWallet{Backend: takerMock, TakerInputsConfidential: true}
	makerLW := &LiveWallet{Backend: makerMock} // explicit maker
	o := offerWithBlinding("")

	blind, txid := runLift(t, takerLW, makerLW, o, makerMock)
	if !blind {
		t.Fatalf("a confidential taker input must force blinding even with an explicit maker")
	}
	if txid == "" {
		t.Fatalf("expected a txid")
	}
	if takerMock.lastConf.Mode() != "mixed" {
		t.Fatalf("expected mixed mode, got %s (%+v)", takerMock.lastConf.Mode(), takerMock.lastConf)
	}
}

func TestLegConfidentialityModes(t *testing.T) {
	explicit := LegConfidentiality{}
	if explicit.NeedsBlinding() || explicit.Mode() != "explicit" {
		t.Fatalf("explicit: %v %s", explicit.NeedsBlinding(), explicit.Mode())
	}
	full := LegConfidentiality{true, true, true, true}
	if !full.NeedsBlinding() || full.Mode() != "confidential" {
		t.Fatalf("confidential: %v %s", full.NeedsBlinding(), full.Mode())
	}
	mixed := LegConfidentiality{TakerInputsConfidential: true}
	if !mixed.NeedsBlinding() || mixed.Mode() != "mixed" {
		t.Fatalf("mixed: %v %s", mixed.NeedsBlinding(), mixed.Mode())
	}
}
