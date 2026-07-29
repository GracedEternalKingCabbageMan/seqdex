package client

// xdriver_anchorgate_test.go pins the ANCHOR PRECONDITION that makes Andreas's
// invariant true instead of merely hoped-for:
//
//	"If the on-chain BTC leg always goes first, the Sequentia leg will always be
//	 anchored to a Bitcoin block equal or greater in height to the one that
//	 included the BTC leg."
//
// Nothing used to enforce that. The asset FUNDER verified the counterparty's BTC
// leg and immediately funded, so whether the confirming Sequentia block committed
// anchorheight >= the BTC-leg height was left to luck — and on testnet4 luck is
// against it, because -anchoravoidcontested deliberately backs a new block's
// anchor DOWN to the last uncontested parent height (85% of blocks anchor below
// the local bitcoind tip; median gap 3). The claimant's gate then correctly
// refused, and because a block's anchorheight is a COMMITTED header field, the
// claimant's retry loop could never clear it.
//
// The fix is a precondition, not a weaker gate: the funder waits for its own
// node's LIVE anchor view to reach the BTC-leg height BEFORE any asset moves.
// Anchors are monotone non-decreasing along a chain (consensus rule R2), so every
// block extending that tip — including the one that confirms the funding —
// satisfies the claimant's gate by construction, on its FIRST read.
//
// This file also pins the FOUR things that wait has to get right, each of which
// was a hole in the first cut:
//   - the BTC leg is RE-VERIFIED after the wait (it can be double-spent during it);
//   - the wait stops at the TIMELOCK, never at a wall-clock constant (owner ruling
//     2026-07-25) — and is cancellable by the user;
//   - the funder aims one block ABOVE the leg height, absorbing the claimant's
//     height-derivation race instead of letting it refuse mid-swap;
//   - the reverse asset funder carries the same post-funding assertion the forward
//     maker does, and refunds rather than inviting a claim on an under-anchored leg.
//
// These tests fail without those fixes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// TestForwardMakerWaitsForAnchorBeforeFundingAsset reproduces the FAILED live run
// (BTC leg at 145609, committee anchor lagging at 145607) in miniature: the
// maker's anchor starts BELOW the taker's BTC-leg height and catches up shortly
// after. With the precondition the maker holds the asset until its anchor reaches
// the BTC-leg height, so the funding block satisfies the taker's gate on the
// first read and the swap settles.
//
// WITHOUT the fix the maker funds immediately, the confirming block commits the
// lagging anchor, and the taker terminally refuses to claim — the exact live
// failure.
func TestForwardMakerWaitsForAnchorBeforeFundingAsset(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	// Our anchor lags the parent tip by 2 blocks, exactly as -anchoravoidcontested
	// makes it do on a contested testnet4.
	st.seqAnchorTip = st.btcTip - 2

	// The committee anchors forward a moment later (a Sequentia block or two).
	go func() {
		time.Sleep(120 * time.Millisecond)
		st.mu.Lock()
		st.seqAnchorTip = st.btcLegHeightLocked() + 1
		st.mu.Unlock()
	}()

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
	if tres.SeqClaimTxid != "seq-claim" {
		t.Fatalf("taker did not claim: %+v", tres)
	}
	if mres.BtcClaimTxid != "btc-claim" {
		t.Fatalf("maker did not claim BTC: %+v", mres)
	}
	// The invariant itself: the block that confirmed the asset leg commits an
	// anchor at or above the BTC-leg height.
	st.mu.Lock()
	legAnchor, btcLegHeight, funds := st.legBlockAnchor, st.btcLegHeightLocked(), st.seqFundCalls
	st.mu.Unlock()
	if legAnchor < btcLegHeight {
		t.Fatalf("asset leg funded into a block anchored at %d, BELOW the BTC-leg height %d: the invariant was not enforced", legAnchor, btcLegHeight)
	}
	if funds != 1 {
		t.Fatalf("asset leg funded %d times, want exactly 1", funds)
	}
	// And the taker's gate passed on its FIRST read, not after a retry storm.
	if calls := tp.Ops.(*fakeOps).seqLegSafeCallCount(); calls != 1 {
		t.Fatalf("taker ran the anchor gate %d times, want 1 (it should pass by construction)", calls)
	}
}

// TestForwardFunderAimsAboveTheBtcLegHeight pins the off-by-one slack. The
// claimant derives its BTC-leg height from two non-atomic RPCs (tip, then
// confirmations), so a parent block landing between them yields a height one
// HIGHER than the funder measured — and the gate then refuses a leg that was
// actually fine, mid-swap, with the asset already committed.
//
// Waiting longer is never unsafe, so the funder absorbs that race: it aims at
// legHeight+1. Here the anchor sits EXACTLY at the leg height, which the old
// target accepted; the fixed funder must still refuse to fund.
func TestForwardFunderAimsAboveTheBtcLegHeight(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	// Pin the live anchor to exactly the height the BTC leg will confirm at.
	st.btcLegHeight = st.btcTip
	st.seqAnchorTip = st.btcTip
	// Close the funding window shortly after, so the (correct) refusal is observable
	// instead of the test hanging.
	go func() {
		time.Sleep(150 * time.Millisecond)
		st.mu.Lock()
		st.seqTip = 5240 - 120 // seqLocktime - MinSeqClaimWindow
		st.mu.Unlock()
	}()

	done := make(chan error, 1)
	go func() { _, err := RunMakerForward(mp, net.toMaker, net.makerSend); done <- err }()
	_, _ = RunTakerForward(tp, net.takerSend, net.takerRecv)

	select {
	case merr := <-done:
		if merr == nil || !errors.Is(merr, ErrFundWindowClosed) {
			t.Fatalf("maker error = %v, want ErrFundWindowClosed (it must aim ABOVE the leg height)", merr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("maker never returned")
	}
	st.mu.Lock()
	funds := st.seqFundCalls
	st.mu.Unlock()
	if funds != 0 {
		t.Fatalf("maker funded %d times with its anchor level with (not above) the BTC-leg height; it must fund 0 times", funds)
	}
}

// TestForwardMakerWaitStopsAtTheTimelockNotAWallClock is the owner ruling
// (Andreas, 2026-07-25) turned into a test:
//
//	"we should let users decide if the wait is intolerable and they want to cancel
//	 the trade (putting the makers order back to rest), rather than cancel it
//	 automatically. We cannot really predict how long contested blocks will take to
//	 clear anyway."
//
// So a flat AnchorWait must NOT end the wait. The only automatic stop is the
// protocol deadline: the point past which a leg funded now could no longer be
// claimed before T_seq. Here AnchorWait is set absurdly SHORT and the anchor never
// catches up: the maker must keep waiting anyway, and abort only once the SEQ tip
// eats into the claim window — with ErrFundWindowClosed, not a timeout.
func TestForwardMakerWaitStopsAtTheTimelockNotAWallClock(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	st.seqAnchorTip = st.btcTip - 2 // stuck below, never advances
	mp.Timing.AnchorWait = 20 * time.Millisecond
	tp.Timing.SeqLockWait = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() { _, err := RunMakerForward(mp, net.toMaker, net.makerSend); done <- err }()

	// Give the maker far longer than AnchorWait with the window still OPEN. A maker
	// that honours a wall-clock constant returns in ~20ms; the ruling says it must
	// still be waiting.
	go func() {
		time.Sleep(400 * time.Millisecond)
		select {
		case err := <-done:
			done <- err // put it back for the assertion below
			panic("maker aborted on a wall-clock timeout while the timelock window was still open: " + err.Error())
		default:
		}
		st.mu.Lock()
		st.seqTip = 5240 - 120 // T_seq minus MinSeqClaimWindow: the window closes NOW
		st.mu.Unlock()
	}()

	_, _ = RunTakerForward(tp, net.takerSend, net.takerRecv)
	select {
	case merr := <-done:
		if merr == nil || !errors.Is(merr, ErrFundWindowClosed) {
			t.Fatalf("maker error = %v, want ErrFundWindowClosed (the timelock is the only automatic stop)", merr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("maker never aborted even at the timelock deadline")
	}
	st.mu.Lock()
	funds := st.seqFundCalls
	st.mu.Unlock()
	if funds != 0 {
		t.Fatalf("maker funded the asset leg %d times with its anchor behind the BTC leg; it must fund ZERO times", funds)
	}
}

// TestForwardMakerWaitIsCancellable: the ruling also says the USER decides when a
// wait is intolerable, so the wait has to accept a cancellation signal. This is
// the seam the wallet's "cancel this trade and re-rest my order" button uses;
// without it that round would need another refactor of the driver.
func TestForwardMakerWaitIsCancellable(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	st.seqAnchorTip = st.btcTip - 2 // never catches up
	ctx, cancel := context.WithCancel(context.Background())
	mp.Ctx = ctx
	tp.Timing.SeqLockWait = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() { _, err := RunMakerForward(mp, net.toMaker, net.makerSend); done <- err }()
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, _ = RunTakerForward(tp, net.takerSend, net.takerRecv)

	select {
	case merr := <-done:
		if merr == nil || !errors.Is(merr, ErrXcCanceled) {
			t.Fatalf("maker error = %v, want ErrXcCanceled", merr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("cancelling the lift did not stop the anchor wait")
	}
	st.mu.Lock()
	funds := st.seqFundCalls
	st.mu.Unlock()
	if funds != 0 {
		t.Fatalf("a cancelled lift funded the asset %d times; it must fund 0", funds)
	}
}

// TestForwardMakerRefusesToFundWhenTheBtcLegIsDoubleSpentDuringTheWait is the
// FUND-LOSS case the anchor wait itself created. Verification happens BEFORE the
// wait; the wait can now run for as long as the timelock allows; and a BTC HTLC
// that was confirmed at the start can be GONE by the end — a one-block parent
// reorg is all the taker needs to double-spend the input it funded with.
//
// Without a re-check the maker funds its asset leg against a dead BTC leg and has
// bought nothing. The fix re-runs the FULL VerifyBTCLeg immediately before
// LockSEQLeg, so the asset never moves.
func TestForwardMakerRefusesToFundWhenTheBtcLegIsDoubleSpentDuringTheWait(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	st.seqAnchorTip = st.btcTip - 2 // the maker will be parked in the anchor wait

	// While it waits, the taker double-spends the HTLC input; a moment later the
	// anchor catches up, so the ONLY thing standing between the maker and a lost
	// asset leg is the re-verification.
	go func() {
		time.Sleep(100 * time.Millisecond)
		st.mu.Lock()
		st.btcLegGone = true
		st.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		st.mu.Lock()
		st.seqAnchorTip = st.btcLegHeightLocked() + 1
		st.mu.Unlock()
	}()

	done := make(chan error, 1)
	go func() { _, err := RunMakerForward(mp, net.toMaker, net.makerSend); done <- err }()
	tp.Timing.SeqLockWait = 300 * time.Millisecond
	_, _ = RunTakerForward(tp, net.takerSend, net.takerRecv)

	select {
	case merr := <-done:
		if merr == nil || !errors.Is(merr, ErrBtcLegChanged) {
			t.Fatalf("maker error = %v, want ErrBtcLegChanged (the BTC leg was double-spent during the wait)", merr)
		}
	case <-time.After(5 * time.Second):
		st.mu.Lock()
		funds := st.seqFundCalls
		st.mu.Unlock()
		t.Fatalf("maker never returned; it funded the asset leg %d time(s) against a dead BTC leg", funds)
	}
	st.mu.Lock()
	funds := st.seqFundCalls
	st.mu.Unlock()
	if funds != 0 {
		t.Fatalf("maker funded the asset leg %d times against a BTC leg that no longer exists; that is the fund-loss case", funds)
	}
}

// TestReverseTakerWaitsForAnchorBeforeFundingAsset is the same precondition on
// the other side of the book: in the reverse flow the TAKER gives the asset, so
// the taker is the one that must not fund until its anchor has reached the
// maker's BTC-leg height. Covering only the forward site would leave this flow
// with the original bug.
func TestReverseTakerWaitsForAnchorBeforeFundingAsset(t *testing.T) {
	st, net, tp, mp := reverseFixture(t)
	st.seqAnchorTip = st.btcTip - 2
	go func() {
		time.Sleep(120 * time.Millisecond)
		st.mu.Lock()
		st.seqAnchorTip = st.btcLegHeightLocked() + 1
		st.mu.Unlock()
	}()

	type mout struct {
		res *MakerReverseResult
		err error
	}
	type tout struct {
		res *TakerReverseResult
		err error
	}
	mch, tch := make(chan mout, 1), make(chan tout, 1)
	go func() { r, e := RunMakerReverse(mp, net.toMaker, net.makerSend); mch <- mout{r, e} }()
	go func() { r, e := RunTakerReverse(tp, net.takerSend, net.takerRecv); tch <- tout{r, e} }()

	var mres *MakerReverseResult
	var tres *TakerReverseResult
	for i := 0; i < 2; i++ {
		select {
		case m := <-mch:
			if m.err != nil {
				t.Fatalf("maker: %v", m.err)
			}
			mres = m.res
		case tr := <-tch:
			if tr.err != nil {
				t.Fatalf("taker: %v", tr.err)
			}
			tres = tr.res
		case <-time.After(5 * time.Second):
			// Neither side can finish once the asset is funded under-anchored: the
			// maker refuses to claim and the taker waits out T_seq.
			t.Fatalf("reverse swap stalled; the asset leg was funded before our anchor reached the BTC-leg height")
		}
	}
	if !mres.Settled || tres.BtcClaimTxid != "btc-claim" {
		t.Fatalf("reverse swap did not settle: maker %+v taker %+v", mres, tres)
	}
	st.mu.Lock()
	legAnchor, btcLegHeight := st.legBlockAnchor, st.btcLegHeightLocked()
	st.mu.Unlock()
	if legAnchor < btcLegHeight {
		t.Fatalf("reverse: asset leg funded into a block anchored at %d, BELOW the BTC-leg height %d", legAnchor, btcLegHeight)
	}
}

// TestReverseTakerWaitStopsAtTheTimelockNotAWallClock mirrors the forward case:
// the reverse taker is the asset funder, so its wait is bounded by T_seq (the
// window the maker needs to claim in), not by AnchorWait.
func TestReverseTakerWaitStopsAtTheTimelockNotAWallClock(t *testing.T) {
	st, net, tp, mp := reverseFixture(t)
	st.seqAnchorTip = st.btcTip - 2 // stuck
	tp.Timing.AnchorWait = 20 * time.Millisecond

	go func() { _, _ = RunMakerReverse(mp, net.toMaker, net.makerSend) }()
	type tout struct {
		res *TakerReverseResult
		err error
	}
	tch := make(chan tout, 1)
	go func() { r, e := RunTakerReverse(tp, net.takerSend, net.takerRecv); tch <- tout{r, e} }()

	// Still waiting well past AnchorWait, then the timelock window closes.
	go func() {
		time.Sleep(400 * time.Millisecond)
		select {
		case tr := <-tch:
			tch <- tr
			panic("reverse taker aborted on a wall-clock timeout while the timelock window was still open")
		default:
		}
		st.mu.Lock()
		st.seqTip = 8240 - 120 // T_seq minus MinSeqFundWindow
		st.mu.Unlock()
	}()

	select {
	case tr := <-tch:
		if tr.err == nil || !errors.Is(tr.err, ErrFundWindowClosed) {
			t.Fatalf("taker error = %v, want ErrFundWindowClosed", tr.err)
		}
		if tr.res.SeqLeg != nil {
			t.Fatalf("taker must not fund the asset leg while its anchor lags the BTC leg")
		}
	case <-time.After(5 * time.Second):
		st.mu.Lock()
		funds := st.seqFundCalls
		st.mu.Unlock()
		t.Fatalf("taker never aborted: it funded the asset %d time(s) with its anchor behind the BTC leg", funds)
	}
	st.mu.Lock()
	funds := st.seqFundCalls
	st.mu.Unlock()
	if funds != 0 {
		t.Fatalf("reverse taker funded the asset %d times despite a lagging anchor; must be 0", funds)
	}
}

// TestReverseTakerRefusesToFundWhenTheBtcLegIsDoubleSpentDuringTheWait is the
// reverse mirror of the forward double-spend case: here the MAKER funded the BTC
// leg first, so it is the maker that can double-spend it out from under the taker
// while the taker waits out its anchor precondition.
func TestReverseTakerRefusesToFundWhenTheBtcLegIsDoubleSpentDuringTheWait(t *testing.T) {
	st, net, tp, mp := reverseFixture(t)
	st.seqAnchorTip = st.btcTip - 2

	go func() {
		time.Sleep(100 * time.Millisecond)
		st.mu.Lock()
		st.btcLegGone = true
		st.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		st.mu.Lock()
		st.seqAnchorTip = st.btcLegHeightLocked() + 1
		st.mu.Unlock()
	}()

	go func() { _, _ = RunMakerReverse(mp, net.toMaker, net.makerSend) }()
	type tout struct {
		res *TakerReverseResult
		err error
	}
	tch := make(chan tout, 1)
	go func() { r, e := RunTakerReverse(tp, net.takerSend, net.takerRecv); tch <- tout{r, e} }()

	select {
	case tr := <-tch:
		if tr.err == nil || !errors.Is(tr.err, ErrBtcLegChanged) {
			t.Fatalf("taker error = %v, want ErrBtcLegChanged", tr.err)
		}
		if tr.res.SeqLeg != nil {
			t.Fatalf("taker funded its asset leg against a BTC leg that no longer exists")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("reverse taker never returned; it may have funded against a dead BTC leg")
	}
	st.mu.Lock()
	funds := st.seqFundCalls
	st.mu.Unlock()
	if funds != 0 {
		t.Fatalf("reverse taker funded %d times against a dead BTC leg; must be 0", funds)
	}
}

// TestReverseTakerWithholdsAnUnderAnchoredLegAndRefunds covers the residual case
// on the REVERSE side, which had no assertion at all: the taker read the leg
// block's anchor, shrugged off a read failure with `anchorH = 0`, and announced
// regardless. That invites the maker to claim an asset leg whose block anchors
// BELOW the BTC leg — the leg that can outlive the BTC leg — which is the taker's
// own money.
//
// With the fix the taker withholds the announcement and recovers the leg through
// the T_seq refund, rather than stranding it while inviting a one-sided claim.
func TestReverseTakerWithholdsAnUnderAnchoredLegAndRefunds(t *testing.T) {
	st, net, tp, mp := reverseFixture(t)
	// The taker's node reports a caught-up anchor tip (so the precondition passes)
	// while the block that actually confirms the funding commits a LOWER anchor.
	st.seqAnchorTip = st.btcTip - 2
	tp.NewOps = func(hashH []byte) (XcOps, error) {
		return &fakeOps{st: st, hashH: hashH, lieAnchorTip: true}, nil
	}

	mch := make(chan error, 1)
	go func() { _, e := RunMakerReverse(mp, net.toMaker, net.makerSend); mch <- e }()
	type tout struct {
		res *TakerReverseResult
		err error
	}
	tch := make(chan tout, 1)
	go func() { r, e := RunTakerReverse(tp, net.takerSend, net.takerRecv); tch <- tout{r, e} }()

	// Once the leg is funded (and withheld), push the SEQ tip past T_seq so the
	// refund path opens; the taker must take it instead of stranding the asset.
	go func() {
		for i := 0; i < 400; i++ {
			st.mu.Lock()
			funded := st.seqFundCalls > 0
			st.mu.Unlock()
			if funded {
				st.mu.Lock()
				st.seqTip = 8240 + 1
				st.mu.Unlock()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// THE DISCRIMINATOR: the maker must learn about this from our XcFail, i.e. we
	// never sent XcSeqLegFunded. Without the assertion the taker announces the
	// under-anchored leg anyway (the old code shrugged the read off with
	// `anchorH = 0`), and the maker then discovers it only via its own gate — a
	// different error, and an invitation to claim we should never have issued.
	select {
	case merr := <-mch:
		if merr == nil || !errors.Is(merr, ErrXcPeerFailed) {
			t.Fatalf("maker error = %v, want ErrXcPeerFailed: the taker must WITHHOLD an under-anchored leg, not announce it and leave the maker's own gate to catch it", merr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("maker never returned")
	}

	select {
	case tr := <-tch:
		if tr.err == nil || !errors.Is(tr.err, ErrXcRefunded) {
			t.Fatalf("taker error = %v, want ErrXcRefunded (withheld leg must be recovered, not stranded)", tr.err)
		}
		if tr.res.SeqRefundTx != "seq-refund" {
			t.Fatalf("taker did not refund the withheld asset leg: %+v", tr.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("reverse taker stranded an under-anchored asset leg (no refund)")
	}
	st.mu.Lock()
	claimed := st.seqClaimedBy
	st.mu.Unlock()
	if len(claimed) > 0 {
		t.Fatalf("the maker claimed an under-anchored asset leg; the taker must not have announced it")
	}
}

// TestMakerDoesNotAnnounceAnUnderAnchoredLeg covers the residual case the
// precondition cannot rule out: a Sequentia reorg lands the funding on a
// LOWER-anchored branch after we checked. The maker's post-confirmation read is
// an assertion, not a log line: it must withhold the leg announcement so an
// honest taker does not waste a doomed claim on an asset leg that could outlive
// its BTC leg, and refund after T_seq instead.
//
// (Withholding is a COURTESY, not a guarantee — the taker minted the secret and
// holds the maker's refund pubkey, so it can rebuild the script and find the P2SH
// itself. The real defence is the pre-funding precondition, which the tests above
// pin. This one pins that the maker does not INVITE the claim.)
//
// The maker's node is made to report a caught-up anchor tip (so it funds) while
// the block that actually confirmed the leg commits a LOWER anchor.
func TestMakerDoesNotAnnounceAnUnderAnchoredLeg(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	// The maker's own node reports "caught up" (so the precondition passes), but
	// the confirming block really commits an anchor 2 below the BTC-leg height.
	st.seqAnchorTip = st.btcTip - 2
	mp.NewOps = func(h []byte) (XcOps, error) {
		return &fakeOps{st: st, hashH: h, lieAnchorTip: true}, nil
	}
	mp.Timing.SeqLockWait = 2 * time.Second

	// The maker holds the leg to T_seq, so it does not return until we advance the
	// Sequentia tip past the refund height below.
	go func() { _, _ = RunMakerForward(mp, net.toMaker, net.makerSend) }()
	tres, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)

	// The taker must learn about the problem from the maker's XcFail, NOT by
	// running its own gate against an announced leg.
	if terr == nil || !errors.Is(terr, ErrXcPeerFailed) {
		t.Fatalf("taker error = %v, want ErrXcPeerFailed (the maker must withhold an under-anchored leg)", terr)
	}
	if tres.SeqClaimTxid != "" {
		t.Fatalf("taker claimed an under-anchored asset leg: %+v", tres)
	}
	st.mu.Lock()
	claimed := st.seqClaimedBy
	st.seqTip = 10_000_000 // past T_seq: the maker's refund path opens
	st.mu.Unlock()
	if len(claimed) > 0 {
		t.Fatalf("the secret was revealed on an under-anchored leg")
	}
	// The withheld leg is not stranded: the maker recovers it after T_seq.
	deadline := time.Now().Add(3 * time.Second)
	for {
		st.mu.Lock()
		refunded := st.seqRefunded
		st.mu.Unlock()
		if refunded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maker never refunded the withheld asset leg after T_seq")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTakerAnchorGateFailsFastOnImmutableOrdering pins D1. anchorheight is a
// committed header field, so once the leg's block is known the ordering conjunct
// can NEVER flip. The old loop re-read it every poll for the whole AnchorWait
// (~240 identical reads over 20 minutes in the live failure) and only then
// refunded, burning the timelock window for nothing.
//
// The verdict is unchanged — still a refusal. Only the latency changes.
func TestTakerAnchorGateFailsFastOnImmutableOrdering(t *testing.T) {
	st, net, tp, mp := forwardFixture(t)
	// The MAKER's node lies in both directions, so it funds AND announces a leg
	// whose block really commits an anchor below the BTC-leg height. The TAKER's
	// own node tells the truth, so its gate is the one that must refuse.
	st.seqAnchorTip = st.btcTip - 2
	mp.NewOps = func(h []byte) (XcOps, error) {
		return &fakeOps{st: st, hashH: h, lieAnchorTip: true, lieBlockAnchor: true}, nil
	}
	tp.Timing.AnchorWait = 3 * time.Second // plenty of room to spin if the fix is absent
	tp.Timing.Poll = 5 * time.Millisecond

	go func() { _, _ = RunMakerForward(mp, net.toMaker, net.makerSend) }()
	start := time.Now()
	tres, terr := RunTakerForward(tp, net.takerSend, net.takerRecv)
	elapsed := time.Since(start)

	if terr == nil || !errors.Is(terr, xchain.ErrAnchorOrderingTerminal) {
		t.Fatalf("taker error = %v, want ErrAnchorOrderingTerminal", terr)
	}
	// Still an ErrAnchorOrdering for every existing caller: the verdict did not change.
	if !errors.Is(terr, xchain.ErrAnchorOrdering) {
		t.Fatalf("terminal error must still satisfy errors.Is(err, ErrAnchorOrdering): %v", terr)
	}
	if tres.SeqClaimTxid != "" {
		t.Fatalf("taker claimed despite a failed anchor gate: %+v", tres)
	}
	if calls := tp.Ops.(*fakeOps).seqLegSafeCallCount(); calls != 1 {
		t.Fatalf("taker re-read an IMMUTABLE header field %d times; it must read it once and abort", calls)
	}
	if elapsed >= tp.Timing.AnchorWait {
		t.Fatalf("taker burned the whole %s AnchorWait (%s) on a verdict that could never change", tp.Timing.AnchorWait, elapsed)
	}
}

// TestAnchorGateWaitsOutAnOrphanedConfirmingBlock pins the re-derivation. The gate
// used to capture the leg's confirming block hash ONCE and re-read that hash
// forever. getblockheader answers for ORPHANED blocks too — it reads the header
// index, not the chain — so the cached hash keeps returning a verdict about a
// block that is no longer part of anything: a Sequentia reorg that re-mines the
// leg into a BETTER-anchored block stays invisible, and an orphan freezes the
// verdict.
//
// Here the confirming block starts off the active chain and re-joins a moment
// later. The gate must WAIT (never read a disconnected header) and then pass.
func TestAnchorGateWaitsOutAnOrphanedConfirmingBlock(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = 1000
	st.legBlockAnchor = 1000
	st.seqBlockOrphan = true
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	go func() {
		time.Sleep(60 * time.Millisecond)
		st.mu.Lock()
		st.seqBlockOrphan = false
		st.mu.Unlock()
	}()

	timing := XcTiming{Poll: 5 * time.Millisecond, AnchorWait: 2 * time.Second}
	timing.setDefaults()
	ev, err := gateSeqLeg(context.Background(), ops, "seq-htlc", 1000,
		claimWindow{SeqLocktime: 5200, Margin: 10}, timing, nil)
	if err != nil {
		t.Fatalf("gate should have waited out the orphan and passed: %v", err)
	}
	if ev == nil || !ev.OK {
		t.Fatalf("evidence not OK: %+v", ev)
	}
	// And while the block was disconnected the gate must NOT have read its header.
	if calls := ops.seqLegSafeCallCount(); calls != 1 {
		t.Fatalf("gate ran the verdict %d times; it must skip a disconnected block entirely and read once it is back", calls)
	}
}

// TestAnchorGateRefusesAPermanentlyOrphanedBlock: if the leg's block never
// rejoins the active chain there is nothing to trust, so the gate must fail
// rather than fall back to a stale header.
func TestAnchorGateRefusesAPermanentlyOrphanedBlock(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = 1000
	st.legBlockAnchor = 1000
	st.seqBlockOrphan = true
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	timing := XcTiming{Poll: 5 * time.Millisecond, AnchorWait: 80 * time.Millisecond}
	timing.setDefaults()
	// The gate now stops on the TIMELOCK, not a clock: T_seq 5005 against tip 5000
	// with a 10-block margin is already closed, so it gives up after one pass.
	if _, err := gateSeqLeg(context.Background(), ops, "seq-htlc", 1000,
		claimWindow{SeqLocktime: 5005, Margin: 10}, timing, nil); err == nil {
		t.Fatalf("gate ACCEPTED a leg whose confirming block is not on the active chain")
	}
	if calls := ops.seqLegSafeCallCount(); calls != 0 {
		t.Fatalf("gate read a disconnected block's header %d times; it must never trust one", calls)
	}
}

// TestBuryingBlockDoesNotRescueAnUnderAnchoredLeg is the REGRESSION GUARD against
// the tempting "fix" of pointing the gate at a later/burying block or at the
// chain tip (whose anchor DOES advance, so the 20-minute wait would have
// "passed" in ~90 seconds).
//
// It would delete the protection. InvalidateBlock disconnects the offending block
// and everything ABOVE it while lower blocks stay CONNECTED, so orphaning the
// burying block's anchor does NOT invalidate the block that holds the funding
// output. Concretely, with the failed run's numbers: leg block anchored 145607,
// burying block anchored 145609, Bitcoin reorgs to fork point 145608. The burying
// block dies; the leg block LIVES; the claim re-mines against the surviving
// funding output while the BTC leg is orphaned and double-spent. The asset giver
// loses everything.
//
// So: a leg whose OWN block anchors below the BTC-leg height must be REFUSED no
// matter how well-anchored the blocks above it are.
func TestBuryingBlockDoesNotRescueAnUnderAnchoredLeg(t *testing.T) {
	const (
		btcLegHeight = int64(145609) // the taker's BTC HTLC confirmed here
		legAnchor    = int64(145607) // the block holding the asset leg committed this
		buryAnchor   = int64(145609) // a later block, anchored high enough to pass a naive gate
	)
	st := &fakeChainState{btcTip: btcLegHeight, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = btcLegHeight
	st.legBlockAnchor = legAnchor
	// The chain moves on and the committee anchors forward: the LIVE view (and any
	// burying block minted from it) now clears the BTC-leg height.
	st.seqAnchorTip = buryAnchor
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	// Sanity: the relaxed reading that option (A) would have used does pass.
	if tipAnchor, _, _ := ops.SeqAnchorTip(); tipAnchor < btcLegHeight {
		t.Fatalf("test setup: the burying/tip anchor %d should clear the BTC-leg height %d", tipAnchor, btcLegHeight)
	}
	// The gate must nonetheless REFUSE, because it reads the FUNDING block.
	ev, err := ops.VerifySeqLegSafe("seq-block-hash", btcLegHeight)
	if err == nil {
		t.Fatalf("the anchor gate ACCEPTED a leg whose own block anchors at %d < BTC-leg height %d because later blocks anchor higher; that is the fund-loss case", legAnchor, btcLegHeight)
	}
	if !errors.Is(err, xchain.ErrAnchorOrderingTerminal) {
		t.Fatalf("want ErrAnchorOrderingTerminal, got %v", err)
	}
	if ev.OK {
		t.Fatalf("evidence reported OK for an under-anchored leg: %+v", ev)
	}
	if ev.SeqBlockAnchor != legAnchor {
		t.Fatalf("the gate read anchor %d; it must read the FUNDING block's committed anchor %d", ev.SeqBlockAnchor, legAnchor)
	}
	// And it stays refused however long we wait: the value is a committed header
	// field, so the retry loop was futile by construction.
	for i := 0; i < 50; i++ {
		st.mu.Lock()
		st.seqAnchorTip += 1 // the chain keeps anchoring forward
		st.mu.Unlock()
		if _, err := ops.VerifySeqLegSafe("seq-block-hash", btcLegHeight); err == nil {
			t.Fatalf("the gate flipped to ACCEPT after the chain tip advanced; the leg block's anchor is immutable and must never be re-derived from a later block")
		}
	}
}

// TestBtcLegHeightIsDerivedAtomically pins the second half of the off-by-one fix.
// tip - confirmations + 1 is only meaningful if no parent block lands between the
// two reads. Here one does, on the FIRST attempt: the naive derivation would
// return 1001 for a leg that really confirmed at 1000, and the counterparty would
// then be held to an anchor it was never asked for.
func TestBtcLegHeightIsDerivedAtomically(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{"btc-htlc": 1}}
	st.btcLegHeight = 1000
	ops := &raceyBtcOps{fakeOps: &fakeOps{st: st, hashH: make([]byte, 32)}}

	h, _, err := btcLegConfirmedHeight(ops, "btc-htlc", 1)
	if err != nil {
		t.Fatalf("derive height: %v", err)
	}
	if h != 1000 {
		t.Fatalf("BTC-leg height derived as %d, want 1000 (a block landing mid-derivation must not inflate it)", h)
	}
}

// raceyBtcOps advances the parent chain exactly once, between the first
// confirmations read and the tip read — the race the naive derivation loses.
type raceyBtcOps struct {
	*fakeOps
	reads int
}

func (r *raceyBtcOps) BtcConfirmations(txid string) (int, error) {
	r.reads++
	if r.reads == 1 {
		return 1, nil // the stale, pre-block count
	}
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	return int(r.st.btcTip - r.st.btcLegHeight + 1), nil
}

func (r *raceyBtcOps) BtcTip() (int64, error) {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	if r.reads == 1 {
		r.st.btcTip++ // a block lands right here
	}
	return r.st.btcTip, nil
}

// unusedRef keeps the imports honest if a case above is edited out.
var _ = hex.EncodeToString
var _ = sha256.Sum256

// --- §4 round-2 residuals: the claimant gate's bound, and a hostile height ----

// TestAnchorGateOutlivesTheFlatBudget pins ROUND-2 ITEM 2. The claimant gate used
// to be bounded by time.Now().Add(timing.AnchorWait) — the flat 20 minutes, merely
// relabelled "claimant flap budget". That is exactly the wall-clock abort the owner
// ruled out (Andreas 2026-07-25): a contested anchor that takes 21 minutes to clear
// would kill a wait that still had HOURS of timelock left, stranding a swap that
// was about to succeed.
//
// Here AnchorWait is deliberately far SHORTER than the time the anchor needs. The
// gate must ignore it entirely and pass, because the timelock still leaves a claim
// window. With the old flat deadline this test fails: the gate returns before the
// anchor ever catches up.
func TestAnchorGateOutlivesTheFlatBudget(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = 1000
	st.legBlockAnchor = 1000
	st.seqBlockOrphan = true // TRANSIENT: the leg is being re-mined, so the gate must wait
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	// The leg re-joins the active chain well after the flat budget would have expired.
	go func() {
		time.Sleep(120 * time.Millisecond)
		st.mu.Lock()
		st.seqBlockOrphan = false
		st.mu.Unlock()
	}()

	timing := XcTiming{Poll: 5 * time.Millisecond, AnchorWait: 20 * time.Millisecond}
	timing.setDefaults()
	start := time.Now()
	ev, err := gateSeqLeg(context.Background(), ops, "seq-htlc", 1000,
		claimWindow{SeqLocktime: 5200, Margin: 10}, timing, nil)
	if err != nil {
		t.Fatalf("gate aborted on the flat AnchorWait while the timelock still allowed a claim: %v", err)
	}
	if ev == nil || !ev.OK {
		t.Fatalf("evidence not OK: %+v", ev)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Fatalf("gate returned after %s, before the anchor could possibly have caught up", elapsed)
	}
}

// TestAnchorGateStopsWhenTheTimelockCloses is the other half of item 2: replacing
// the wall clock must not make the wait unbounded. The bound is the PROTOCOL
// deadline — once the SEQ tip is within the claim margin of T_seq, claiming would
// race the counterparty's refund, so continuing to wait cannot produce a usable
// result and the gate must stop there.
func TestAnchorGateStopsWhenTheTimelockCloses(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = 1000
	st.legBlockAnchor = 1000
	st.seqBlockOrphan = true // never re-mined, so the gate would wait indefinitely
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	// The chain advances into the claim margin while we wait.
	go func() {
		time.Sleep(40 * time.Millisecond)
		st.mu.Lock()
		st.seqTip = 5195 // 5195 + 10 >= 5200
		st.mu.Unlock()
	}()

	timing := XcTiming{Poll: 5 * time.Millisecond, AnchorWait: time.Hour}
	timing.setDefaults()
	_, err := gateSeqLeg(context.Background(), ops, "seq-htlc", 1000,
		claimWindow{SeqLocktime: 5200, Margin: 10}, timing, nil)
	if err == nil {
		t.Fatalf("gate kept waiting past the point where a claim could still land")
	}
	if !errors.Is(err, ErrFundWindowClosed) {
		t.Fatalf("err = %v, want ErrFundWindowClosed", err)
	}
}

// TestAnchorGateRefusesAnUnboundedWait: a zero T_seq means the caller supplied no
// timelock. That is a programming error, and treating it as "wait forever" would
// silently restore the behaviour claimWindow exists to remove, so the gate refuses
// up front rather than spinning.
func TestAnchorGateRefusesAnUnboundedWait(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = 1000
	st.legBlockAnchor = 1000
	st.seqBlockOrphan = true
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	timing := XcTiming{Poll: time.Millisecond, AnchorWait: time.Hour}
	timing.setDefaults()
	done := make(chan error, 1)
	go func() {
		_, err := gateSeqLeg(context.Background(), ops, "seq-htlc", 1000, claimWindow{}, timing, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ErrXcBadTerms) {
			t.Fatalf("err = %v, want ErrXcBadTerms", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("gate spun forever when given no timelock to bound it")
	}
}

// TestAnchorGateHonoursCancellation pins the user's own exit from a slow wait: the
// owner ruled that a human decides when a wait is intolerable, so ctx cancellation
// must end the gate promptly. Nothing is committed here — the secret is unrevealed
// and both legs stay refundable — so cancelling is always safe.
func TestAnchorGateHonoursCancellation(t *testing.T) {
	st := &fakeChainState{btcTip: 1000, seqTip: 5000, btcConfs: map[string]int{}}
	st.btcLegHeight = 1000
	st.legBlockAnchor = 1000
	st.seqBlockOrphan = true // never re-mined, so the gate would wait indefinitely
	ops := &fakeOps{st: st, hashH: make([]byte, 32)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	timing := XcTiming{Poll: 5 * time.Millisecond, AnchorWait: time.Hour}
	timing.setDefaults()
	done := make(chan error, 1)
	go func() {
		_, err := gateSeqLeg(ctx, ops, "seq-htlc", 1000,
			claimWindow{SeqLocktime: 9000, Margin: 10}, timing, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ErrXcCanceled) {
			t.Fatalf("err = %v, want ErrXcCanceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("gate ignored ctx cancellation")
	}
}

// TestClaimantBtcLegHeightClampsAHostileReport pins ROUND-2 ITEM 5. Leg.Height
// arrives in a COURIER message, so the counterparty picks the number. It was used
// unbounded to raise the funder's wait target, which is a remote denial-of-service:
// a peer reporting 2^62 sets a target no anchor will ever reach and the funder
// burns its entire timelock waiting for it.
//
// Our own node verified the same leg, so the true height is `ours`; the peer's
// value exists only to absorb a few blocks of derivation race and indexing lag.
func TestClaimantBtcLegHeightClampsAHostileReport(t *testing.T) {
	const ours = 1000

	// A hostile report must not move the target beyond the sane slack.
	if got := claimantBtcLegHeight(ours, 1<<62); got != ours+maxCounterpartyHeightSlack {
		t.Fatalf("hostile height 2^62 produced target %d; want the clamp %d", got, ours+maxCounterpartyHeightSlack)
	}
	// A plausible lag is still honoured, since raising the bar is safety-monotone.
	if got := claimantBtcLegHeight(ours, ours+2); got != ours+2 {
		t.Fatalf("a 2-block lag was not honoured: %d", got)
	}
	// Exactly at the limit is still honoured.
	if got := claimantBtcLegHeight(ours, ours+maxCounterpartyHeightSlack); got != ours+maxCounterpartyHeightSlack {
		t.Fatalf("the slack limit itself was clamped away: %d", got)
	}
	// A LOWER report never lowers the bar: our own verification wins.
	if got := claimantBtcLegHeight(ours, 10); got != ours {
		t.Fatalf("a lower counterparty height lowered the target to %d", got)
	}
}

// TestReverseResumeHonoursCancellation pins ROUND-2 ITEM 3. The reverse-maker gates
// used to call gateSeqLeg(context.Background(), ...) because MakerReverseParams and
// MakerReverseResumeParams had no Ctx field at all, so a user-initiated cancel had
// no way in and the brief's "every wait accepts cancellation" was unmet on this
// side. Cancelling here is safe: the secret is not revealed and the BTC leg is
// refundable at T_btc.
func TestReverseResumeHonoursCancellation(t *testing.T) {
	secret := make([]byte, 32)
	secret[0] = 7
	h := sha256.Sum256(secret)
	// btcTip is already past T_btc so the refund the cancel falls through to can
	// complete at once; otherwise this test would block on the refund, not the gate.
	st := &fakeChainState{btcTip: 2200, seqTip: 8000, btcConfs: map[string]int{"btc-htlc": 3}}
	st.btcLegHeight = 2000
	st.legBlockAnchor = 2000
	st.seqBlockOrphan = true // transient failure, so the gate waits rather than aborting
	ops := (&fakeOps{st: st, hashH: h[:]}).withSecret(secret)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := ResumeMakerReverse(MakerReverseResumeParams{
			Ops:          ops,
			Ctx:          ctx,
			BtcLeg:       &xchain.LegLock{Script: []byte("btc"), Funded: &xchain.FundedHTLC{TxID: "btc-htlc", Vout: 0, Amount: 25000}, Locktime: 2100},
			SeqLeg:       &xchain.LegLock{Script: []byte("seq"), Funded: &xchain.FundedHTLC{TxID: "seq-htlc", Vout: 1, Amount: 5000000, AssetID: testAsset}, Locktime: 8240},
			SeqBlockHash: "seq-block-hash",
			Secret:       secret,
			HashH:        h[:],
			SeqClaimKey:  mustKey(t),
			BtcRefundKey: mustKey(t),
			BtcLocktime:  2100,
			SeqLocktime:  8240,
			AssetHex:     testAsset,
			BtcAmount:    25000,
			SeqAmount:    5000000,
			Timing:       XcTiming{Poll: 5 * time.Millisecond},
		})
		// However it exits, it must NOT have revealed the secret by claiming.
		if err == nil && res != nil && res.SeqClaimTxid != "" {
			t.Errorf("reverse resume claimed despite a failed gate: %+v", res)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("reverse resume ignored ctx cancellation and kept waiting")
	}
}
