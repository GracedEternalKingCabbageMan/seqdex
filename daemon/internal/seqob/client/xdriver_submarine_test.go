package client

// xdriver_submarine_test.go exercises the NORMAL submarine handshake fully
// in-process: both drivers run concurrently over channels standing in for the
// relay courier, against fake ops that mimic the submarine semantics (minting an
// invoice on P, and the maker "learning" P by paying it). No RPC, no chains, no
// LN node — this pins the handshake protocol; the settlement engine itself is
// proven live by pkg/xchain's TestSubmarineRunNormalLive.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// subState is shared by the maker + taker fakes, like the real chains/LN node are.
type subState struct {
	mu          sync.Mutex
	seqTip      int64
	secret      []byte // P, recorded by the taker when it mints the invoice
	invoiceMsat uint64
	invoicePaid bool // set when the maker "pays" (RunNormal); the taker awaits it
	claimTxid   string
}

// fakeSubMakerOps implements SubMakerOps: it validates H, "pays" the invoice
// (revealing P from the shared state), and reports the asset claim.
type fakeSubMakerOps struct{ st *subState }

func (o *fakeSubMakerOps) RunNormal(p xchain.NormalParams, key *xchain.Key, fee uint64) (*xchain.NormalResult, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	// The maker was handed H; it must match the taker's secret (sanity, as the
	// real VerifySEQLeg + pay would enforce via the shared payment_hash).
	want := sha256.Sum256(o.st.secret)
	if hex.EncodeToString(p.HashH) != hex.EncodeToString(want[:]) {
		return nil, errFakeBadHash
	}
	if p.InvoiceMsat != 0 && p.InvoiceMsat != o.st.invoiceMsat {
		return nil, errFakeBadAmount
	}
	// Paying the BOLT11 reveals P and lets the maker claim the asset.
	o.st.invoicePaid = true
	o.st.claimTxid = "seq-claim-tx"
	return &xchain.NormalResult{Preimage: append([]byte(nil), o.st.secret...), SeqClaimTxID: "seq-claim-tx"}, nil
}

// fakeSubTakerOps implements SubTakerOps.
type fakeSubTakerOps struct{ st *subState }

func (o *fakeSubTakerOps) SeqTip() (int64, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	return o.st.seqTip, nil
}
func (o *fakeSubTakerOps) LockSEQLeg(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*xchain.LegLock, string, error) {
	return &xchain.LegLock{
		Script:   fakeScript("seq", nil, claimPub, refundPub, locktime),
		Funded:   &xchain.FundedHTLC{TxID: "seq-htlc", Vout: 1, Amount: coinsToAtoms(amountCoins), AssetID: assetLabel},
		Locktime: locktime,
	}, "seq-block-hash", nil
}
func (o *fakeSubTakerOps) MintInvoice(preimage []byte, amountMsat uint64, cltv uint32, label, desc string) (string, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.secret = append([]byte(nil), preimage...)
	o.st.invoiceMsat = amountMsat
	h := sha256.Sum256(preimage)
	return "lnbc-fake-" + hex.EncodeToString(h[:4]), nil
}
func (o *fakeSubTakerOps) AwaitInvoicePaid(label string, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for {
		o.st.mu.Lock()
		paid, amt := o.st.invoicePaid, o.st.invoiceMsat
		o.st.mu.Unlock()
		if paid {
			return amt, nil
		}
		if time.Now().After(deadline) {
			return 0, errFakeTimeout
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (o *fakeSubTakerOps) RefundSEQLeg(leg *xchain.LegLock, key *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	return "seq-refund-tx", nil
}
func (o *fakeSubTakerOps) SeqBlockAnchorHeightOf(blockHash string) (int64, error) { return 1000, nil }

var (
	errFakeBadHash   = fakeErr("fake: H mismatch")
	errFakeBadAmount = fakeErr("fake: amount mismatch")
	errFakeTimeout   = fakeErr("fake: invoice not paid in time")
)

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func TestSubmarineNormalHandshake(t *testing.T) {
	st := &subState{seqTip: 5000}
	tc, mc := testCrypters(t)

	net := newFakeXcNet()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	takerRefund := mustKey(t)

	const seqAmount = uint64(100_000_000_000) // 1000 units
	const invoiceMsat = uint64(1_000_000)     // 1000 sat

	var makerRes *MakerSubmarineResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerSubmarineNormal(MakerSubmarineParams{
			NewMakerOps: func(hashH []byte) SubMakerOps { return &fakeSubMakerOps{st: st} },
			Crypter:     mc,
			SeqTip:      func() (int64, error) { return st.seqTip, nil },
			AssetHex:    "asset-hex",
			SeqAmount:   seqAmount,
			InvoiceMsat: invoiceMsat,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubmarineNormal(TakerSubmarineParams{
		Ops:               &fakeSubTakerOps{st: st},
		Crypter:           tc,
		Secret:            secret,
		SeqRefundKey:      takerRefund,
		ExpectAsset:       "asset-hex",
		ExpectSeqAmount:   seqAmount,
		ExpectInvoiceMsat: invoiceMsat,
		Timing:            XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 3 * time.Second},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if !takerRes.Settled || takerRes.PaidMsat != invoiceMsat {
		t.Fatalf("taker not settled: %+v", takerRes)
	}
	if !makerRes.Settled {
		t.Fatalf("maker not settled: %+v", makerRes)
	}
	if hex.EncodeToString(makerRes.Preimage) != hex.EncodeToString(secret) {
		t.Fatalf("maker recovered preimage %x != P %x", makerRes.Preimage, secret)
	}
	if makerRes.SeqClaimTxid != "seq-claim-tx" {
		t.Fatalf("unexpected claim txid %q", makerRes.SeqClaimTxid)
	}
}

// TestSubmarineNormalTakerRejectsBadAmount: the taker refuses terms whose
// seq_amount does not match the signed offer, before funding anything.
func TestSubmarineNormalTakerRejectsBadAmount(t *testing.T) {
	st := &subState{seqTip: 5000}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)

	// A maker that quotes the WRONG seq_amount.
	go func() {
		_, _ = RunMakerSubmarineNormal(MakerSubmarineParams{
			NewMakerOps: func(hashH []byte) SubMakerOps { return &fakeSubMakerOps{st: st} },
			Crypter:     mc,
			SeqTip:      func() (int64, error) { return st.seqTip, nil },
			AssetHex:    "asset-hex",
			SeqAmount:   999, // != taker's expectation
			InvoiceMsat: 1_000_000,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 2 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	_, err := RunTakerSubmarineNormal(TakerSubmarineParams{
		Ops:               &fakeSubTakerOps{st: st},
		Crypter:           tc,
		Secret:            secret,
		SeqRefundKey:      mustKey(t),
		ExpectAsset:       "asset-hex",
		ExpectSeqAmount:   100_000_000_000,
		ExpectInvoiceMsat: 1_000_000,
		Timing:            XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 2 * time.Second},
	}, net.takerSend, net.takerRecv)
	if err == nil {
		t.Fatal("taker must reject a maker quoting the wrong seq_amount")
	}
}
