package client

// xminslice_test.go exercises the minimum-slice guard for PARTIAL cross-chain
// fills (xminslice.go): a partial must never produce a leg so small that, after
// the HTLC spend fee, its claim/refund output is sub-dust and stranded. It reuses
// the in-process fake-courier + fake-chain fixtures from xdriver_test.go.
//
// Invariants under test:
//   - MinSafeBtcLegSats == dust(546) + 2x the spend-fee floor(1000) = 2546, and a
//     lower fee floors up to the default while a higher fee raises it;
//   - MinFillBase is the smallest base slice whose BTC leg clears that floor, and
//     collapses to "whole take only" when even the whole offer barely clears it;
//   - a sub-dust partial fails CLOSED in BOTH the forward and reverse driver, on
//     BOTH the taker and maker side, BEFORE any leg is funded;
//   - the smallest slice min_fill advertises still settles end to end.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMinSafeBtcLegSatsMatchesDustFee: the BTC-leg floor is dust + 2x the fee, the
// fee floored up to the 1000-sat default and raised by a larger spend fee.
func TestMinSafeBtcLegSatsMatchesDustFee(t *testing.T) {
	if got := MinSafeBtcLegSats(1000); got != 546+2*1000 {
		t.Fatalf("MinSafeBtcLegSats(1000) = %d, want %d", got, 546+2*1000)
	}
	if got := MinSafeBtcLegSats(0); got != 2546 {
		t.Fatalf("MinSafeBtcLegSats(0) must floor to the default = 2546, got %d", got)
	}
	if got := MinSafeBtcLegSats(500); got != 2546 {
		t.Fatalf("a sub-floor fee must floor up: MinSafeBtcLegSats(500) = %d, want 2546", got)
	}
	if got := MinSafeBtcLegSats(5000); got != 546+2*5000 {
		t.Fatalf("MinSafeBtcLegSats(5000) = %d, want %d", got, 546+2*5000)
	}
}

// TestMinFillBaseMeetsFloor: min_fill is the smallest base slice whose BTC leg
// (ceil forward AND floor reverse) clears the safe minimum, clamped to
// [1, wholeAsset], and equals wholeAsset when even the whole barely clears it.
func TestMinFillBaseMeetsFloor(t *testing.T) {
	const wholeBtc, wholeAsset = uint64(25_000), uint64(5_000_000)
	mf := MinFillBase(wholeBtc, wholeAsset, 1000)
	// 2546 * 5_000_000 / 25_000 = 509_200 exactly.
	if mf != 509_200 {
		t.Fatalf("MinFillBase = %d, want 509200", mf)
	}
	min := MinSafeBtcLegSats(1000)
	if leg := ProportionalBtc(wholeBtc, mf, wholeAsset); leg < min {
		t.Fatalf("forward BTC leg at min_fill = %d, below floor %d", leg, min)
	}
	if leg := ProportionalBtcFloor(wholeBtc, mf, wholeAsset); leg < min {
		t.Fatalf("reverse BTC leg at min_fill = %d, below floor %d", leg, min)
	}
	// Whole-only collapse: a whole offer whose BTC barely clears the floor cannot be
	// safely partialled, so min_fill == base_amount (which the validator requires be
	// <= base_amount).
	if got := MinFillBase(45, 100, 1000); got != 100 {
		t.Fatalf("MinFillBase(45,100,1000) = %d, want 100 (whole take only)", got)
	}
	if got := MinFillBase(2546, 100, 1000); got != 100 {
		t.Fatalf("MinFillBase at exactly the floor = %d, want 100 (whole take only)", got)
	}
	// Empty offer.
	if got := MinFillBase(25_000, 0, 1000); got != 0 {
		t.Fatalf("MinFillBase with 0 asset = %d, want 0", got)
	}
	// min_fill never exceeds base_amount (validator invariant).
	for _, c := range []struct{ btc, asset uint64 }{{1, 1}, {25_000, 5_000_000}, {10, 1_000_000_000}, {1_000_000_000, 3}} {
		if got := MinFillBase(c.btc, c.asset, 1000); got > c.asset {
			t.Fatalf("MinFillBase(%d,%d) = %d exceeds base_amount %d", c.btc, c.asset, got, c.asset)
		}
	}
}

// TestForwardTakerRejectsSubDustPartial: a forward taker asked to buy a slice that
// prices to a sub-dust BTC leg fails CLOSED before funding the BTC leg.
func TestForwardTakerRejectsSubDustPartial(t *testing.T) {
	_, net, tp, mp := forwardFixture(t)
	tp.TakeSeqAmount = 1 // fundBtc = ceil(25000*1/5M) = 1 sat << 2546

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	res, err := RunTakerForward(tp, net.takerSend, net.takerRecv)
	wg.Wait()
	if err == nil || !errors.Is(err, ErrXcBadTerms) {
		t.Fatalf("want ErrXcBadTerms for a sub-dust partial, got %v", err)
	}
	if res.BtcLeg != nil {
		t.Fatal("taker must not fund a BTC leg for a sub-dust partial")
	}
}

// TestForwardMakerRejectsSubDustPartial: even if a taker announces a correctly
// priced but sub-dust slice, the maker fails CLOSED before locking its asset leg,
// and couriers an XcFail.
func TestForwardMakerRejectsSubDustPartial(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	var (
		wg   sync.WaitGroup
		merr error
		mres *MakerForwardResult
	)
	wg.Add(1)
	go func() { defer wg.Done(); mres, merr = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	s := &scriptedTaker{t: t, tp: tp, net: net}
	terms := s.requestTerms()
	makerClaim, _ := hex.DecodeString(terms.MakerBtcClaimPub)
	hashH := sha256.Sum256(tp.Secret)
	st.mu.Lock()
	st.confirmBtcLegLocked("btc-htlc")
	legH := st.btcLegHeightLocked()
	st.mu.Unlock()
	const takeSeq = uint64(1)
	const priced = uint64(1) // ProportionalBtc(25000,1,5M) = 1: correctly priced, still sub-dust
	_ = sendXc(&XcMsg{
		Type: XcBtcLegFunded, HashH: hex.EncodeToString(hashH[:]),
		TakerSeqClaimPub:  hex.EncodeToString(tp.SeqClaimKey.PubKey()),
		TakerBtcRefundPub: hex.EncodeToString(tp.BtcRefundKey.PubKey()),
		SeqAmount:         takeSeq,
		BtcAmount:         priced,
		Leg: &XcLeg{
			Txid: "btc-htlc", Vout: 0, Amount: priced,
			RedeemScript: hex.EncodeToString(fakeScript("btc", hashH[:], makerClaim, tp.BtcRefundKey.PubKey(), terms.BtcLocktime)),
			Locktime:     terms.BtcLocktime, Height: legH,
		},
	}, tp.Crypter, net.takerSend)
	wg.Wait()
	if merr == nil || !errors.Is(merr, ErrXcBadTerms) || !strings.Contains(merr.Error(), "safe minimum") {
		t.Fatalf("maker must reject a sub-dust partial with the min-slice error; got %v", merr)
	}
	if mres.SeqLeg != nil {
		t.Fatal("maker must not lock its asset leg for a sub-dust partial")
	}
	sealed, err := net.takerRecv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m, oerr := OpenXcMsg(sealed, tp.Crypter); oerr != nil || m.Type != XcFail {
		t.Fatalf("expected XcFail, got %v %v", m, oerr)
	}
}

// TestForwardSmallestSliceSettles: the smallest slice min_fill advertises (whose
// BTC leg == the floor exactly) still settles end to end.
func TestForwardSmallestSliceSettles(t *testing.T) {
	_, net, tp, mp := forwardFixture(t)
	takeSeq := MinFillBase(tp.ExpectBtcAmount, tp.ExpectSeqAmount, 1000) // 509200
	wantBtc := ProportionalBtc(tp.ExpectBtcAmount, takeSeq, tp.ExpectSeqAmount)
	if wantBtc != MinSafeBtcLegSats(1000) {
		t.Fatalf("smallest slice BTC leg = %d, want exactly the floor %d", wantBtc, MinSafeBtcLegSats(1000))
	}
	tp.TakeSeqAmount = takeSeq
	// Terms name the slice this session settles, not the whole resting offer.
	mp.SeqAmount, mp.BtcAmount = takeSeq, wantBtc

	var (
		wg   sync.WaitGroup
		mres *MakerForwardResult
		merr error
	)
	wg.Add(1)
	go func() { defer wg.Done(); mres, merr = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	tres, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)
	if terr != nil {
		t.Fatalf("taker: %v", terr)
	}
	wg.Wait()
	if merr != nil {
		t.Fatalf("maker: %v", merr)
	}
	if tres.SeqClaimTxid != "seq-claim" || mres.BtcClaimTxid != "btc-claim" {
		t.Fatalf("smallest slice did not settle: taker %+v maker %+v", tres, mres)
	}
	if tres.FilledSeq != takeSeq || tres.FilledBtc != wantBtc {
		t.Fatalf("taker fill = %d/%d, want %d/%d", tres.FilledSeq, tres.FilledBtc, takeSeq, wantBtc)
	}
	if mres.FilledSeq != takeSeq || mres.FilledBtc != wantBtc {
		t.Fatalf("maker fill = %d/%d, want %d/%d", mres.FilledSeq, mres.FilledBtc, takeSeq, wantBtc)
	}
}

// TestReverseTakerRejectsSubDustPartial: a reverse taker selling a slice that
// prices to a >0-but-sub-dust BTC leg fails CLOSED before funding its asset leg.
func TestReverseTakerRejectsSubDustPartial(t *testing.T) {
	_, net, tp, _ := reverseFixture(t)
	tp.TakeSeqAmount = 400 // wantBtc = floor(25000*400/5M) = 2 sats (>0, << 2546)
	if got := ProportionalBtcFloor(tp.ExpectBtcAmount, 400, tp.ExpectSeqAmount); got == 0 || got >= MinSafeBtcLegSats(1000) {
		t.Fatalf("test setup: slice must price to (0, floor); got %d", got)
	}
	res, err := RunTakerReverse(tp, net.takerSend, net.takerRecv)
	if err == nil || !errors.Is(err, ErrXcBadTerms) {
		t.Fatalf("want ErrXcBadTerms for a sub-dust reverse partial, got %v", err)
	}
	if res.SeqLeg != nil {
		t.Fatal("taker must not fund an asset leg for a sub-dust partial")
	}
}

// TestReverseMakerRejectsSubDustPartial: the reverse maker fails CLOSED on a
// sub-dust slice BEFORE minting the secret or funding the BTC leg, and couriers an
// XcFail. Driven by a scripted terms request to bypass the taker's own guard.
func TestReverseMakerRejectsSubDustPartial(t *testing.T) {
	_, net, tp, mp := reverseFixture(t)
	var (
		wg   sync.WaitGroup
		merr error
		mres *MakerReverseResult
	)
	wg.Add(1)
	go func() { defer wg.Done(); mres, merr = RunMakerReverse(mp, net.toMaker, net.makerSend) }()

	_ = sendXc(&XcMsg{
		Type:              XcTermsRequest,
		TakerSeqRefundPub: hex.EncodeToString(tp.SeqRefundKey.PubKey()),
		TakerBtcClaimPub:  hex.EncodeToString(tp.BtcClaimKey.PubKey()),
		SeqAmount:         400, // payBtc = floor(25000*400/5M) = 2 sats (>0, << 2546)
	}, tp.Crypter, net.takerSend)
	wg.Wait()
	if merr == nil || !errors.Is(merr, ErrXcBadTerms) {
		t.Fatalf("maker must reject a sub-dust reverse partial; got %v", merr)
	}
	if mres.BtcLeg != nil || len(mres.Secret) != 0 {
		t.Fatal("maker must not mint the secret or fund the BTC leg for a sub-dust partial")
	}
	sealed, err := net.takerRecv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m, oerr := OpenXcMsg(sealed, tp.Crypter); oerr != nil || m.Type != XcFail {
		t.Fatalf("expected XcFail, got %v %v", m, oerr)
	}
}
