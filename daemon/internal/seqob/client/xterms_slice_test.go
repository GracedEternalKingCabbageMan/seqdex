package client

// TERMS NAME THE SLICE BEING TRADED, NOT THE WHOLE RESTING OFFER.
//
// Both ends of a partial must state the same numbers. The forward driver used to
// demand terms equal to the whole offer and then price the slice itself, so a
// maker quoting the slice — which is what seqob-maker does, and what the wallet
// and the LSP's bridge bind check both read — was rejected with "btc_amount
// differs from the signed offer" even though the two sides had computed the
// identical proportional price. A live partial cross take died exactly there.
//
// These tests pin the contract from the taker's side: the slice is accepted, the
// whole-offer quote is refused, and a maker cannot use the looser check to quote
// a worse ratio than the one it signed.

import (
	"strings"
	"sync"
	"testing"
)

// A maker quoting the WHOLE offer for a PARTIAL take must be refused: under the
// slice contract those are different trades, and accepting it would fund the
// whole offer's price for a fraction of its asset.
func TestForwardRefusesWholeOfferQuoteOnAPartialTake(t *testing.T) {
	_, net, tp, mp := forwardFixture(t)
	const takeSeq = uint64(2_500_000) // half of the 5_000_000 offer

	tp.TakeSeqAmount = takeSeq
	// mp keeps the fixture's WHOLE-offer amounts: the pre-fix maker behaviour.

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	_, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)
	wg.Wait()

	if terr == nil {
		t.Fatal("a whole-offer quote against a half-offer take was ACCEPTED; the taker would " +
			"have locked the whole offer's BTC for half its asset")
	}
	if !strings.Contains(terr.Error(), "seq_amount") {
		t.Fatalf("want a seq_amount mismatch naming the slice, got: %v", terr)
	}
}

// The maker may not quote a cheaper-for-itself ratio than the one it signed. The
// taker recomputes the price from its OWN verified copy of the offer, so terms
// are only ever accepted when they match that exactly.
func TestForwardRefusesTermsThatBeatTheSignedRatio(t *testing.T) {
	_, net, tp, mp := forwardFixture(t)
	const takeSeq = uint64(2_500_000)
	wantBtc := ProportionalBtc(tp.ExpectBtcAmount, takeSeq, tp.ExpectSeqAmount)

	tp.TakeSeqAmount = takeSeq
	// Right slice of the asset, but the maker asks MORE BTC for it than its own
	// signed offer prices that slice at.
	mp.SeqAmount, mp.BtcAmount = takeSeq, wantBtc+1

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	_, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)
	wg.Wait()

	if terr == nil {
		t.Fatal("the maker over-charged for the slice and the taker accepted it")
	}
	if !strings.Contains(terr.Error(), "btc_amount") {
		t.Fatalf("want a btc_amount mismatch, got: %v", terr)
	}
}

// A WHOLE lift is byte-identical under the slice contract, because the slice IS
// the whole offer and ProportionalBtc returns the whole price. This is what keeps
// the change from being a wire break for non-partial takes.
func TestForwardWholeLiftIsTheSliceContractsDegenerateCase(t *testing.T) {
	_, net, tp, mp := forwardFixture(t)
	whole := tp.ExpectSeqAmount

	if got := ProportionalBtc(tp.ExpectBtcAmount, whole, whole); got != tp.ExpectBtcAmount {
		t.Fatalf("whole-take price = %d, want the offer's own %d", got, tp.ExpectBtcAmount)
	}
	tp.TakeSeqAmount = 0 // 0 means "the whole offer"
	// The fixture's mp already quotes the whole offer, which under the slice
	// contract is exactly what a whole take must be quoted.

	var (
		wg   sync.WaitGroup
		mres *MakerForwardResult
		merr error
	)
	wg.Add(1)
	go func() { defer wg.Done(); mres, merr = RunMakerForward(mp, net.toMaker, net.makerSend) }()

	tres, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)
	if terr != nil {
		t.Fatalf("whole lift broke under the slice contract: %v", terr)
	}
	wg.Wait()
	if merr != nil {
		t.Fatalf("maker: %v", merr)
	}
	if tres.SeqClaimTxid != "seq-claim" || mres.BtcClaimTxid != "btc-claim" {
		t.Fatalf("whole lift did not settle: taker %+v maker %+v", tres, mres)
	}
	if tres.FilledSeq != whole || tres.FilledBtc != tp.ExpectBtcAmount {
		t.Fatalf("whole fill = %d/%d, want %d/%d",
			tres.FilledSeq, tres.FilledBtc, whole, tp.ExpectBtcAmount)
	}
}
