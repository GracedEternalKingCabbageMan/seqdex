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

// --- PARTIAL FILLS (both submarine directions) --------------------------------

// TestSubmarineNormalPartialFill pins the protocol gap: the taker never told the
// maker a size, so XcSubTermsRequest went out with NO FIELDS AT ALL and the maker
// locked the WHOLE offer on every lift. A taker wanting a tenth of an offer had
// to take all of it or nothing, while the cross rail had been doing real partials
// for some time.
//
// XcMsg already carried SeqAmount — the cross path uses exactly that field — so
// this is populating an existing field, not a wire change.
func TestSubmarineNormalPartialFill(t *testing.T) {
	st := &subState{seqTip: 5000}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}

	const whole = uint64(100_000_000_000)
	const wholeMsat = uint64(1_000_000)
	const take = whole / 4         // a quarter of the offer
	const wantMsat = wholeMsat / 4 // its proportional price

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
			SeqAmount:   whole,
			InvoiceMsat: wholeMsat,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubmarineNormal(TakerSubmarineParams{
		Ops:               &fakeSubTakerOps{st: st},
		Crypter:           tc,
		Secret:            secret,
		SeqRefundKey:      mustKey(t),
		ExpectAsset:       "asset-hex",
		ExpectSeqAmount:   whole,
		ExpectInvoiceMsat: wholeMsat,
		TakeSeqAmount:     take,
		Timing:            XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 3 * time.Second},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if takerRes.FilledSeq != take {
		t.Fatalf("taker funded %d atoms, want the %d slice", takerRes.FilledSeq, take)
	}
	if takerRes.FilledMsat != wantMsat || takerRes.PaidMsat != wantMsat {
		t.Fatalf("taker priced the slice at %d/%d msat, want %d", takerRes.FilledMsat, takerRes.PaidMsat, wantMsat)
	}
	if makerRes.FilledSeq != take {
		t.Fatalf("maker locked %d atoms, want the %d slice (it used to lock the whole offer)", makerRes.FilledSeq, take)
	}
	if makerRes.RemainderSeq != whole-take {
		t.Fatalf("remainder %d, want %d — the caller re-rests this", makerRes.RemainderSeq, whole-take)
	}
	if !takerRes.Settled || !makerRes.Settled {
		t.Fatalf("partial did not settle: taker=%+v maker=%+v", takerRes, makerRes)
	}
}

// A whole-offer lift is unchanged: TakeSeqAmount 0 means the whole thing, so an
// older taker that sends an empty terms request still works.
func TestSubmarineNormalWholeLiftIsUnchanged(t *testing.T) {
	st := &subState{seqTip: 5000}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	const whole = uint64(100_000_000_000)
	const wholeMsat = uint64(1_000_000)

	var makerRes *MakerSubmarineResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, _ = RunMakerSubmarineNormal(MakerSubmarineParams{
			NewMakerOps: func(hashH []byte) SubMakerOps { return &fakeSubMakerOps{st: st} },
			Crypter:     mc,
			SeqTip:      func() (int64, error) { return st.seqTip, nil },
			AssetHex:    "asset-hex",
			SeqAmount:   whole,
			InvoiceMsat: wholeMsat,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubmarineNormal(TakerSubmarineParams{
		Ops:               &fakeSubTakerOps{st: st},
		Crypter:           tc,
		Secret:            secret,
		SeqRefundKey:      mustKey(t),
		ExpectAsset:       "asset-hex",
		ExpectSeqAmount:   whole,
		ExpectInvoiceMsat: wholeMsat,
		// TakeSeqAmount deliberately unset.
		Timing: XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 3 * time.Second},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if takerRes.FilledSeq != whole || takerRes.PaidMsat != wholeMsat {
		t.Fatalf("whole lift changed shape: %+v", takerRes)
	}
	if makerRes.RemainderSeq != 0 {
		t.Fatalf("a whole lift must leave no remainder, got %d", makerRes.RemainderSeq)
	}
}

// An over-ask must be refused rather than silently clamped: a taker asking for
// more than the offer holds is a mismatch, and the maker has nothing to lock it
// against. (Genuine partials pass; only strictly-greater is rejected.)
func TestSubmarineNormalRejectsOverAsk(t *testing.T) {
	st := &subState{seqTip: 5000}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	const whole = uint64(100_000_000_000)

	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, makerErr = RunMakerSubmarineNormal(MakerSubmarineParams{
			NewMakerOps: func(hashH []byte) SubMakerOps { return &fakeSubMakerOps{st: st} },
			Crypter:     mc,
			SeqTip:      func() (int64, error) { return st.seqTip, nil },
			AssetHex:    "asset-hex",
			SeqAmount:   whole,
			InvoiceMsat: 1_000_000,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: time.Second},
		}, net.toMaker, net.makerSend)
	}()

	// Ask the maker directly for more than the offer holds (the taker's own params
	// clamp, so this drives the wire message the maker must defend against).
	_ = sendXc(&XcMsg{Type: XcSubTermsRequest, SeqAmount: whole + 1}, tc, net.takerSend)
	wg.Wait()

	if makerErr == nil {
		t.Fatal("maker accepted a slice larger than its own offer")
	}
	_ = secret
	_ = net
}
