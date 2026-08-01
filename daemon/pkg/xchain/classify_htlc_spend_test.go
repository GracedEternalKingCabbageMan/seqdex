package xchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// spendFakeRPC scripts the JSON-RPC methods ClassifyBTCHTLCSpend consults — gettxout
// (unspent test), getrawmempool / listtransactions / getblockcount+getblockhash+
// getblock (spender search), getrawtransaction / gettransaction (wallet-aware raw
// fetch) — from a table, so the classifier is exercised end-to-end WITHOUT a live
// bitcoind. A method named in errMethods returns an rpc error (to prove the
// DEFINITIVE-vs-UNCERTAIN accounting); gettransaction on an unknown txid returns the
// real "non-wallet transaction id" error, so getrawtransaction-miss => wallet fetch
// works exactly as against a real node.
type spendFakeRPC struct {
	gettxout     interface{}       // nil => spent (JSON null); non-nil object => unspent
	mempool      []string          // getrawmempool
	listtxns     interface{}       // listtransactions rows
	blockcount   int64             // getblockcount
	rawByTxid    map[string]string // getrawtransaction(txid,false) -> hex ("" => node miss)
	getTxByTxid  map[string]string // gettransaction(txid,...) -> raw hex (wallet-aware)
	confsByTxid  map[string]int    // gettransaction(txid,...) -> confirmations (spender burial depth)
	heightByTxid map[string]int64  // gettransaction(txid,...) -> blockheight (funding-anchor for the forward scan)
	// Block-scan support (findSpender step 3, getblock verbosity 2). hashByHeight maps a
	// height to its block hash for getblockhash; spendersInBlock maps a block hash to the
	// txids spending the funded outpoint (1) — the fake renders each as a verbosity-2 tx
	// with an input referencing (fundingTxid, 1). Heights absent from hashByHeight return
	// the sentinel "00" hash whose (absent) block is empty, so an unfunded height finds
	// nothing.
	hashByHeight    map[int64]string
	spendersInBlock map[string][]string
	fundingTxidRef  string          // the funding txid the synthetic block inputs reference (vout 1)
	blockchainInfo  interface{}     // getblockchaininfo (CheckClassifierNodeCapabilities)
	indexInfo       interface{}     // getindexinfo (CheckClassifierNodeCapabilities)
	errMethods      map[string]bool // methods that should return an rpc error
}

func (f spendFakeRPC) chain(t *testing.T) *BitcoinChain {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		var (
			result interface{}
			errObj interface{}
		)
		if f.errMethods[req.Method] {
			errObj = map[string]interface{}{"code": -1, "message": "simulated " + req.Method + " failure"}
		} else {
			switch req.Method {
			case "gettxout":
				result = f.gettxout // nil => null (spent); object => unspent
			case "getrawmempool":
				result = f.mempool
			case "listtransactions":
				result = f.listtxns
			case "getblockcount":
				result = f.blockcount
			case "getblockhash":
				h := int64(0)
				if len(req.Params) > 0 {
					if fv, ok := req.Params[0].(float64); ok {
						h = int64(fv)
					}
				}
				if bh, ok := f.hashByHeight[h]; ok {
					result = bh
				} else {
					result = "00" // unfunded height -> sentinel hash whose block is empty
				}
			case "getblock":
				// Verbosity-2 shape: {tx:[{txid, vin:[{txid,vout}]}]}. Each spender listed
				// for this block hash is rendered as a tx whose input spends (fundingTxid, 1).
				hash, _ := req.Params[0].(string)
				txs := []interface{}{}
				for _, sp := range f.spendersInBlock[hash] {
					txs = append(txs, map[string]interface{}{
						"txid": sp,
						"vin": []interface{}{
							map[string]interface{}{"txid": f.fundingTxidRef, "vout": 1},
						},
					})
				}
				result = map[string]interface{}{"tx": txs}
			case "getblockchaininfo":
				result = f.blockchainInfo
			case "getindexinfo":
				result = f.indexInfo
			case "getrawtransaction":
				txid, _ := req.Params[0].(string)
				result = f.rawByTxid[txid] // "" (empty) when absent -> node miss
			case "gettransaction":
				txid, _ := req.Params[0].(string)
				m := map[string]interface{}{}
				if raw, ok := f.getTxByTxid[txid]; ok {
					m["hex"] = raw
				}
				if cf, ok := f.confsByTxid[txid]; ok {
					m["confirmations"] = cf
				}
				if bh, ok := f.heightByTxid[txid]; ok {
					m["blockheight"] = bh
				}
				if len(m) > 0 {
					result = m
				} else {
					errObj = map[string]interface{}{"code": -5, "message": "Invalid or non-wallet transaction id"}
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": result, "error": errObj})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)
	return NewBitcoinChain(NewRPC(host, port, "u", "p"), "w", &chaincfg.RegressionNetParams)
}

// htlcSpendFixture builds a REAL Design-A HTLC on a known preimage and both spend
// transactions of its funded output (vout 1 of the funding tx): a CLAIM spend
// (revealing P via the IF branch) and a REFUND spend (the CLTV/ELSE branch). Both
// are built with the same BitcoinLeg used in production, so the tests classify the
// exact bytes the daemon emits.
func htlcSpendFixture(t *testing.T, locktime uint32) (redeem, secret, hashH []byte, fundingTxid, claimHex, refundHex string, amount uint64) {
	t.Helper()
	secret = bytes.Repeat([]byte{0x9a}, 32)
	hl := NewHashLock(secret) // Hash = sha256(secret)
	hashH = hl.Hash
	leg := NewBitcoinLeg(hl, &chaincfg.RegressionNetParams)
	claimKey := KeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	refundKey := KeyFromBytes(bytes.Repeat([]byte{0x22}, 32))

	var err error
	redeem, err = leg.HTLCScript(claimKey.PubKey(), refundKey.PubKey(), locktime)
	if err != nil {
		t.Fatalf("HTLCScript: %v", err)
	}
	spk, err := leg.P2SHScriptPubKey(redeem)
	if err != nil {
		t.Fatalf("P2SHScriptPubKey: %v", err)
	}
	amount = 250000

	// Funding tx: a decoy at vout 0, the HTLC output at vout 1.
	ftx := wire.NewMsgTx(2)
	prev, _ := chainhash.NewHashFromStr("77" + strings.Repeat("00", 31))
	ftx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(prev, 0), nil, nil))
	ftx.AddTxOut(wire.NewTxOut(99000, []byte{0x51}))
	ftx.AddTxOut(wire.NewTxOut(int64(amount), spk))
	fundingTxid = ftx.TxHash().String()

	in := BitcoinSpendInput{TxID: fundingTxid, Vout: 1, Amount: amount, DestPK: []byte{0x51}, Fee: 1000}
	claimHex, err = leg.BuildClaimTx(redeem, in, claimKey)
	if err != nil {
		t.Fatalf("BuildClaimTx: %v", err)
	}
	refundHex, err = leg.BuildRefundTx(redeem, in, locktime, refundKey)
	if err != nil {
		t.Fatalf("BuildRefundTx: %v", err)
	}
	return
}

// spenderTxid returns the txid (display order) of a raw Bitcoin tx hex.
func spenderTxid(t *testing.T, rawHex string) string {
	t.Helper()
	rawB, err := hex.DecodeString(rawHex)
	if err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	var msg wire.MsgTx
	if err := msg.Deserialize(bytes.NewReader(rawB)); err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	return msg.TxHash().String()
}

// TestClassifyBTCHTLCSpend_Unspent proves the DEFINITIVE unspent verdict: gettxout
// (which accounts for a mempool spend) still shows the output in the UTXO set, so
// no spender search happens and the HTLC is reported live.
func TestClassifyBTCHTLCSpend_Unspent(t *testing.T) {
	redeem, _, _, fundingTxid, _, _, _ := htlcSpendFixture(t, 500)
	f := spendFakeRPC{
		gettxout: map[string]interface{}{"confirmations": 3}, // still in the UTXO set
	}
	status, preimage, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil {
		t.Fatalf("ClassifyBTCHTLCSpend: %v", err)
	}
	if !definitive {
		t.Fatal("expected a DEFINITIVE unspent (gettxout shows the output live)")
	}
	if status != BTCHTLCSpendUnspent {
		t.Fatalf("status = %s, want UNSPENT", status)
	}
	if preimage != nil {
		t.Fatalf("unspent must carry no preimage, got %x", preimage)
	}
}

// TestClassifyBTCHTLCSpend_ViaClaim proves the CLAIM verdict + preimage extraction: the
// funded output is spent (gettxout null) by a claim tx resting in the mempool; the
// classifier finds it, reads the IF branch, and returns P with sha256(P)==H.
func TestClassifyBTCHTLCSpend_ViaClaim(t *testing.T) {
	redeem, secret, hashH, fundingTxid, claimHex, _, _ := htlcSpendFixture(t, 500)
	claimTxid := spenderTxid(t, claimHex)
	f := spendFakeRPC{
		gettxout:  nil, // spent
		mempool:   []string{claimTxid},
		rawByTxid: map[string]string{claimTxid: claimHex},
	}
	status, preimage, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil {
		t.Fatalf("ClassifyBTCHTLCSpend: %v", err)
	}
	if !definitive {
		t.Fatal("expected a DEFINITIVE claim verdict")
	}
	if status != BTCHTLCSpendClaim {
		t.Fatalf("status = %s, want SPENT_VIA_CLAIM", status)
	}
	// The preimage MUST be the real P and MUST hash to H (the invariant the LSP
	// settles the taker's hold on).
	if !bytes.Equal(preimage, secret) {
		t.Fatalf("extracted preimage %x != funded secret %x", preimage, secret)
	}
	sum := sha256.Sum256(preimage)
	if !bytes.Equal(sum[:], hashH) {
		t.Fatalf("sha256(P)=%x != H=%x", sum[:], hashH)
	}
}

// TestClassifyBTCHTLCSpend_ViaRefund proves the REFUND verdict AND the wallet-aware path
// for the LSP's OWN refund: getrawtransaction MISSES the refund (non-txindex node) but
// listtransactions surfaces its txid and gettransaction supplies the raw hex; the
// classifier reads the CLTV/ELSE branch (no preimage) and reports refund.
func TestClassifyBTCHTLCSpend_ViaRefund(t *testing.T) {
	redeem, _, _, fundingTxid, _, refundHex, _ := htlcSpendFixture(t, 500)
	refundTxid := spenderTxid(t, refundHex)
	f := spendFakeRPC{
		gettxout:    nil,        // spent
		mempool:     []string{}, // not in the mempool
		listtxns:    []map[string]interface{}{{"txid": refundTxid, "category": "receive"}},
		rawByTxid:   map[string]string{}, // getrawtransaction MISSES it (non-txindex)
		getTxByTxid: map[string]string{refundTxid: refundHex},
	}
	status, preimage, confs, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil {
		t.Fatalf("ClassifyBTCHTLCSpend: %v", err)
	}
	if !definitive {
		t.Fatal("expected a DEFINITIVE refund verdict")
	}
	if status != BTCHTLCSpendRefund {
		t.Fatalf("status = %s, want SPENT_VIA_REFUND", status)
	}
	if preimage != nil {
		t.Fatalf("a refund must carry NO preimage, got %x", preimage)
	}
	// No confirmations supplied by the wallet view => a SHALLOW (0-conf) refund. The LSP
	// keeps watching (never treats it as terminal) until it buries — see the burial test.
	if confs != 0 {
		t.Fatalf("spender confirmations = %d, want 0 (no wallet confs => shallow)", confs)
	}
}

// TestClassifyBTCHTLCSpend_RefundBurialDepth proves the classifier surfaces the spender's
// BURIAL DEPTH (confirmations) for a refund the LSP found only wallet-aware (non-txindex
// node) — the SECONDARY datum the payer bridge keys "a refund is terminal only once BURIED"
// on. gettransaction returns the LSP's OWN refund hex AND its confirmation count; the branch
// verdict is still the DEFINITIVE refund, and confs flow through unchanged.
func TestClassifyBTCHTLCSpend_RefundBurialDepth(t *testing.T) {
	redeem, _, _, fundingTxid, _, refundHex, _ := htlcSpendFixture(t, 500)
	refundTxid := spenderTxid(t, refundHex)
	f := spendFakeRPC{
		gettxout:    nil,        // spent
		mempool:     []string{}, // not in the mempool
		listtxns:    []map[string]interface{}{{"txid": refundTxid, "category": "receive"}},
		rawByTxid:   map[string]string{},                      // non-txindex: getrawtransaction MISSES it
		getTxByTxid: map[string]string{refundTxid: refundHex}, // wallet supplies the raw hex
		confsByTxid: map[string]int{refundTxid: 7},            // ...and its confirmation depth (buried)
	}
	status, _, confs, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil || !definitive {
		t.Fatalf("expected a DEFINITIVE refund with a burial depth, got definitive=%v err=%v", definitive, err)
	}
	if status != BTCHTLCSpendRefund {
		t.Fatalf("status = %s, want SPENT_VIA_REFUND", status)
	}
	if confs != 7 {
		t.Fatalf("spender confirmations = %d, want 7 (buried refund depth surfaced wallet-aware)", confs)
	}
}

// TestClassifyBTCHTLCSpend_GetTxOutError proves fail-closed on a lookup error: gettxout
// itself errors, so the fate is UNCERTAIN — never reported as UNSPENT (which would let
// the LSP release a hold the maker may already have claimed against).
func TestClassifyBTCHTLCSpend_GetTxOutError(t *testing.T) {
	redeem, _, _, fundingTxid, _, _, _ := htlcSpendFixture(t, 500)
	f := spendFakeRPC{
		errMethods: map[string]bool{"gettxout": true},
	}
	status, preimage, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if definitive {
		t.Fatal("a failed gettxout must make the result NOT definitive (fail closed)")
	}
	if status != BTCHTLCSpendUnknown {
		t.Fatalf("status = %s, want UNKNOWN on an uncertain classification", status)
	}
	if preimage != nil {
		t.Fatalf("expected no preimage on an uncertain result, got %x", preimage)
	}
	if err == nil || !strings.Contains(err.Error(), "gettxout") {
		t.Fatalf("error should name the failed lookup, got %v", err)
	}
}

// TestClassifyBTCHTLCSpend_SpentButSpenderSearchError proves the subtler fail-closed
// case: the output IS spent (gettxout null) but the spender search cannot complete —
// the mempool and block lookups error and the wallet shows nothing — so the branch is
// unresolved and the fate is UNCERTAIN (never guessed claim/refund).
func TestClassifyBTCHTLCSpend_SpentButSpenderSearchError(t *testing.T) {
	redeem, _, _, fundingTxid, _, _, _ := htlcSpendFixture(t, 500)
	f := spendFakeRPC{
		gettxout:   nil,                        // spent
		listtxns:   []map[string]interface{}{}, // wallet shows nothing
		errMethods: map[string]bool{"getrawmempool": true, "getblockcount": true},
	}
	status, _, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if definitive {
		t.Fatal("spent-but-spender-unresolvable (lookups errored) must be NOT definitive")
	}
	if status != BTCHTLCSpendUnknown {
		t.Fatalf("status = %s, want UNKNOWN", status)
	}
	if err == nil || !strings.Contains(err.Error(), "UNCERTAIN") {
		t.Fatalf("error should signal an UNCERTAIN fate, got %v", err)
	}
}

// TestClassifyBTCHTLCSpend_ClaimBeyondOldWindow is the round-8 regression: a maker CLAIM
// that CONFIRMED while the LSP was offline for the whole HTLC lifetime — deep enough that
// BOTH the old fixed 50-block window AND the spenderScanBlocks fallback window (600 blocks
// below the tip) miss it. The scan finds it ONLY because it is ANCHORED at the funding
// height (readable wallet-aware via gettransaction blockheight), which is window-
// INDEPENDENT. Missing it would strand P => the LSP could never recoup => fund-loss.
func TestClassifyBTCHTLCSpend_ClaimBeyondOldWindow(t *testing.T) {
	redeem, secret, hashH, fundingTxid, claimHex, _, _ := htlcSpendFixture(t, 500)
	claimTxid := spenderTxid(t, claimHex)
	const tip = 2000
	const fundingHeight = 1000 // funding confirmed here
	const claimHeight = 1005   // maker claimed 5 blocks later, then the LSP slept ~1000 blocks
	// Sanity: the claim is below BOTH the old 50-block window (1950..2000) and the
	// spenderScanBlocks fallback window (1400..2000) — only funding-anchoring reaches it.
	if claimHeight > tip-50 || claimHeight > tip-spenderScanBlocks {
		t.Fatalf("test fixture broken: claim height %d is not beyond both windows (tip %d)", claimHeight, tip)
	}
	f := spendFakeRPC{
		gettxout:        nil,                                           // spent
		mempool:         []string{},                                    // not in the mempool
		listtxns:        []map[string]interface{}{},                    // not the LSP's own wallet tx
		blockcount:      tip,                                           // getblockcount -> tip
		rawByTxid:       map[string]string{claimTxid: claimHex},        // branch read of the located spender
		heightByTxid:    map[string]int64{fundingTxid: fundingHeight},  // gettransaction blockheight -> anchor
		hashByHeight:    map[int64]string{claimHeight: "hashClaim"},    // the block holding the claim
		spendersInBlock: map[string][]string{"hashClaim": {claimTxid}}, // that block spends the outpoint
		fundingTxidRef:  fundingTxid,
	}
	status, preimage, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil {
		t.Fatalf("ClassifyBTCHTLCSpend: %v", err)
	}
	if !definitive || status != BTCHTLCSpendClaim {
		t.Fatalf("expected a DEFINITIVE CLAIM located via funding-anchored scan, got status=%s definitive=%v", status, definitive)
	}
	// P must be recovered (the whole point: the LSP settles the taker's hold with it).
	if !bytes.Equal(preimage, secret) {
		t.Fatalf("extracted preimage %x != funded secret %x", preimage, secret)
	}
	sum := sha256.Sum256(preimage)
	if !bytes.Equal(sum[:], hashH) {
		t.Fatalf("sha256(P)=%x != H=%x", sum[:], hashH)
	}
}

// TestClassifyBTCHTLCSpend_ConfirmedVsZeroConf proves the SPENDER confirmation count is
// reported distinctly for a 0-conf (mempool) spend vs a buried (confirmed) one — the datum
// the payer bridge keys "a claim is safe at any depth, a refund is terminal only once
// buried" on. Both scenarios classify the SAME claim; only its burial differs.
func TestClassifyBTCHTLCSpend_ConfirmedVsZeroConf(t *testing.T) {
	redeem, _, _, fundingTxid, claimHex, _, _ := htlcSpendFixture(t, 500)
	claimTxid := spenderTxid(t, claimHex)

	// (a) 0-conf: the claim rests in the mempool -> confirmations 0.
	zc := spendFakeRPC{
		gettxout:  nil,
		mempool:   []string{claimTxid},
		rawByTxid: map[string]string{claimTxid: claimHex},
	}
	status, _, confs, definitive, err := zc.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil || !definitive || status != BTCHTLCSpendClaim {
		t.Fatalf("0-conf claim: status=%s definitive=%v err=%v", status, definitive, err)
	}
	if confs != 0 {
		t.Fatalf("0-conf claim: confirmations = %d, want 0 (mempool spend)", confs)
	}

	// (b) buried: the claim confirmed in a block; the wallet reports its depth.
	const tip = 100
	const fundingHeight = 40
	const claimHeight = 45
	bc := spendFakeRPC{
		gettxout:        nil,
		mempool:         []string{},
		listtxns:        []map[string]interface{}{},
		blockcount:      tip,
		rawByTxid:       map[string]string{claimTxid: claimHex}, // branch read
		confsByTxid:     map[string]int{claimTxid: 56},          // spenderConfirmations reads this depth
		heightByTxid:    map[string]int64{fundingTxid: fundingHeight},
		hashByHeight:    map[int64]string{claimHeight: "hashClaim"},
		spendersInBlock: map[string][]string{"hashClaim": {claimTxid}},
		fundingTxidRef:  fundingTxid,
	}
	status, _, confs, definitive, err = bc.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil || !definitive || status != BTCHTLCSpendClaim {
		t.Fatalf("confirmed claim: status=%s definitive=%v err=%v", status, definitive, err)
	}
	if confs != 56 {
		t.Fatalf("confirmed claim: confirmations = %d, want 56 (buried depth surfaced)", confs)
	}
}

// TestClassifyBTCHTLCSpend_SpenderOutsideFallbackFailsClosed proves the fail-closed floor:
// when the funding height CANNOT be read (so the scan cannot anchor) and the spender sits
// below the spenderScanBlocks fallback window, the classifier reports UNCERTAIN — it never
// certifies "unspent"/a wrong branch. The caller re-drives (e.g. once the funding height
// becomes readable), never releasing a hold on an unlocated claim.
func TestClassifyBTCHTLCSpend_SpenderOutsideFallbackFailsClosed(t *testing.T) {
	redeem, _, _, fundingTxid, claimHex, _, _ := htlcSpendFixture(t, 500)
	claimTxid := spenderTxid(t, claimHex)
	const tip = 1000
	const claimHeight = 100 // below the fallback window (tip-600 = 400); funding height unknown
	f := spendFakeRPC{
		gettxout:   nil,
		mempool:    []string{},
		listtxns:   []map[string]interface{}{},
		blockcount: tip,
		rawByTxid:  map[string]string{claimTxid: claimHex},
		// heightByTxid deliberately empty -> gettransaction(funding) errors -> no anchor.
		hashByHeight:    map[int64]string{claimHeight: "hashClaim"},
		spendersInBlock: map[string][]string{"hashClaim": {claimTxid}},
		fundingTxidRef:  fundingTxid,
	}
	status, _, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if definitive {
		t.Fatal("a spent output whose spender is outside the fallback window (no anchor) must be UNCERTAIN, not definitive")
	}
	if status != BTCHTLCSpendUnknown {
		t.Fatalf("status = %s, want UNKNOWN (fail closed)", status)
	}
	if err == nil || !strings.Contains(err.Error(), "UNCERTAIN") {
		t.Fatalf("error should signal an UNCERTAIN fate, got %v", err)
	}
}

// TestClassifyBTCHTLCSpend_ConfirmedClaimBeats0ConfWalletRefund is the round-9 fund-loss
// regression: the maker's CLAIM won the T_btc race and CONFIRMED in a block, while the LSP's
// OWN refund still lingers in its wallet as a live 0-conf tx. Under the OLD (mempool -> wallet
// -> block) order the wallet refund would be returned first and the HTLC misclassified
// SPENT_VIA_REFUND — the LSP would release the taker's hold after the maker already took the
// BTC (fund-loss). The CONFIRMED-first order finds the block-confirmed claim BEFORE ever
// reaching the wallet, so the fate is SPENT_VIA_CLAIM and P is extracted to recoup.
func TestClassifyBTCHTLCSpend_ConfirmedClaimBeats0ConfWalletRefund(t *testing.T) {
	redeem, secret, hashH, fundingTxid, claimHex, refundHex, _ := htlcSpendFixture(t, 500)
	claimTxid := spenderTxid(t, claimHex)
	refundTxid := spenderTxid(t, refundHex)
	const tip = 100
	const fundingHeight = 40
	const claimHeight = 46 // the maker CLAIM confirmed here, winning the T_btc race
	f := spendFakeRPC{
		gettxout:   nil, // spent
		mempool:    []string{},
		blockcount: tip,
		// The LSP's OWN refund still sits in its wallet as a live 0-conf tx (no confs => NOT
		// conflicted): fully resolvable, so under the old wallet-before-block order it would be
		// returned as the spender and misclassified SPENT_VIA_REFUND.
		listtxns:        []map[string]interface{}{{"txid": refundTxid, "category": "receive"}},
		getTxByTxid:     map[string]string{refundTxid: refundHex},
		rawByTxid:       map[string]string{claimTxid: claimHex},       // branch read of the confirmed claim
		heightByTxid:    map[string]int64{fundingTxid: fundingHeight}, // funding-anchor for the forward scan
		hashByHeight:    map[int64]string{claimHeight: "hashClaim"},   // block holding the confirmed claim
		spendersInBlock: map[string][]string{"hashClaim": {claimTxid}},
		fundingTxidRef:  fundingTxid,
	}
	status, preimage, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if err != nil {
		t.Fatalf("ClassifyBTCHTLCSpend: %v", err)
	}
	if !definitive || status != BTCHTLCSpendClaim {
		t.Fatalf("a CONFIRMED maker claim must beat the 0-conf wallet refund: got status=%s definitive=%v", status, definitive)
	}
	// P must be the funded secret (the LSP settles the taker's hold with it to recoup).
	if !bytes.Equal(preimage, secret) {
		t.Fatalf("extracted preimage %x != funded secret %x", preimage, secret)
	}
	sum := sha256.Sum256(preimage)
	if !bytes.Equal(sum[:], hashH) {
		t.Fatalf("sha256(P)=%x != H=%x", sum[:], hashH)
	}
}

// TestClassifyBTCHTLCSpend_ConflictedWalletRefundNotReturned proves findSpender never returns
// a CONFLICTED wallet tx as the spender. The output is spent; there is no confirmed block
// spender and nothing in the mempool; the wallet lists the LSP's OWN refund but gettransaction
// reports confirmations < 0 (it was replaced/evicted, e.g. by a maker claim that won the race).
// Returning that dead refund would misclassify a claimed HTLC as refunded (fund-loss), so it is
// rejected and the fate is UNCERTAIN — the LSP fails closed and keeps watching until the winning
// spender confirms.
func TestClassifyBTCHTLCSpend_ConflictedWalletRefundNotReturned(t *testing.T) {
	redeem, _, _, fundingTxid, _, refundHex, _ := htlcSpendFixture(t, 500)
	refundTxid := spenderTxid(t, refundHex)
	const tip = 20
	f := spendFakeRPC{
		gettxout:       nil,        // spent
		mempool:        []string{}, // nothing unconfirmed
		blockcount:     tip,        // no confirmed spender in any scanned block (hashByHeight empty)
		listtxns:       []map[string]interface{}{{"txid": refundTxid, "category": "receive"}},
		rawByTxid:      map[string]string{},                      // non-txindex: getrawtransaction misses the refund
		getTxByTxid:    map[string]string{refundTxid: refundHex}, // wallet still holds the (now-dead) refund hex
		confsByTxid:    map[string]int{refundTxid: -1},           // CONFLICTED: replaced/evicted
		fundingTxidRef: fundingTxid,
	}
	status, preimage, _, definitive, err := f.chain(t).ClassifyBTCHTLCSpend(fundingTxid, 1, redeem)
	if definitive {
		t.Fatal("a CONFLICTED wallet refund must never be returned as the spender: expected UNCERTAIN")
	}
	if status != BTCHTLCSpendUnknown {
		t.Fatalf("status = %s, want UNKNOWN (fail closed on a conflicted-only wallet spender)", status)
	}
	if preimage != nil {
		t.Fatalf("expected no preimage on an uncertain result, got %x", preimage)
	}
	if err == nil || !strings.Contains(err.Error(), "UNCERTAIN") {
		t.Fatalf("error should signal an UNCERTAIN fate, got %v", err)
	}
}

// TestCheckClassifierNodeCapabilities exercises the startup node-capability check: a healthy
// node (unpruned + synced txindex) is OK, while a pruned node OR a missing/unsynced txindex is
// flagged with an error naming the deficiency (so the payer bridge can refuse to front on a node
// that cannot read a non-wallet maker claim).
func TestCheckClassifierNodeCapabilities(t *testing.T) {
	healthy := spendFakeRPC{
		blockchainInfo: map[string]interface{}{"pruned": false},
		indexInfo:      map[string]interface{}{"txindex": map[string]interface{}{"synced": true}},
	}
	caps, err := healthy.chain(t).CheckClassifierNodeCapabilities()
	if err != nil || !caps.OK() {
		t.Fatalf("healthy node should pass: caps=%+v err=%v", caps, err)
	}

	pruned := spendFakeRPC{
		blockchainInfo: map[string]interface{}{"pruned": true},
		indexInfo:      map[string]interface{}{"txindex": map[string]interface{}{"synced": true}},
	}
	if caps, err := pruned.chain(t).CheckClassifierNodeCapabilities(); err == nil || caps.OK() || !strings.Contains(err.Error(), "PRUNED") {
		t.Fatalf("pruned node should fail naming PRUNED: caps=%+v err=%v", caps, err)
	}

	noTxindex := spendFakeRPC{
		blockchainInfo: map[string]interface{}{"pruned": false},
		indexInfo:      map[string]interface{}{}, // no "txindex" entry -> disabled
	}
	if caps, err := noTxindex.chain(t).CheckClassifierNodeCapabilities(); err == nil || caps.OK() || !strings.Contains(err.Error(), "txindex") {
		t.Fatalf("no-txindex node should fail naming txindex: caps=%+v err=%v", caps, err)
	}

	unsynced := spendFakeRPC{
		blockchainInfo: map[string]interface{}{"pruned": false},
		indexInfo:      map[string]interface{}{"txindex": map[string]interface{}{"synced": false}},
	}
	if caps, err := unsynced.chain(t).CheckClassifierNodeCapabilities(); err == nil || caps.OK() || !strings.Contains(err.Error(), "synced") {
		t.Fatalf("unsynced-txindex node should fail naming synced: caps=%+v err=%v", caps, err)
	}
}

// TestClassifyBTCHTLCSpendScriptSig_Pure exercises the pure branch classifier directly
// (no chain): the claim scriptSig yields CLAIM+P (sha256(P)==H) and the refund
// scriptSig yields REFUND with no preimage. It also asserts H is read from the redeem
// script and that a foreign redeem-script binding is rejected (fail closed).
func TestClassifyBTCHTLCSpendScriptSig_Pure(t *testing.T) {
	redeem, secret, hashH, fundingTxid, claimHex, refundHex, _ := htlcSpendFixture(t, 44300)

	// H is recovered from the redeem script alone.
	gotH, err := HashFromHTLCRedeemScript(redeem)
	if err != nil {
		t.Fatalf("HashFromHTLCRedeemScript: %v", err)
	}
	if !bytes.Equal(gotH, hashH) {
		t.Fatalf("H from redeem %x != %x", gotH, hashH)
	}

	claimSig, err := htlcSpendSigScript(claimHex, fundingTxid, 1)
	if err != nil {
		t.Fatalf("claim sigScript: %v", err)
	}
	status, p, err := ClassifyBTCHTLCSpendScriptSig(claimSig, redeem)
	if err != nil {
		t.Fatalf("classify claim: %v", err)
	}
	if status != BTCHTLCSpendClaim {
		t.Fatalf("claim classified as %s", status)
	}
	sum := sha256.Sum256(p)
	if !bytes.Equal(p, secret) || !bytes.Equal(sum[:], hashH) {
		t.Fatalf("claim P mismatch: p=%x secret=%x sha256(p)=%x H=%x", p, secret, sum[:], hashH)
	}

	refundSig, err := htlcSpendSigScript(refundHex, fundingTxid, 1)
	if err != nil {
		t.Fatalf("refund sigScript: %v", err)
	}
	status, p, err = ClassifyBTCHTLCSpendScriptSig(refundSig, redeem)
	if err != nil {
		t.Fatalf("classify refund: %v", err)
	}
	if status != BTCHTLCSpendRefund {
		t.Fatalf("refund classified as %s", status)
	}
	if p != nil {
		t.Fatalf("refund must yield no preimage, got %x", p)
	}

	// P2SH binding: classifying a real spend against a DIFFERENT redeem script must
	// fail closed (the final push is not that script).
	otherRedeem, _, _, _, _, _, _ := htlcSpendFixture(t, 999)
	if _, _, err := ClassifyBTCHTLCSpendScriptSig(claimSig, otherRedeem); err == nil {
		t.Fatal("expected a P2SH-binding failure classifying against a foreign redeem script")
	}
}
