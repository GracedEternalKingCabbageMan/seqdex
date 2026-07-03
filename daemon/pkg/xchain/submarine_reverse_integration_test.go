package xchain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSubmarineReverseMakerSecretLive drives the plugin-free REVERSE submarine
// swap end to end (BTC over Lightning -> Sequentia asset on-chain), the mode
// where the MAKER generates P so a PLAIN invoice suffices (no hold plugin):
//
//   - the maker locks the asset HTLC (claim=taker) and issues a plain invoice on
//     H = SHA256(P);
//   - the TAKER verifies the HTLC and runs the anchor-depth gate BEFORE paying
//     (shown: it REFUSES while shallow, passes once buried >= min_anchor_depth);
//   - the taker pays the invoice over REAL testnet4 Lightning, which reveals P;
//   - the taker independently claims the asset with the learned P.
//
// Same env gating + harness as TestSubmarineRunNormalLive (skips otherwise). The
// LN payment goes node2 (taker) -> node1 (maker), so it needs node2 outbound.
func TestSubmarineReverseMakerSecretLive(t *testing.T) {
	base := os.Getenv("SEQDEX_XCHAIN_DIR")
	sock1 := os.Getenv("SEQLN_SOCK1") // maker: locks asset, issues invoice, receives BTC
	sock2 := os.Getenv("SEQLN_SOCK2") // taker: pays the invoice, claims the asset
	if base == "" || sock1 == "" || sock2 == "" {
		t.Skip("set SEQDEX_XCHAIN_DIR (regtest up) + SEQLN_SOCK1 + SEQLN_SOCK2 to run")
	}
	seqPort := envInt("SEQDEX_XCHAIN_SEQ_RPC", 18001)
	parentPort := envInt("SEQDEX_XCHAIN_PARENT_RPC", 18000)

	seqUser, seqPass := readCookie(t, filepath.Join(base, "seq"))
	parUser, parPass := readCookie(t, filepath.Join(base, "parent"))
	seqRPC := NewRPC("127.0.0.1", seqPort, seqUser, seqPass)
	parRPC := NewRPC("127.0.0.1", parentPort, parUser, parPass)
	ensureWallet(t, seqRPC)
	ensureWallet(t, parRPC)
	seq := NewChain(seqRPC, "w")
	parent := NewChain(parRPC, "w")

	if err := parent.Mine(20); err != nil {
		t.Fatalf("parent mine: %v", err)
	}
	if err := seq.Mine(110); err != nil {
		t.Fatalf("seq mine: %v", err)
	}
	waitAnchorOKLocal(t, seq)
	asset := issueAssetLocal(t, seqRPC)
	mustCall(t, "setfeeexchangerates", seqRPC.WithWallet("w").Call(nil,
		"setfeeexchangerates", map[string]interface{}{asset: 100000000}))
	if err := seq.Mine(1); err != nil {
		t.Fatalf("seq mine asset: %v", err)
	}
	t.Logf("SEQ asset issued: %s", asset)

	// The MAKER generates P (this is the maker-secret mode).
	p := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		t.Fatal(err)
	}
	hArr := sha256.Sum256(p)
	h := hArr[:]
	makerSwap := NewSubmarineSwap(seq, NewCLNLNLeg(sock1), NewHashLock(p))

	takerClaimSEQ := mustKeyLocal(t)  // taker claims the asset with P
	makerRefundSEQ := mustKeyLocal(t) // maker reclaims after CLTV
	seqHeight, _ := seq.BlockCount()
	seqLocktime := uint32(seqHeight + 50)
	label := "submarine-reverse-" + hex.EncodeToString(h[:6])

	// Maker locks the asset (claim=taker) and issues the plain invoice on H.
	offer, err := makerSwap.OfferReverseMakerSecret(ReverseMakerSecretParams{
		TakerSeqClaimPub:  takerClaimSEQ.PubKey(),
		MakerSeqRefundPub: makerRefundSEQ.PubKey(),
		SeqLocktime:       seqLocktime,
		SeqAmountCoins:    "1000",
		SeqAssetLabel:     asset,
		InvoiceMsat:       1_000_000,
		InvoiceLabel:      label,
		InvoiceDesc:       "phase2 REVERSE submarine (BTC-LN -> SEQ asset), maker-secret mode",
	})
	if err != nil {
		t.Fatalf("OfferReverseMakerSecret: %v", err)
	}
	t.Logf("maker locked asset HTLC (claim=taker) txid=%s in block %s, issued invoice on H=%x",
		offer.SeqLeg.Funded.TxID, offer.SeqBlock, offer.HashH)

	const minAnchorDepth = int64(3)

	// TAKER's job #1: verify the offered HTLC is a real Design-A HTLC claimable by
	// the taker key and paying the agreed asset amount. (VerifySEQLeg recomputes
	// the script from claim=taker, refund=maker.)
	taker := NewSubmarineSwap(seq, nil, NewHashLockFromHash(h)) // taker knows only H until it pays
	if _, err := taker.VerifySEQLeg(
		h, takerClaimSEQ.PubKey(), makerRefundSEQ.PubKey(), offer.SeqLeg.Script,
		seqLocktime, offer.SeqLeg.Funded.TxID, offer.SeqLeg.Funded.Vout, offer.SeqLeg.Funded.Amount, "", 1,
	); err != nil {
		t.Fatalf("taker verify SEQ HTLC: %v", err)
	}

	// TAKER's job #2 (THE gate): do NOT pay until the asset HTLC is anchor-buried.
	shallow, gerr := taker.VerifySeqAnchorBuried(offer.SeqBlock, minAnchorDepth)
	if gerr == nil || !errors.Is(gerr, ErrAnchorOrdering) {
		t.Fatalf("taker gate must REFUSE paying while the asset anchor is shallow (depth %d), got err=%v", shallow.AnchorDepth, gerr)
	}
	t.Logf("taker correctly REFUSES to pay while asset anchor shallow: depth=%d < min=%d", shallow.AnchorDepth, minAnchorDepth)
	buryAnchor(t, parent, seq, taker, offer.SeqBlock, minAnchorDepth)

	// TAKER pays the invoice over real testnet4 LN; paying REVEALS P.
	learnedP := takerPay(t, sock2, offer.Bolt11, offer.HashH)
	if hex.EncodeToString(learnedP) != hex.EncodeToString(p) {
		t.Fatalf("taker learned preimage %x != maker's P %x", learnedP, p)
	}
	t.Logf("taker paid the invoice and LEARNED P=%x from the settlement", learnedP)

	// Maker confirms it received the BTC-LN.
	paid, err := makerSwap.AwaitReversePayment(label, 30*time.Second)
	if err != nil {
		t.Fatalf("maker AwaitReversePayment: %v", err)
	}
	t.Logf("maker received %d msat over BTC-LN", paid)

	// TAKER independently claims the asset with the P it learned.
	if err := taker.InjectSecret(learnedP); err != nil {
		t.Fatalf("taker inject learned P: %v", err)
	}
	claimTx, err := taker.ClaimSEQLeg(offer.SeqLeg, takerClaimSEQ, 100000)
	if err != nil {
		t.Fatalf("taker claim asset: %v", err)
	}
	if err := seq.Mine(1); err != nil {
		t.Fatalf("mine taker claim: %v", err)
	}
	found, asm, err := seq.RedeemScriptSigContains(claimTx, hex.EncodeToString(learnedP))
	if err != nil {
		t.Fatalf("read preimage from taker claim: %v", err)
	}
	if !found {
		t.Fatalf("preimage not in taker claim scriptSig: %s", asm)
	}
	if c, _ := seq.Confirmations(claimTx); c < 1 {
		t.Fatalf("taker claim not confirmed (confs=%d)", c)
	}
	t.Logf("REVERSE (maker-secret) submarine swap COMPLETE: taker paid BTC-LN, learned P, claimed the asset in %s.", claimTx)
}

// takerPay pays a BOLT11 from the taker node and returns the revealed preimage.
// It mirrors clnLNLeg.Pay but runs on the taker's socket (node2), verifying the
// invoice hash first so the test never pays an unrelated invoice.
func takerPay(t *testing.T, sock, bolt11 string, wantHash []byte) []byte {
	t.Helper()
	taker := NewCLNLNLeg(sock)
	pre, err := taker.Pay(bolt11, wantHash, 0)
	if err != nil {
		t.Fatalf("taker pay: %v", err)
	}
	return pre
}
