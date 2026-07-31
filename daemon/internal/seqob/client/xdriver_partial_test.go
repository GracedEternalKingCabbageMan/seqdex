package client

// xdriver_partial_test.go exercises PARTIAL FILLS on the CROSS-CHAIN HTLC rail —
// the spec's "buy 10 of a resting 43" fix for BTC pairs. It reuses the in-process
// fake-courier + fake-chain fixtures from xdriver_test.go (forwardFixture /
// reverseFixture / fakeOps / scriptedTaker). The fund-safety invariants under test:
//   - a partial forward lift pays ProportionalBtc (CEIL, in the maker's favour) and
//     the maker delivers exactly the slice, reporting FilledSeq/FilledBtc so the
//     serve loop can re-rest the remainder;
//   - a partial reverse lift pays ProportionalBtcFloor (FLOOR, in the maker's favour
//     since the maker GIVES the BTC there), reported the same way;
//   - a taker that under-pays the maker (a wrong proportional BTC amount) is rejected
//     BEFORE any leg the maker cannot recover is created — every value-move fails closed;
//   - an over-ask (take > offer) is rejected;
//   - the pricing helpers never wrap uint64 at realistic atom scales;
//   - the reverse floor+subtraction re-rest can never over-commit the offer's BTC.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestForwardPartialFill: the taker buys HALF a resting cross offer. It locks the
// PROPORTIONAL BTC (ceil), the maker delivers exactly the slice, and both sides
// report the fill (FilledSeq/FilledBtc) so the serve loop can re-rest the rest.
func TestForwardPartialFill(t *testing.T) {
	_, net, tp, mp := forwardFixture(t)
	// Offer is 5_000_000 asset for 25_000 sats. Take half.
	const takeSeq = uint64(2_500_000)
	const wantBtc = uint64(12_500) // ProportionalBtc(25_000, 2.5M, 5M)
	if got := ProportionalBtc(tp.ExpectBtcAmount, takeSeq, tp.ExpectSeqAmount); got != wantBtc {
		t.Fatalf("proportional BTC = %d, want %d", got, wantBtc)
	}
	tp.TakeSeqAmount = takeSeq
	// TERMS NAME THE SLICE, not the whole resting offer: the maker quotes what THIS
	// session settles, mirroring seqob-maker, which sizes its terms from the
	// relay-forwarded take_amount before handing them to RunMakerForward.
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
		t.Fatalf("partial did not settle: taker %+v maker %+v", tres, mres)
	}
	if string(mres.Secret) != string(tp.Secret) {
		t.Fatal("maker extracted the wrong secret")
	}
	// The taker locked exactly the proportional BTC and the SEQ leg it claimed was the slice.
	if tres.FilledSeq != takeSeq || tres.FilledBtc != wantBtc {
		t.Fatalf("taker fill = %d/%d, want %d/%d", tres.FilledSeq, tres.FilledBtc, takeSeq, wantBtc)
	}
	if mres.FilledSeq != takeSeq || mres.FilledBtc != wantBtc {
		t.Fatalf("maker fill = %d/%d, want %d/%d", mres.FilledSeq, mres.FilledBtc, takeSeq, wantBtc)
	}
	if tres.BtcLeg == nil || tres.BtcLeg.Funded.Amount != wantBtc {
		t.Fatalf("taker BTC leg amount = %v, want %d", tres.BtcLeg, wantBtc)
	}
	if mres.SeqLeg == nil || mres.SeqLeg.Funded.Amount != takeSeq {
		t.Fatalf("maker SEQ leg amount = %v, want %d", mres.SeqLeg, takeSeq)
	}
}

// TestForwardWholeOfferUnchanged: TakeSeqAmount==0 (and ==whole) still lifts the
// whole offer at the whole price — the partial path must not regress it.
func TestForwardWholeOfferUnchanged(t *testing.T) {
	for _, take := range []uint64{0, 5_000_000} {
		_, net, tp, mp := forwardFixture(t)
		tp.TakeSeqAmount = take
		var wg sync.WaitGroup
		var mres *MakerForwardResult
		wg.Add(1)
		go func() { defer wg.Done(); mres, _ = RunMakerForward(mp, net.toMaker, net.makerSend) }()
		tres, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)
		wg.Wait()
		if terr != nil {
			t.Fatalf("take=%d taker: %v", take, terr)
		}
		if tres.FilledSeq != 5_000_000 || tres.FilledBtc != 25_000 {
			t.Fatalf("take=%d fill = %d/%d, want whole 5000000/25000", take, tres.FilledSeq, tres.FilledBtc)
		}
		if mres.FilledSeq != 5_000_000 || mres.FilledBtc != 25_000 {
			t.Fatalf("take=%d maker fill = %d/%d, want whole", take, mres.FilledSeq, mres.FilledBtc)
		}
	}
}

// TestForwardMakerRejectsUnderpaidPartial: a taker that announces a valid slice but
// funds LESS BTC than the proportional price is refused by the maker BEFORE it locks
// its asset leg (the maker is never underpaid; the funds it never risks are safe).
func TestForwardMakerRejectsUnderpaidPartial(t *testing.T) {
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
	const takeSeq = uint64(2_500_000)
	const underpaid = uint64(12_400) // proportional is 12_500; fund 100 sats short
	_ = sendXc(&XcMsg{
		Type: XcBtcLegFunded, HashH: hex.EncodeToString(hashH[:]),
		TakerSeqClaimPub:  hex.EncodeToString(tp.SeqClaimKey.PubKey()),
		TakerBtcRefundPub: hex.EncodeToString(tp.BtcRefundKey.PubKey()),
		SeqAmount:         takeSeq,
		BtcAmount:         underpaid,
		Leg: &XcLeg{
			Txid: "btc-htlc", Vout: 0, Amount: underpaid,
			RedeemScript: hex.EncodeToString(fakeScript("btc", hashH[:], makerClaim, tp.BtcRefundKey.PubKey(), terms.BtcLocktime)),
			Locktime:     terms.BtcLocktime, Height: legH,
		},
	}, tp.Crypter, net.takerSend)
	wg.Wait()
	if merr == nil || !strings.Contains(merr.Error(), "btc leg amount") {
		t.Fatalf("maker must reject an underpaid partial; got %v", merr)
	}
	if mres.SeqLeg != nil {
		t.Fatal("maker must not lock its asset leg against an underpaid BTC leg")
	}
	// And it couriered an XcFail (fails closed, tells the taker).
	sealed, err := net.takerRecv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m, oerr := OpenXcMsg(sealed, tp.Crypter); oerr != nil || m.Type != XcFail {
		t.Fatalf("expected XcFail, got %v %v", m, oerr)
	}
}

// TestForwardMakerRejectsOverAsk: a taker announcing a slice LARGER than the offer is
// refused before anything is locked (never over-deliver the offer's committed asset).
func TestForwardMakerRejectsOverAsk(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	var (
		wg   sync.WaitGroup
		merr error
	)
	wg.Add(1)
	go func() { defer wg.Done(); _, merr = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	s := &scriptedTaker{t: t, tp: tp, net: net}
	terms := s.requestTerms()
	makerClaim, _ := hex.DecodeString(terms.MakerBtcClaimPub)
	hashH := sha256.Sum256(tp.Secret)
	st.mu.Lock()
	st.confirmBtcLegLocked("btc-htlc")
	legH := st.btcLegHeightLocked()
	st.mu.Unlock()
	_ = sendXc(&XcMsg{
		Type: XcBtcLegFunded, HashH: hex.EncodeToString(hashH[:]),
		TakerSeqClaimPub:  hex.EncodeToString(tp.SeqClaimKey.PubKey()),
		TakerBtcRefundPub: hex.EncodeToString(tp.BtcRefundKey.PubKey()),
		SeqAmount:         mp.SeqAmount + 1, // more than the offer
		BtcAmount:         26_000,
		Leg: &XcLeg{
			Txid: "btc-htlc", Vout: 0, Amount: 26_000,
			RedeemScript: hex.EncodeToString(fakeScript("btc", hashH[:], makerClaim, tp.BtcRefundKey.PubKey(), terms.BtcLocktime)),
			Locktime:     terms.BtcLocktime, Height: legH,
		},
	}, tp.Crypter, net.takerSend)
	wg.Wait()
	if merr == nil || !strings.Contains(merr.Error(), "> offer") {
		t.Fatalf("maker must reject an over-ask; got %v", merr)
	}
}

// TestReversePartialFill: the taker sells HALF into a resting cross BUY offer. The
// maker pays the PROPORTIONAL BTC (floor, in its favour) and buys exactly the slice.
func TestReversePartialFill(t *testing.T) {
	_, net, tp, mp := reverseFixture(t)
	const takeSeq = uint64(2_500_000)
	const payBtc = uint64(12_500) // ProportionalBtcFloor(25_000, 2.5M, 5M)
	if got := ProportionalBtcFloor(tp.ExpectBtcAmount, takeSeq, tp.ExpectSeqAmount); got != payBtc {
		t.Fatalf("proportional-floor BTC = %d, want %d", got, payBtc)
	}
	tp.TakeSeqAmount = takeSeq

	var (
		wg   sync.WaitGroup
		mres *MakerReverseResult
		merr error
	)
	wg.Add(1)
	go func() { defer wg.Done(); mres, merr = RunMakerReverse(mp, net.toMaker, net.makerSend) }()

	tres, terr := RunTakerReverse(tp, net.takerSend, net.takerRecv)
	if terr != nil {
		t.Fatalf("taker: %v", terr)
	}
	wg.Wait()
	if merr != nil {
		t.Fatalf("maker: %v", merr)
	}
	if mres.SeqClaimTxid != "seq-claim" || !mres.Settled || tres.BtcClaimTxid != "btc-claim" {
		t.Fatalf("reverse partial did not settle: taker %+v maker %+v", tres, mres)
	}
	if string(tres.Secret) != string(mres.Secret) {
		t.Fatal("taker learned the wrong secret")
	}
	if tres.FilledSeq != takeSeq || tres.FilledBtc != payBtc {
		t.Fatalf("taker fill = %d/%d, want %d/%d", tres.FilledSeq, tres.FilledBtc, takeSeq, payBtc)
	}
	if mres.FilledSeq != takeSeq || mres.FilledBtc != payBtc {
		t.Fatalf("maker fill = %d/%d, want %d/%d", mres.FilledSeq, mres.FilledBtc, takeSeq, payBtc)
	}
	if mres.BtcLeg == nil || mres.BtcLeg.Funded.Amount != payBtc {
		t.Fatalf("maker BTC leg amount = %v, want %d", mres.BtcLeg, payBtc)
	}
	if tres.SeqLeg == nil || tres.SeqLeg.Funded.Amount != takeSeq {
		t.Fatalf("taker SEQ leg amount = %v, want %d", tres.SeqLeg, takeSeq)
	}
}

// TestReverseTakerRejectsUnderpayingMaker: a maker that pays LESS than the floor
// proportional for the taker's slice is refused before the taker funds its asset leg
// (a maker cannot quote a worse ratio than its signed offer; the taker fails closed).
func TestReverseTakerRejectsUnderpayingMaker(t *testing.T) {
	_, net, tp, mp := reverseFixture(t)
	tp.TakeSeqAmount = 2_500_000 // taker expects floor(25_000, 2.5M, 5M) = 12_500
	mp.BtcAmount = 24_000        // maker will fund floor(24_000, 2.5M, 5M) = 12_000 < 12_500

	go func() { _, _ = RunMakerReverse(mp, net.toMaker, net.makerSend) }()
	res, err := RunTakerReverse(tp, net.takerSend, net.takerRecv)
	if err == nil || !errors.Is(err, ErrXcBadTerms) {
		t.Fatalf("want ErrXcBadTerms for an underpaying maker, got %v", err)
	}
	if res.SeqLeg != nil {
		t.Fatal("taker must not fund the asset against an underpaying BTC leg")
	}
}

// TestProportionalBtcFloorCorrectness: floor pricing matches a big.Int reference at
// realistic atom scales (the products overflow a bare uint64 multiply), and the
// whole-take / dust edges behave (whole -> the whole price; a tiny slice -> possibly 0,
// which every caller rejects as dust).
func TestProportionalBtcFloorCorrectness(t *testing.T) {
	for _, c := range []struct{ wholeBtc, take, whole, want uint64 }{
		{200_000, 50_000, 100_000, 100_000},    // exactly half
		{3, 1, 100, 0},                         // dust: floor(3/100) rounds a tiny slice to 0 (caller rejects)
		{100, 1, 3, 33},                        // floor(100/3)
		{100, 2, 3, 66},                        // floor(200/3)
		{25_000, 5_000_000, 5_000_000, 25_000}, // whole take -> whole price (early return)
		{25_000, 6_000_000, 5_000_000, 25_000}, // over-whole clamps to whole (early return)
		{100_000_000, 1_050_000_000_000_000, 2_100_000_000_000_000, 50_000_000}, // half, overflow-scale
	} {
		if got := ProportionalBtcFloor(c.wholeBtc, c.take, c.whole); got != c.want {
			t.Errorf("ProportionalBtcFloor(%d,%d,%d) = %d, want %d", c.wholeBtc, c.take, c.whole, got, c.want)
		}
	}
	// Cross-check overflow-scale products against a big.Int FLOOR.
	for _, tc := range []struct{ b, tk, w uint64 }{
		{100_000_000, 999_999_999_999_999, 2_100_000_000_000_000},
		{4_500_000_000, 3_300_000_000, 9_000_000_000},
		{2_100_000_000_000_000, 7, 2_100_000_000_000_001},
	} {
		num := new(big.Int).Mul(new(big.Int).SetUint64(tc.b), new(big.Int).SetUint64(tc.tk))
		num.Div(num, new(big.Int).SetUint64(tc.w))
		if got := ProportionalBtcFloor(tc.b, tc.tk, tc.w); got != num.Uint64() {
			t.Errorf("ProportionalBtcFloor(%d,%d,%d) = %d, big.Int floor = %s", tc.b, tc.tk, tc.w, got, num.String())
		}
	}
}

// TestReverseFloorReRestNeverOverCommits models the reverse serve loop's re-rest
// (pay floor for each slice, then SUBTRACT the exact paid sats from the offer's BTC
// budget) over a full sweep of an offer into arbitrary slices. Invariant: the maker
// pays AT MOST its offer's original BTC across every possible partition, and EXACTLY
// that when the sweep consumes the whole asset — so a stream of partial fills can
// never over-commit the offer's committed capital. This is the reason the reverse
// re-rest subtracts rather than re-pricing each remainder with a ceil.
func TestReverseFloorReRestNeverOverCommits(t *testing.T) {
	cases := []struct {
		offerBtc, offerBase uint64
		slices              []uint64 // must sum to offerBase
	}{
		{100, 3, []uint64{1, 1, 1}},
		{25_000, 5_000_000, []uint64{1_000_000, 1_500_000, 2_500_000}},
		{7, 10, []uint64{3, 3, 3, 1}}, // very low price: floors round hard
		{3, 100, []uint64{99, 1}},     // the sub-asset-warned free-drain edge
		{1_000_000_003, 7, []uint64{2, 2, 3}},
	}
	for _, c := range cases {
		btc, base := c.offerBtc, c.offerBase
		var paid uint64
		for i, s := range c.slices {
			last := i == len(c.slices)-1
			var pay uint64
			if last || s >= base {
				pay = btc // the whole remainder pays out the whole remaining budget
			} else {
				pay = ProportionalBtcFloor(btc, s, base)
			}
			if pay > btc {
				t.Fatalf("case %v: slice %d paid %d > remaining budget %d", c, s, pay, btc)
			}
			paid += pay
			btc -= pay
			base -= s
		}
		if base != 0 {
			t.Fatalf("case %v: slices did not sum to offerBase (left %d)", c, base)
		}
		if paid != c.offerBtc {
			t.Fatalf("case %v: full sweep paid %d, want exactly the offer's %d (never more)", c, paid, c.offerBtc)
		}
	}
}
