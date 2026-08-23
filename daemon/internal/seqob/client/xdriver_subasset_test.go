package client

// xdriver_subasset_test.go exercises the SUB-ASSET handshake fully in-process: a
// taker pays BTC ON-CHAIN and receives an asset OVER LIGHTNING. Both drivers run
// concurrently over channels standing in for the relay courier, against fake ops
// that mimic the two legs — an on-chain BTC HTLC (LockBTCLeg/VerifyBTCLeg/
// ClaimBTCLeg/RefundBTCLeg) and an asset LN hold invoice (PrepareAssetHold/
// WaitAssetHeld/SettleAssetHold + the maker's PayAssetHold). No RPC, no chains, no
// LN node: this pins the handshake protocol and the preimage flow. The leg
// primitives themselves are proven live by pkg/xchain (real-bitcoind cross tests
// + pure-LN M2/M3 tests).

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// subasState is shared by the maker + taker fakes, like the real chain + LN node
// are: the taker's funded BTC HTLC, and the asset hold invoice whose preimage the
// maker learns by "paying" it once the taker settles.
type subasState struct {
	mu sync.Mutex

	// BTC on-chain HTLC (funded by the taker).
	btcTip       int64
	btcTxid      string
	btcAmount    uint64
	btcConfs     int
	btcClaimedBy []byte // P used to claim the BTC HTLC (maker)
	btcRefunded  bool

	// Asset LN hold invoice (issued by the taker).
	secret      []byte // P, recorded by the taker when it mints the hold invoice
	holdHash    []byte // H the invoice is bound to
	holdAmtMsat uint64
	makerPaid   bool // maker's asset payment is in-flight and HELD at the taker
	settled     bool // taker settled the hold with P (asset received)
	canceled    bool
}

// --- maker fake -------------------------------------------------------------

type fakeSubAsMakerOps struct{ st *subasState }

func (o *fakeSubAsMakerOps) AssetLNNodeID() (string, error) {
	return "02" + hex.EncodeToString(make([]byte, 32)), nil
}
func (o *fakeSubAsMakerOps) BtcTip() (int64, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	return o.st.btcTip, nil
}
func (o *fakeSubAsMakerOps) VerifyBTCLeg(hashH, makerClaimPub, takerRefundPub, providedScript []byte, btcLocktime uint32,
	txid string, vout uint32, amount uint64, minConf int) (*xchain.VerifiedBTCLeg, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	if hex.EncodeToString(hashH) != hex.EncodeToString(o.st.holdHash) {
		return nil, xchain.ErrBTCLegInvalid
	}
	if txid != o.st.btcTxid || amount != o.st.btcAmount {
		return nil, xchain.ErrBTCLegInvalid
	}
	if o.st.btcConfs < minConf {
		return nil, xchain.ErrBTCLegUnconfirmed
	}
	return &xchain.VerifiedBTCLeg{
		Leg:           &xchain.LegLock{Script: providedScript, Funded: &xchain.FundedHTLC{TxID: txid, Vout: vout, Amount: amount}, Locktime: btcLocktime},
		Height:        o.st.btcTip,
		Confirmations: o.st.btcConfs,
	}, nil
}

// PayAssetHold "pays" the taker's hold invoice: it marks the payment held, then
// blocks until the taker settles it (revealing P), returning P.
func (o *fakeSubAsMakerOps) PayAssetHold(bolt11 string, h []byte, amtMsat uint64, _ uint32) ([]byte, error) {
	o.st.mu.Lock()
	if hex.EncodeToString(h) != hex.EncodeToString(o.st.holdHash) {
		o.st.mu.Unlock()
		return nil, errSubAsBadHash
	}
	if amtMsat != 0 && o.st.holdAmtMsat != 0 && amtMsat != o.st.holdAmtMsat {
		o.st.mu.Unlock()
		return nil, errSubAsBadAmount
	}
	o.st.makerPaid = true // the payment is now HELD at the taker's node
	o.st.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for {
		o.st.mu.Lock()
		settled, canceled := o.st.settled, o.st.canceled
		secret := o.st.secret
		o.st.mu.Unlock()
		if canceled {
			return nil, errSubAsTimeout
		}
		if settled {
			return append([]byte(nil), secret...), nil // settling revealed P
		}
		if time.Now().After(deadline) {
			return nil, errSubAsTimeout
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// PayAssetHashHold is the pay-by-hash HODL variant: node id is ignored in the fake,
// it drives the same held-then-settle simulation keyed by hash.
func (o *fakeSubAsMakerOps) PayAssetHashHold(takerNodeID string, h []byte, amtMsat uint64) ([]byte, error) {
	return o.PayAssetHold("", h, amtMsat, 0)
}

func (o *fakeSubAsMakerOps) InjectSecret(preimage []byte) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	// The injected preimage must be the P the maker just learned by settling.
	if hex.EncodeToString(preimage) != hex.EncodeToString(o.st.secret) {
		return errSubAsBadHash
	}
	return nil
}
func (o *fakeSubAsMakerOps) ClaimBTCLeg(leg *xchain.LegLock, claimKey *xchain.Key, fee uint64) (string, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.btcClaimedBy = append([]byte(nil), o.st.secret...)
	return "btc-claim-" + leg.Funded.TxID, nil
}

// --- taker fake -------------------------------------------------------------

type fakeSubAsTakerOps struct{ st *subasState }

func (o *fakeSubAsTakerOps) BtcTip() (int64, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	return o.st.btcTip, nil
}
func (o *fakeSubAsTakerOps) BtcConfirmations(txid string) (int, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	return o.st.btcConfs, nil
}
func (o *fakeSubAsTakerOps) LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*xchain.LegLock, int64, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.btcTxid = "btc-htlc-deadbeef"
	o.st.btcAmount = coinsToAtoms(amountCoins)
	o.st.btcConfs = 1 // instantly confirmed in the fake
	return &xchain.LegLock{
		Script:   []byte{0x51}, // fake redeem script
		Funded:   &xchain.FundedHTLC{TxID: o.st.btcTxid, Vout: 0, Amount: o.st.btcAmount},
		Locktime: locktime,
	}, 0, nil // hp=0 -> the driver polls BtcConfirmations
}
func (o *fakeSubAsTakerOps) PrepareAssetHold(p []byte, amtMsat uint64) (string, []byte, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.secret = append([]byte(nil), p...)
	h := sha256.Sum256(p)
	o.st.holdHash = h[:]
	o.st.holdAmtMsat = amtMsat
	return "ln-asset-hold-" + hex.EncodeToString(h[:4]), h[:], nil
}
func (o *fakeSubAsTakerOps) WaitAssetHeld(h []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		o.st.mu.Lock()
		paid := o.st.makerPaid
		o.st.mu.Unlock()
		if paid {
			return nil
		}
		if time.Now().After(deadline) {
			return errSubAsTimeout
		}
		time.Sleep(3 * time.Millisecond)
	}
}
func (o *fakeSubAsTakerOps) SettleAssetHold(h, preimage []byte) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.settled = true
	return nil
}
func (o *fakeSubAsTakerOps) CancelAssetHold(h []byte) error {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.canceled = true
	return nil
}
func (o *fakeSubAsTakerOps) WaitAssetPaid(h []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		o.st.mu.Lock()
		settled := o.st.settled
		o.st.mu.Unlock()
		if settled {
			return nil
		}
		if time.Now().After(deadline) {
			return errSubAsTimeout
		}
		time.Sleep(3 * time.Millisecond)
	}
}
func (o *fakeSubAsTakerOps) RefundBTCLeg(leg *xchain.LegLock, refundKey *xchain.Key, nLockTime uint32, fee uint64) (string, error) {
	o.st.mu.Lock()
	defer o.st.mu.Unlock()
	o.st.btcRefunded = true
	return "btc-refund-" + leg.Funded.TxID, nil
}

var (
	errSubAsBadHash   = fakeErr("fake subas: H mismatch")
	errSubAsBadAmount = fakeErr("fake subas: asset amount mismatch")
	errSubAsTimeout   = fakeErr("fake subas: counterparty did not act in time")
)

// TestSubAssetHandshake drives the full happy path: the taker funds BTC on-chain,
// the maker verifies it and pays the asset over LN, the taker settles (revealing
// P), and the maker claims the BTC with P. Both sides converge on the same P and
// the BTC HTLC ends up claimed by P (not refunded).
func TestSubAssetHandshake(t *testing.T) {
	st := &subasState{btcTip: 100}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetAtoms = uint64(100_000) // asset atoms the maker pays over LN
	const btcSats = uint64(200_000)    // sats the taker locks on-chain
	const tBtc = uint32(200)           // T_btc, comfortably above tip=100

	var makerRes *MakerSubAssetResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerSubAsset(MakerSubAssetParams{
			NewMakerOps:   func(hashH []byte) SubAssetMakerOps { return &fakeSubAsMakerOps{st: st} },
			AssetLNNodeID: "02fakenode",
			Crypter:       mc,
			BtcAmount:     btcSats,
			AssetAmount:   assetAtoms,
			BtcLocktime:   tBtc,
			MinBTCConf:    1,
			HoldTimeout:   3 * time.Second,
			Timing:        XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubAsset(TakerSubAssetParams{
		Ops:         &fakeSubAsTakerOps{st: st},
		Crypter:     tc,
		BtcAmount:   btcSats,
		AssetAmount: assetAtoms,
		MinBTCConf:  1,
		Timing:      XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()

	if takerErr != nil {
		t.Fatalf("taker: %v", takerErr)
	}
	if makerErr != nil {
		t.Fatalf("maker: %v", makerErr)
	}
	if !makerRes.Settled || makerRes.BtcClaimTxid == "" {
		t.Fatalf("maker not settled: %+v", makerRes)
	}
	if !takerRes.Received {
		t.Fatalf("taker did not receive the asset: %+v", takerRes)
	}
	// Both sides converged on the same P, and H = SHA256(P).
	if hex.EncodeToString(makerRes.Preimage) != hex.EncodeToString(takerRes.Preimage) {
		t.Fatalf("preimage mismatch: maker %x taker %x", makerRes.Preimage, takerRes.Preimage)
	}
	want := sha256.Sum256(takerRes.Preimage)
	if hex.EncodeToString(takerRes.HashH) != hex.EncodeToString(want[:]) {
		t.Fatalf("H != SHA256(P): H=%x", takerRes.HashH)
	}
	// The BTC HTLC was claimed with P (the maker got the BTC), not refunded.
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.btcRefunded {
		t.Fatal("BTC HTLC was refunded on the happy path")
	}
	if hex.EncodeToString(st.btcClaimedBy) != hex.EncodeToString(takerRes.Preimage) {
		t.Fatalf("BTC claimed with the wrong preimage: %x", st.btcClaimedBy)
	}
}

// TestSubAssetPartialFill (T8): the taker takes HALF the offer, locking the
// proportional BTC; the maker pays exactly the requested slice and reports the fill
// (so its serve loop can re-rest the remainder). The whole-offer path is unchanged.
func TestSubAssetPartialFill(t *testing.T) {
	st := &subasState{btcTip: 100}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const offerAsset = uint64(100_000) // the WHOLE offer
	const offerBtc = uint64(200_000)
	const takeAsset = uint64(50_000) // take half
	const takeBtc = uint64(100_000)  // proportionalBtc(200_000, 50_000, 100_000)
	const tBtc = uint32(200)

	if got := ProportionalBtc(offerBtc, takeAsset, offerAsset); got != takeBtc {
		t.Fatalf("proportional BTC = %d, want %d", got, takeBtc)
	}

	var makerRes *MakerSubAssetResult
	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		makerRes, makerErr = RunMakerSubAsset(MakerSubAssetParams{
			NewMakerOps:   func(hashH []byte) SubAssetMakerOps { return &fakeSubAsMakerOps{st: st} },
			AssetLNNodeID: "02fakenode",
			Crypter:       mc,
			BtcAmount:     offerBtc, // the maker advertises the WHOLE offer
			AssetAmount:   offerAsset,
			BtcLocktime:   tBtc,
			MinBTCConf:    1,
			HoldTimeout:   3 * time.Second,
			Timing:        XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	takerRes, takerErr := RunTakerSubAsset(TakerSubAssetParams{
		Ops:         &fakeSubAsTakerOps{st: st},
		Crypter:     tc,
		BtcAmount:   takeBtc, // the taker locks the PROPORTIONAL BTC for its slice
		AssetAmount: takeAsset,
		MinBTCConf:  1,
		Timing:      XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
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
	if makerRes.FilledAsset != takeAsset || makerRes.FilledBtc != takeBtc {
		t.Fatalf("fill = %d asset / %d btc, want %d / %d", makerRes.FilledAsset, makerRes.FilledBtc, takeAsset, takeBtc)
	}
	if !takerRes.Received {
		t.Fatalf("taker did not receive the asset: %+v", takerRes)
	}
	// The BTC HTLC the taker locked (and the maker verified/claimed) was the partial amount.
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.btcAmount != takeBtc {
		t.Fatalf("BTC leg locked %d, want the proportional %d", st.btcAmount, takeBtc)
	}
}

// TestSubAssetPartialRejectsWrongBtc (T8): a taker that asks for a partial slice but
// locks a NON-proportional BTC amount is refused by the maker before it pays.
func TestSubAssetPartialRejectsWrongBtc(t *testing.T) {
	st := &subasState{btcTip: 100}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()
	const offerAsset, offerBtc = uint64(100_000), uint64(200_000)
	const takeAsset = uint64(50_000)
	const tBtc = uint32(200)

	var makerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, makerErr = RunMakerSubAsset(MakerSubAssetParams{
			NewMakerOps:   func(hashH []byte) SubAssetMakerOps { return &fakeSubAsMakerOps{st: st} },
			AssetLNNodeID: "02fakenode", Crypter: mc,
			BtcAmount: offerBtc, AssetAmount: offerAsset, BtcLocktime: tBtc, MinBTCConf: 1,
			HoldTimeout: 3 * time.Second,
			Timing:      XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
		}, net.toMaker, net.makerSend)
	}()

	// The taker requests half the asset but only locks HALF of the proportional BTC. Its own
	// pre-fund terms check rejects it first, so nothing is funded (its BtcAmount disagrees with
	// the proportional price it must pay). We assert the taker aborts before any BTC lock.
	_, takerErr := RunTakerSubAsset(TakerSubAssetParams{
		Ops: &fakeSubAsTakerOps{st: st}, Crypter: tc,
		BtcAmount:   uint64(50_000), // WRONG: proportional would be 100_000
		AssetAmount: takeAsset, MinBTCConf: 1,
		Timing: XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 3 * time.Second, SeqLockWait: 3 * time.Second, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	wg.Wait()
	_ = makerErr
	if takerErr == nil {
		t.Fatal("taker accepted a non-proportional BTC amount for a partial take")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.btcTxid != "" {
		t.Fatal("BTC HTLC was funded despite the amount mismatch")
	}
}

// TestProportionalBtcNoOverflow: the partial-price ceil must use a 128-bit product. A bare uint64
// multiply overflows for realistic sizes (e.g. 1e6 sats * 1.5e15 atoms = 1.5e21 >> 2^64) and would
// silently return a tiny price, letting a partial taker pay a few sats for a large asset slice.
func TestProportionalBtcNoOverflow(t *testing.T) {
	// Hand-computed cases (small + the exact overflow scenario from the review).
	for _, c := range []struct{ wholeBtc, take, whole, want uint64 }{
		{1_000_000, 1_500_000_000_000_000, 2_100_000_000_000_000, 714286},        // ceil(1e6*1.5e15/2.1e15); bare multiply overflows
		{100_000_000, 2_100_000_000_000_000, 2_100_000_000_000_000, 100_000_000}, // whole take -> early return
		{100_000_000, 1_050_000_000_000_000, 2_100_000_000_000_000, 50_000_000},  // exactly half
		{1, 1, 100, 1},                      // ceil(1/100)=1, never 0 for a positive take
		{200_000, 50_000, 100_000, 100_000}, // the handshake test's partial
	} {
		if got := ProportionalBtc(c.wholeBtc, c.take, c.whole); got != c.want {
			t.Errorf("ProportionalBtc(%d,%d,%d) = %d, want %d (overflow?)", c.wholeBtc, c.take, c.whole, got, c.want)
		}
	}
	// Cross-check a batch (including uint64-overflowing products) against a big.Int ceil.
	for _, tc := range []struct{ b, tk, w uint64 }{
		{100_000_000, 999_999_999_999_999, 2_100_000_000_000_000},
		{4_500_000_000, 3_300_000_000, 9_000_000_000},
		{21_000_000_00000000, 1, 2_100_000_000_000_000},
	} {
		num := new(big.Int).Mul(new(big.Int).SetUint64(tc.b), new(big.Int).SetUint64(tc.tk))
		den := new(big.Int).SetUint64(tc.w)
		num.Add(num, new(big.Int).Sub(den, big.NewInt(1)))
		num.Div(num, den)
		if got := ProportionalBtc(tc.b, tc.tk, tc.w); got != num.Uint64() {
			t.Errorf("ProportionalBtc(%d,%d,%d) = %d, big.Int ceil = %s", tc.b, tc.tk, tc.w, got, num.String())
		}
	}
}

// TestSubAssetTakerRejectsBadAmount: the taker refuses terms whose BTC amount does
// not match the signed offer, before funding anything on-chain.
func TestSubAssetTakerRejectsBadAmount(t *testing.T) {
	st := &subasState{btcTip: 100}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetAtoms = uint64(100_000)
	const btcSats = uint64(200_000)

	go func() {
		_, _ = RunMakerSubAsset(MakerSubAssetParams{
			NewMakerOps:   func(hashH []byte) SubAssetMakerOps { return &fakeSubAsMakerOps{st: st} },
			AssetLNNodeID: "02fakenode",
			Crypter:       mc,
			BtcAmount:     btcSats + 5000, // != taker's expectation
			AssetAmount:   assetAtoms,
			BtcLocktime:   200,
			Timing:        XcTiming{TermsReqWait: 2 * time.Second, BtcFundWait: 2 * time.Second},
		}, net.toMaker, net.makerSend)
	}()

	_, err := RunTakerSubAsset(TakerSubAssetParams{
		Ops:         &fakeSubAsTakerOps{st: st},
		Crypter:     tc,
		BtcAmount:   btcSats,
		AssetAmount: assetAtoms,
		Timing:      XcTiming{TermsWait: 2 * time.Second, SeqLockWait: 2 * time.Second},
	}, net.takerSend, net.takerRecv)
	if err == nil {
		t.Fatal("taker must reject a maker quoting the wrong BTC amount")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.btcTxid != "" {
		t.Fatal("taker funded a BTC HTLC despite bad terms")
	}
}

// TestSubAssetTakerRefundsWhenMakerNeverPays: after the taker funds BTC and
// announces, if the maker never pays the asset (WaitAssetHeld times out), the
// taker cancels its hold and the caller can refund the BTC HTLC at T_btc.
func TestSubAssetTakerRefundsWhenMakerNeverPays(t *testing.T) {
	st := &subasState{btcTip: 100}
	tc, mc := testCrypters(t)
	net := newFakeXcNet()

	const assetAtoms = uint64(100_000)
	const btcSats = uint64(200_000)
	const tBtc = uint32(200)

	// A maker that verifies + acks but then "dies" before paying: drive the maker
	// handshake up to the ack, then stop (never call PayAssetHold to settlement).
	go func() {
		mc := mc
		recv := chanRecv(net.toMaker)
		_, _ = recvXcType(recv, mc, XcSubAsTermsRequest, 2*time.Second)
		_ = sendXc(&XcMsg{Type: XcSubAsTerms, MakerBtcClaimPub: hex.EncodeToString(mustPub(t)), BtcLocktime: tBtc, BtcAmount: btcSats, SeqAmount: assetAtoms, MakerLNNodeID: "02node"}, mc, net.makerSend)
		_, _ = recvXcType(recv, mc, XcSubAsBtcFunded, 2*time.Second)
		_ = sendXc(&XcMsg{Type: XcSubAsBtcVerified}, mc, net.makerSend)
		// ... and then the maker never pays the asset.
	}()

	takerOps := &fakeSubAsTakerOps{st: st}
	takerRes, err := RunTakerSubAsset(TakerSubAssetParams{
		Ops:         takerOps,
		Crypter:     tc,
		BtcAmount:   btcSats,
		AssetAmount: assetAtoms,
		MinBTCConf:  1,
		Timing:      XcTiming{TermsWait: 2 * time.Second, BtcConfWait: 2 * time.Second, SeqLockWait: 300 * time.Millisecond, Poll: 5 * time.Millisecond},
	}, net.takerSend, net.takerRecv)
	if err == nil {
		t.Fatal("taker must error when the maker never pays the asset")
	}
	if takerRes.BtcLeg == nil {
		t.Fatal("taker result must carry the funded BTC leg for the refund path")
	}
	if takerRes.Received {
		t.Fatal("taker must not report the asset received")
	}
	// The refund path: the caller refunds the BTC HTLC at T_btc.
	txid, rerr := RefundSubAssetBTC(takerOps, takerRes.BtcLeg, mustKey(t), takerRes.BtcLocktime, 1000, btcSats)
	if rerr != nil {
		t.Fatalf("refund: %v", rerr)
	}
	if txid == "" || !st.btcRefunded {
		t.Fatalf("BTC HTLC not refunded: txid=%q refunded=%v", txid, st.btcRefunded)
	}
}

func mustPub(t *testing.T) []byte {
	t.Helper()
	return mustKey(t).PubKey()
}
