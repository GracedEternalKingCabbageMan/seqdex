package client

// xdriver_submarine_reverse_test.go exercises the REVERSE (maker-secret) submarine
// handshake fully in-process over an in-memory courier with fake ops that mimic
// the semantics: the maker generates P (recorded via the NewMakerOps closure), the
// taker's PayInvoice reveals it, and the maker's AwaitReversePayment observes the
// payment. No RPC / chains / LN node; the settlement engine is proven live by
// pkg/xchain's TestSubmarineReverseMakerSecretLive.

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type revState struct {
	mu          sync.Mutex
	seqTip      int64
	secret      []byte // P, recorded when the maker builds its ops
	invoicePaid bool   // set when the taker pays; the maker awaits it
}

type fakeRevMakerOps struct{ st *revState }

func (o *fakeRevMakerOps) OfferReverseMakerSecret(p xchain.ReverseMakerSecretParams) (*xchain.ReverseMakerSecretOffer, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	hArr := sha256.Sum256(o.st.secret)
	h := hArr[:]
	return &xchain.ReverseMakerSecretOffer{
		Bolt11: "lnbc-rev-" + hex.EncodeToString(h[:4]),
		HashH:  h,
		SeqLeg: &xchain.LegLock{
			Script:   fakeScript("seq", h, p.TakerSeqClaimPub, p.MakerSeqRefundPub, p.SeqLocktime),
			Funded:   &xchain.FundedHTLC{TxID: "rev-htlc", Vout: 1, Amount: coinsToAtoms(p.SeqAmountCoins), AssetID: p.SeqAssetLabel},
			Locktime: p.SeqLocktime,
		},
		SeqBlock: "rev-block",
		Label:    p.InvoiceLabel,
	}, nil
}
func (o *fakeRevMakerOps) AwaitReversePayment(label string, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for {
		o.st.mu.Lock()
		paid := o.st.invoicePaid
		o.st.mu.Unlock()
		if paid {
			return 1_000_000, nil
		}
		if time.Now().After(deadline) {
			return 0, errFakeTimeout
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (o *fakeRevMakerOps) RefundReverseSEQ(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return "rev-refund", nil
}

type fakeRevTakerOps struct{ st *revState }

func (o *fakeRevTakerOps) VerifySEQLeg(hashH, claimPub, refundPub, providedScript []byte, seqLocktime uint32,
	txid string, vout uint32, amount uint64, assetID string, minConf int) (*xchain.VerifiedSEQLeg, error) {
	return &xchain.VerifiedSEQLeg{Leg: &xchain.LegLock{
		Funded:   &xchain.FundedHTLC{TxID: txid, Vout: vout, Amount: amount, AssetID: assetID},
		Locktime: seqLocktime,
	}}, nil
}
func (o *fakeRevTakerOps) VerifySeqAnchorBuried(seqBlockHash string, minAnchorDepth int64) (*xchain.SubAnchorEvidence, error) {
	return &xchain.SubAnchorEvidence{OK: true, AnchorDepth: minAnchorDepth, MinAnchorDepth: minAnchorDepth}, nil
}
func (o *fakeRevTakerOps) PayInvoice(bolt11 string, wantHash []byte, amountMsat uint64, _ uint32) ([]byte, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.invoicePaid = true
	return append([]byte(nil), o.st.secret...), nil // paying reveals P
}
func (o *fakeRevTakerOps) InjectSecret(secret []byte) error { return nil }
func (o *fakeRevTakerOps) ClaimSEQLeg(leg *xchain.LegLock, key *xchain.Key, fee uint64) (string, error) {
	return "rev-claim-tx", nil
}

func TestSubmarineReverseHandshake(t *testing.T) {
	// The maker records the P it generates via the NewMakerOps closure.
	st := &revState{seqTip: 5000}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const seqAmount = uint64(100_000_000)
	const invoiceMsat = uint64(1_000_000)

	var makerRes *MakerReverseSubmarineResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerReverseSubmarine(MakerReverseSubmarineParams{
			NewMakerOps: func(secret []byte) SubReverseMakerOps {
				st.mu.Lock()
				st.secret = append([]byte(nil), secret...)
				st.mu.Unlock()
				return &fakeRevMakerOps{st: st}
			},
			Crypter:     mc,
			SeqTip:      func() (int64, error) { return st.seqTip, nil },
			AssetHex:    "asset-hex",
			SeqAmount:   seqAmount,
			InvoiceMsat: invoiceMsat,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcConfWait: 3 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerReverseSubmarine(TakerReverseSubmarineParams{
		NewTakerOps:       func(hashH []byte) SubReverseTakerOps { return &fakeRevTakerOps{st: st} },
		Crypter:           tc,
		SeqClaimKey:       mustKey(t),
		ExpectAsset:       "asset-hex",
		ExpectSeqAmount:   seqAmount,
		ExpectInvoiceMsat: invoiceMsat,
		Timing:            XcTiming{SeqLockWait: 2 * time.Second, AnchorWait: 2 * time.Second},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if !takerRes.Settled || takerRes.SeqClaimTxid != "rev-claim-tx" {
		t.Fatalf("taker not settled: %+v", takerRes)
	}
	if !makerRes.Settled || makerRes.PaidMsat != invoiceMsat {
		t.Fatalf("maker not settled: %+v", makerRes)
	}
	// The P the taker learned by paying must equal the P the maker generated.
	st.mu.Lock()
	makerSecret := st.secret
	st.mu.Unlock()
	if hex.EncodeToString(takerRes.Preimage) != hex.EncodeToString(makerSecret) {
		t.Fatalf("taker learned preimage %x != maker's P %x", takerRes.Preimage, makerSecret)
	}
}
