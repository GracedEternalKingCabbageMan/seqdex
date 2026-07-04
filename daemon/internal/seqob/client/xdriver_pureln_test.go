package client

// xdriver_pureln_test.go exercises the pure-LN BUY handshake fully in-process:
// both drivers run concurrently over channels standing in for the relay courier,
// against fake ops that mimic the pure-LN semantics — the taker mints an asset
// invoice on P and locks BTC into the maker's hold; the maker "pays" the asset
// invoice (learning P) and settles the hold, revealing P back to the taker. No
// RPC, no chains, no LN node: this pins the handshake protocol. The settlement
// engine itself is proven live by pkg/xchain's pure-LN M2/M3 tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"
)

// plnState is shared by the maker + taker fakes, like the real LN nodes are: the
// asset invoice's preimage lives here (the maker learns it by "paying"), and the
// BTC hold flips held->settled as the two sides dance.
type plnState struct {
	mu          sync.Mutex
	secret      []byte // P, recorded by the taker when it mints the asset invoice
	invoiceMsat uint64
	holdHash    []byte // H the maker registered its BTC hold on
	holdAmtMsat uint64
	btcHeld     bool   // taker locked the BTC into the hold (PayHold)
	settled     bool   // maker paid the asset + settled the hold (Fulfill)
	revealed    []byte // P, released to the taker when the hold settles
}

// fakePlnMakerOps implements PlnMakerOps.
type fakePlnMakerOps struct{ st *plnState }

func (o *fakePlnMakerOps) HoldNodeID() (string, error) {
	return "02" + hex.EncodeToString(make([]byte, 32)), nil // stable fake node id
}
func (o *fakePlnMakerOps) RegisterHold(h []byte, btcAmtMsat uint64) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.holdHash = append([]byte(nil), h...)
	o.st.holdAmtMsat = btcAmtMsat
	return nil
}
func (o *fakePlnMakerOps) Fulfill(h []byte, assetInvoice string, assetAmtMsat uint64, holdTimeout time.Duration) ([]byte, error) {
	o.st.mu.Lock()
	// The hold H must match the taker's secret (as the shared payment_hash would).
	want := sha256.Sum256(o.st.secret)
	if hex.EncodeToString(h) != hex.EncodeToString(want[:]) {
		o.st.mu.Unlock()
		return nil, errPlnBadHash
	}
	if assetAmtMsat != 0 && assetAmtMsat != o.st.invoiceMsat {
		o.st.mu.Unlock()
		return nil, errPlnBadAmount
	}
	o.st.mu.Unlock()

	// Wait for the taker to lock the BTC into the hold, then "pay the asset
	// invoice" (revealing P) and settle the hold.
	deadline := time.Now().Add(holdTimeout)
	for {
		o.st.mu.Lock()
		held := o.st.btcHeld
		o.st.mu.Unlock()
		if held {
			break
		}
		if time.Now().After(deadline) {
			return nil, errPlnTimeout
		}
		time.Sleep(5 * time.Millisecond)
	}
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.settled = true
	o.st.revealed = append([]byte(nil), o.st.secret...) // paying the asset invoice revealed P
	return append([]byte(nil), o.st.secret...), nil
}

// fakePlnTakerOps implements PlnTakerOps.
type fakePlnTakerOps struct{ st *plnState }

func (o *fakePlnTakerOps) PrepareInvoice(p []byte, invoiceAmtMsat uint64) (string, []byte, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.secret = append([]byte(nil), p...)
	o.st.invoiceMsat = invoiceAmtMsat
	h := sha256.Sum256(p)
	return "ln-fake-" + hex.EncodeToString(h[:4]), h[:], nil
}
func (o *fakePlnTakerOps) PayHold(h []byte, makerBtcNodeID string, btcAmtMsat uint64, finalCltv uint32, paymentSecret []byte) ([]byte, error) {
	// Lock the BTC into the maker's hold, then block until the maker settles.
	o.st.mu.Lock()
	o.st.btcHeld = true
	o.st.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for {
		o.st.mu.Lock()
		settled, revealed := o.st.settled, o.st.revealed
		o.st.mu.Unlock()
		if settled {
			return append([]byte(nil), revealed...), nil
		}
		if time.Now().After(deadline) {
			return nil, errPlnTimeout
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var (
	errPlnBadHash   = fakeErr("fake pln: H mismatch")
	errPlnBadAmount = fakeErr("fake pln: asset amount mismatch")
	errPlnTimeout   = fakeErr("fake pln: counterparty did not act in time")
)

func TestPureLNBuyHandshake(t *testing.T) {
	st := &plnState{}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetMsat = uint64(1_000_000_000) // asset the maker pays
	const btcMsat = uint64(2_000_000)       // BTC the taker pays

	var makerRes *MakerPlnResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerPureLNBuy(MakerPlnParams{
			NewMakerOps: func() PlnMakerOps { return &fakePlnMakerOps{st: st} },
			Crypter:     mc,
			SeqAmtMsat:  assetMsat,
			BtcAmtMsat:  btcMsat,
			HoldTimeout: 3 * time.Second,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerPureLNBuy(TakerPlnParams{
		Ops:        &fakePlnTakerOps{st: st},
		Crypter:    tc,
		SeqAmtMsat: assetMsat,
		BtcAmtMsat: btcMsat,
		Timing:     XcTiming{TermsWait: 2 * time.Second, SeqLockWait: 3 * time.Second},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if !makerRes.Settled {
		t.Fatalf("maker not settled: %+v", makerRes)
	}
	// Both sides converged on the same P, and H = SHA256(P).
	if hex.EncodeToString(makerRes.Preimage) != hex.EncodeToString(takerRes.Preimage) {
		t.Fatalf("preimage mismatch: maker %x taker %x", makerRes.Preimage, takerRes.Preimage)
	}
	want := sha256.Sum256(takerRes.Preimage)
	if hex.EncodeToString(takerRes.HashH) != hex.EncodeToString(want[:]) {
		t.Fatalf("H != SHA256(P): H=%x", takerRes.HashH)
	}
}

// TestPureLNBuyTakerRejectsBadAmount: the taker refuses terms whose BTC amount
// does not match the signed offer, before minting an invoice or locking anything.
func TestPureLNBuyTakerRejectsBadAmount(t *testing.T) {
	st := &plnState{}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetMsat = uint64(1_000_000_000)
	const btcMsat = uint64(2_000_000)

	go func() {
		_, _ = RunMakerPureLNBuy(MakerPlnParams{
			NewMakerOps: func() PlnMakerOps { return &fakePlnMakerOps{st: st} },
			Crypter:     mc,
			SeqAmtMsat:  assetMsat,
			BtcAmtMsat:  btcMsat + 999, // != taker's expectation
			HoldTimeout: 2 * time.Second,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 2 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	_, err := RunTakerPureLNBuy(TakerPlnParams{
		Ops:        &fakePlnTakerOps{st: st},
		Crypter:    tc,
		SeqAmtMsat: assetMsat,
		BtcAmtMsat: btcMsat,
		Timing:     XcTiming{TermsWait: 2 * time.Second, SeqLockWait: 2 * time.Second},
	}, net.takerSend, net.takerRecv)
	if err == nil {
		t.Fatal("taker must reject a maker quoting the wrong BTC amount")
	}
}

// TestPureLNSellHandshake: the mirror direction. The taker sells the asset for
// BTC — it receives BTC (mints a BTC invoice on P) and pays the asset into the
// maker's ASSET hold; the maker pays the BTC invoice (learns P) and settles. The
// same direction-agnostic fake ops serve both directions; only which amount is
// the hold vs the invoice flips, which the driver derives from the direction.
func TestPureLNSellHandshake(t *testing.T) {
	st := &plnState{}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetMsat = uint64(1_000_000_000) // asset the taker pays into the hold
	const btcMsat = uint64(2_000_000)       // BTC the maker pays out

	var makerRes *MakerPlnResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerPureLNSell(MakerPlnParams{
			NewMakerOps: func() PlnMakerOps { return &fakePlnMakerOps{st: st} },
			Crypter:     mc,
			SeqAmtMsat:  assetMsat,
			BtcAmtMsat:  btcMsat,
			HoldTimeout: 3 * time.Second,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerPureLNSell(TakerPlnParams{
		Ops:        &fakePlnTakerOps{st: st},
		Crypter:    tc,
		SeqAmtMsat: assetMsat,
		BtcAmtMsat: btcMsat,
		Timing:     XcTiming{TermsWait: 2 * time.Second, SeqLockWait: 3 * time.Second},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if !makerRes.Settled {
		t.Fatalf("maker not settled: %+v", makerRes)
	}
	if hex.EncodeToString(makerRes.Preimage) != hex.EncodeToString(takerRes.Preimage) {
		t.Fatalf("preimage mismatch: maker %x taker %x", makerRes.Preimage, takerRes.Preimage)
	}
	// The maker paid the BTC invoice, so the recorded invoice amount is the BTC side.
	if st.invoiceMsat != btcMsat {
		t.Fatalf("sell: taker should have minted a BTC invoice for %d, got %d", btcMsat, st.invoiceMsat)
	}
}
