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
func (o *fakeSubAsMakerOps) PayAssetHold(bolt11 string, h []byte, amtMsat uint64) ([]byte, error) {
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
	return o.PayAssetHold("", h, amtMsat)
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
