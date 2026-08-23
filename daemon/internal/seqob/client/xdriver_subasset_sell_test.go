package client

// xdriver_subasset_sell_test.go exercises the SUB-ASSET SELL handshake fully in-process:
// a taker pays an asset OVER LIGHTNING and receives BTC ON-CHAIN. Both drivers run
// concurrently over channels standing in for the relay courier, against fakes that mimic
// the two legs — the maker funds a BTC HTLC + holds an asset invoice (settles with P);
// the taker verifies the HTLC, pays the invoice (learning P), and claims the BTC. No RPC,
// no chains, no LN node: this pins the handshake + preimage flow (mirror of the BUY test).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

type sellState struct {
	mu sync.Mutex

	secret   []byte // P, minted by the maker
	holdHash []byte // H the maker's hold invoice is on
	btcTxid  string
	btcAmt   uint64
	btcConfs int

	takerPaid    bool // taker's LN payment is in-flight + HELD at the maker
	makerSettled bool // maker settled the hold with P (revealing it to the taker)
	btcClaimedBy []byte
}

// --- maker fake ---

type fakeSellMakerOps struct{ st *sellState }

func (o *fakeSellMakerOps) BtcTip() (int64, error) { return 100, nil }
func (o *fakeSellMakerOps) BtcConfirmations(string) (int, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	return o.st.btcConfs, nil
}
func (o *fakeSellMakerOps) LockBTCLeg(takerClaimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.btcTxid = "sell-btc-htlc-cafe"
	o.st.btcAmt = coinsToAtoms(amountCoins)
	o.st.btcConfs = 1
	return &xchain.LegLock{Script: []byte{0x51}, Funded: &xchain.FundedHTLC{TxID: o.st.btcTxid, Vout: 0, Amount: o.st.btcAmt}, Locktime: locktime}, 0, nil
}
func (o *fakeSellMakerOps) AssetLNNodeID() (string, error) {
	return "02" + hex.EncodeToString(make([]byte, 32)), nil
}
func (o *fakeSellMakerOps) CreateAssetHold(h []byte, amtMsat uint64) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.holdHash = append([]byte(nil), h...)
	return nil
}
func (o *fakeSellMakerOps) WaitAssetHeld(h []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		o.st.mu.Lock()
		paid := o.st.takerPaid
		o.st.mu.Unlock()
		if paid {
			return nil
		}
		if time.Now().After(deadline) {
			return errSellTimeout
		}
		time.Sleep(3 * time.Millisecond)
	}
}
func (o *fakeSellMakerOps) SettleAssetHold(h, preimage []byte) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.makerSettled = true // reveals P to the taker's Pay
	return nil
}
func (o *fakeSellMakerOps) CancelAssetHold([]byte) error { return nil }
func (o *fakeSellMakerOps) RefundBTCLeg(*xchain.LegLock, *xchain.Key, uint32, uint64) (string, error) {
	return "sell-refund", nil
}

// --- taker fake ---

type fakeSellTakerOps struct{ st *sellState }

func (o *fakeSellTakerOps) BtcTip() (int64, error) { return 100, nil }
func (o *fakeSellTakerOps) VerifyBTCLeg(hashH, takerClaimPub, makerRefundPub, script []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	if hex.EncodeToString(hashH) != hex.EncodeToString(o.st.holdHash) || txid != o.st.btcTxid || amount != o.st.btcAmt {
		return nil, xchain.ErrBTCLegInvalid
	}
	if o.st.btcConfs < minConf {
		return nil, xchain.ErrBTCLegUnconfirmed
	}
	return &xchain.VerifiedBTCLeg{Leg: &xchain.LegLock{Script: script, Funded: &xchain.FundedHTLC{TxID: txid, Vout: vout, Amount: amount}, Locktime: btcLocktime}}, nil
}

// PayAsset marks the payment held, then blocks until the maker settles, returning P.
func (o *fakeSellTakerOps) PayAsset(makerNodeID string, wantHash []byte, amtMsat uint64) ([]byte, error) {
	o.st.mu.Lock()
	o.st.takerPaid = true
	o.st.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		o.st.mu.Lock()
		settled := o.st.makerSettled
		secret := o.st.secret
		o.st.mu.Unlock()
		if settled {
			return append([]byte(nil), secret...), nil
		}
		if time.Now().After(deadline) {
			return nil, errSellTimeout
		}
		time.Sleep(3 * time.Millisecond)
	}
}
func (o *fakeSellTakerOps) InjectSecret(preimage []byte) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	if hex.EncodeToString(preimage) != hex.EncodeToString(o.st.secret) {
		return errSellBadHash
	}
	return nil
}
func (o *fakeSellTakerOps) ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.btcClaimedBy = append([]byte(nil), o.st.secret...)
	return "sell-btc-claim-" + leg.Funded.TxID, nil
}

var (
	errSellTimeout = fakeErr("fake sell: counterparty did not act in time")
	errSellBadHash = fakeErr("fake sell: P mismatch")
)

func TestSubAssetSellHandshake(t *testing.T) {
	// The maker mints P; expose it to both fakes (as the real LN would once settled).
	P := sha256.Sum256([]byte("sell-secret"))
	st := &sellState{secret: P[:]}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetAtoms = uint64(50_000) // asset the taker pays over LN
	const btcSats = uint64(2_000)     // BTC the taker receives on-chain
	const tBtc = uint32(200)

	var makerRes *MakerSubAssetSellResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerSubAssetSell(MakerSubAssetSellParams{
			NewMakerOps: func(preimage []byte) SubAssetSellMakerOps { return &fakeSellMakerOps{st: st} },
			Crypter:     mc,
			BtcAmount:   btcSats,
			AssetAmount: assetAtoms,
			BtcLocktime: tBtc,
			MinBTCConf:  1,
			HoldTimeout: 3 * time.Second,
			Preimage:    P[:],
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubAssetSell(TakerSubAssetSellParams{
		NewTakerOps: func(hashH []byte) SubAssetSellTakerOps { return &fakeSellTakerOps{st: st} },
		Crypter:     tc,
		BtcAmount:   btcSats,
		AssetAmount: assetAtoms,
		MinBTCConf:  1,
		Timing:      XcTiming{TermsWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if !makerRes.Settled {
		t.Fatalf("maker did not settle (take the asset): %+v", makerRes)
	}
	if !takerRes.Received || takerRes.BtcClaimTxid == "" {
		t.Fatalf("taker did not claim the BTC: %+v", takerRes)
	}
	// Both converged on P; the BTC was claimed with it.
	want := sha256.Sum256(takerRes.Preimage)
	if hex.EncodeToString(takerRes.HashH) != hex.EncodeToString(want[:]) {
		t.Fatalf("H != SHA256(P)")
	}
	if hex.EncodeToString(st.btcClaimedBy) != hex.EncodeToString(takerRes.Preimage) {
		t.Fatalf("BTC claimed with the wrong preimage")
	}
	// A whole lift (TakeAssetAmount unset = 0) fills the whole offer on both sides.
	if takerRes.FilledAsset != assetAtoms || takerRes.FilledBtc != btcSats {
		t.Fatalf("whole lift changed shape: filled %d asset / %d sats, want %d / %d",
			takerRes.FilledAsset, takerRes.FilledBtc, assetAtoms, btcSats)
	}
	if makerRes.FilledAsset != assetAtoms || makerRes.FilledBtc != btcSats {
		t.Fatalf("maker whole lift changed shape: %+v", makerRes)
	}
	if st.btcAmt != btcSats {
		t.Fatalf("whole lift locked %d sats, want %d", st.btcAmt, btcSats)
	}
}

// TestSubAssetSellOnState pins the crash-recovery callback: OnState must fire exactly twice —
// "verified" (BTC HTLC terms known, P still nil) then "paid" (P set and hashing to H) — and the
// "paid" snapshot must carry everything needed to reconstruct the settled result.
func TestSubAssetSellOnState(t *testing.T) {
	P := sha256.Sum256([]byte("sell-onstate-secret"))
	st := &sellState{secret: P[:]}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetAtoms = uint64(50_000)
	const btcSats = uint64(2_000)
	const tBtc = uint32(200)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = RunMakerSubAssetSell(MakerSubAssetSellParams{
			NewMakerOps: func(preimage []byte) SubAssetSellMakerOps { return &fakeSellMakerOps{st: st} },
			Crypter:     mc,
			BtcAmount:   btcSats,
			AssetAmount: assetAtoms,
			BtcLocktime: tBtc,
			MinBTCConf:  1,
			HoldTimeout: 3 * time.Second,
			Preimage:    P[:],
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	// OnState fires synchronously on THIS (taker) goroutine, so a plain slice is race-free.
	var states []SubAssetSellState
	takerRes, takerErr := RunTakerSubAssetSell(TakerSubAssetSellParams{
		NewTakerOps: func(hashH []byte) SubAssetSellTakerOps { return &fakeSellTakerOps{st: st} },
		Crypter:     tc,
		BtcAmount:   btcSats,
		AssetAmount: assetAtoms,
		MinBTCConf:  1,
		OnState:     func(s SubAssetSellState) { states = append(states, s) },
		Timing:      XcTiming{TermsWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()
	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if len(states) != 2 {
		t.Fatalf("want 2 OnState calls (verified, paid), got %d: %+v", len(states), states)
	}

	// (1) verified: terms known, asset NOT yet paid (no preimage).
	v := states[0]
	if v.Phase != "verified" {
		t.Fatalf("first phase = %q, want verified", v.Phase)
	}
	if len(v.Preimage) != 0 {
		t.Fatalf("verified snapshot must not carry a preimage")
	}
	if v.BtcTxid == "" || v.BtcAmount != btcSats || v.BtcLocktime != tBtc ||
		len(v.HashH) != 32 || len(v.TakerClaimPub) == 0 || len(v.MakerRefundPub) == 0 {
		t.Fatalf("verified snapshot missing BTC HTLC terms: %+v", v)
	}

	// (2) paid: P set, hashes to H, terms unchanged — enough to reconstruct the settled result.
	p := states[1]
	if p.Phase != "paid" {
		t.Fatalf("second phase = %q, want paid", p.Phase)
	}
	if got := sha256.Sum256(p.Preimage); hex.EncodeToString(got[:]) != hex.EncodeToString(p.HashH) {
		t.Fatalf("paid snapshot preimage does not hash to H")
	}
	if hex.EncodeToString(p.Preimage) != hex.EncodeToString(takerRes.Preimage) {
		t.Fatalf("paid snapshot preimage != the returned preimage")
	}
	if p.BtcTxid != v.BtcTxid || p.BtcAmount != v.BtcAmount || p.BtcLocktime != v.BtcLocktime {
		t.Fatalf("paid snapshot BTC terms drifted from verified: %+v vs %+v", p, v)
	}
}

// --- PARTIAL FILLS on the sub-asset SELL rail ---------------------------------
//
// The mirror of the pure-LN slice tests (xdriver_pureln_test.go): the taker NAMES
// the asset-side slice in the terms request, both sides derive the on-chain BTC
// side from the SIGNED offer with the taker-receives-BTC rounding (ceil), and the
// maker locks ONLY what the slice requires.

// A genuine slice: deliberately non-divisible amounts so floor and ceil differ.
// Both derivations must agree on the ceil, and the maker's HTLC must carry exactly
// the priced slice (not the offer's whole).
func TestSubAssetSellPartialFill(t *testing.T) {
	P := sha256.Sum256([]byte("sell-partial-secret"))
	st := &sellState{secret: P[:]}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const wholeAsset = uint64(3_000_000)
	const wholeBtc = uint64(1_000_001)
	const take = wholeAsset / 3
	const wantBtc = uint64(333_334) // ceil(1000001/3): the taker RECEIVES the on-chain leg
	const tBtc = uint32(200)

	var makerRes *MakerSubAssetSellResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerSubAssetSell(MakerSubAssetSellParams{
			NewMakerOps: func(preimage []byte) SubAssetSellMakerOps { return &fakeSellMakerOps{st: st} },
			Crypter:     mc,
			BtcAmount:   wholeBtc,
			AssetAmount: wholeAsset,
			BtcLocktime: tBtc,
			MinBTCConf:  1,
			HoldTimeout: 3 * time.Second,
			Preimage:    P[:],
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubAssetSell(TakerSubAssetSellParams{
		NewTakerOps:     func(hashH []byte) SubAssetSellTakerOps { return &fakeSellTakerOps{st: st} },
		Crypter:         tc,
		BtcAmount:       wholeBtc,
		AssetAmount:     wholeAsset,
		TakeAssetAmount: take,
		MinBTCConf:      1,
		Timing:          XcTiming{TermsWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if takerRes.FilledAsset != take || takerRes.FilledBtc != wantBtc {
		t.Fatalf("taker filled %d asset / %d sats, want %d / %d",
			takerRes.FilledAsset, takerRes.FilledBtc, take, wantBtc)
	}
	if makerRes.FilledAsset != take || makerRes.FilledBtc != wantBtc {
		t.Fatalf("maker settled %d asset / %d sats, want the %d / %d slice (it used to lock the whole offer)",
			makerRes.FilledAsset, makerRes.FilledBtc, take, wantBtc)
	}
	// Fund-safety: the maker locked ONLY the slice on-chain, and the taker's verify
	// bound to that same slice (a whole-offer HTLC would have been refused).
	if st.btcAmt != wantBtc {
		t.Fatalf("maker locked %d sats on-chain, want the %d slice", st.btcAmt, wantBtc)
	}
	if !makerRes.Settled || !takerRes.Received {
		t.Fatalf("partial did not settle: maker %+v taker %+v", makerRes, takerRes)
	}
}

// Take >= the offer clamps to the whole offer (the documented "0 or >= offer =
// whole" contract), so an over-ask never fails and never over-locks.
func TestSubAssetSellOverAskClampsToWhole(t *testing.T) {
	P := sha256.Sum256([]byte("sell-overask-secret"))
	st := &sellState{secret: P[:]}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetAtoms = uint64(50_000)
	const btcSats = uint64(2_000)
	const tBtc = uint32(200)

	var makerRes *MakerSubAssetSellResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, _ = RunMakerSubAssetSell(MakerSubAssetSellParams{
			NewMakerOps: func(preimage []byte) SubAssetSellMakerOps { return &fakeSellMakerOps{st: st} },
			Crypter:     mc,
			BtcAmount:   btcSats,
			AssetAmount: assetAtoms,
			BtcLocktime: tBtc,
			MinBTCConf:  1,
			HoldTimeout: 3 * time.Second,
			Preimage:    P[:],
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubAssetSell(TakerSubAssetSellParams{
		NewTakerOps:     func(hashH []byte) SubAssetSellTakerOps { return &fakeSellTakerOps{st: st} },
		Crypter:         tc,
		BtcAmount:       btcSats,
		AssetAmount:     assetAtoms,
		TakeAssetAmount: assetAtoms * 2, // over-ask: clamps to the whole offer
		MinBTCConf:      1,
		Timing:          XcTiming{TermsWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if takerRes.FilledAsset != assetAtoms || takerRes.FilledBtc != btcSats {
		t.Fatalf("over-ask did not clamp to the whole: %+v", takerRes)
	}
	if makerRes == nil || !makerRes.Settled || makerRes.FilledAsset != assetAtoms {
		t.Fatalf("maker did not settle the whole on an over-ask: %+v", makerRes)
	}
}

// A maker that re-prices the slice (quotes a BTC amount other than the one the
// SIGNED offer's ratio gives) is refused BEFORE the taker binds ops or pays
// anything — the taker trusts only its own derivation from the signed offer.
func TestSubAssetSellMakerRepricedSliceRefused(t *testing.T) {
	st := &sellState{secret: []byte("unused")}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const wholeAsset = uint64(3_000_000)
	const wholeBtc = uint64(1_000_001)
	const take = wholeAsset / 3
	const wantBtc = uint64(333_334) // the honest ceil price of the slice

	// Hand-rolled dishonest maker: answers the terms request with the taker's own
	// slice but a re-priced BTC side (+7 sats), leg sized to match its lie.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		recv := chanRecv(net.toMaker)
		req, err := recvXcType(recv, mc, XcSubAsSellTermsRequest, 2*time.Second)
		if err != nil {
			t.Errorf("fake maker: %v", err)
			return
		}
		if req.SeqAmount != take {
			t.Errorf("terms request named slice %d, want %d", req.SeqAmount, take)
		}
		h := sha256.Sum256([]byte("reprice"))
		_ = sendXc(&XcMsg{
			Type:           XcSubAsSellTerms,
			HashH:          hex.EncodeToString(h[:]),
			MakerLNNodeID:  "02" + hex.EncodeToString(make([]byte, 32)),
			BtcAmount:      wantBtc + 7, // the re-price
			SeqAmount:      req.SeqAmount,
			MakerRefundPub: hex.EncodeToString([]byte{0x02, 0xbb}),
			Leg:            &XcLeg{Txid: "reprice-htlc", Vout: 0, Amount: wantBtc + 7, RedeemScript: "51", Locktime: 200},
		}, mc, net.makerSend)
	}()

	opsBound := false
	takerRes, takerErr := RunTakerSubAssetSell(TakerSubAssetSellParams{
		NewTakerOps: func(hashH []byte) SubAssetSellTakerOps {
			opsBound = true
			return &fakeSellTakerOps{st: st}
		},
		Crypter:         tc,
		BtcAmount:       wholeBtc,
		AssetAmount:     wholeAsset,
		TakeAssetAmount: take,
		MinBTCConf:      1,
		Timing:          XcTiming{TermsWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr == nil {
		t.Fatalf("taker accepted a re-priced slice: %+v", takerRes)
	}
	if !errors.Is(takerErr, ErrXcBadTerms) {
		t.Fatalf("want ErrXcBadTerms, got: %v", takerErr)
	}
	if opsBound {
		t.Fatalf("taker bound settlement ops for a re-priced slice (must refuse before anything moves)")
	}
	if st.takerPaid {
		t.Fatalf("taker paid the asset on a re-priced slice")
	}
}

// A slice that prices to zero sats (only possible when the offer's BTC side is
// itself zero — ceil never rounds a funded offer to nothing) is refused before a
// single frame leaves the taker.
func TestSubAssetSellDustSliceRefused(t *testing.T) {
	st := &sellState{}
	tc, _ := testCrypters(t)
	net := newFakeXcNet()

	_, takerErr := RunTakerSubAssetSell(TakerSubAssetSellParams{
		NewTakerOps:     func(hashH []byte) SubAssetSellTakerOps { return &fakeSellTakerOps{st: st} },
		Crypter:         tc,
		BtcAmount:       0, // a zero-priced offer: any slice is dust
		AssetAmount:     1_000,
		TakeAssetAmount: 10,
		MinBTCConf:      1,
		Timing:          XcTiming{TermsWait: 50 * time.Millisecond, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)

	if takerErr == nil {
		t.Fatalf("taker accepted a slice priced to 0 sats")
	}
	if !errors.Is(takerErr, ErrXcBadTerms) {
		t.Fatalf("want ErrXcBadTerms (dust), got: %v", takerErr)
	}
	if len(net.toMaker) != 0 {
		t.Fatalf("dust refusal must happen before anything is sent; %d frame(s) left the taker", len(net.toMaker))
	}
	if st.takerPaid {
		t.Fatalf("taker paid the asset on a dust slice")
	}
}
