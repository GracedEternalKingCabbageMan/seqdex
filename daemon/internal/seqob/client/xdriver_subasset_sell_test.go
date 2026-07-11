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
}
