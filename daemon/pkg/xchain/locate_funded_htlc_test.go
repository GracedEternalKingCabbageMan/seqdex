package xchain

import (
	"bytes"
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

// fakeRPC scripts the JSON-RPC methods LocateFundedHTLC consults —
// listunspent, listtransactions, scantxoutset, getrawtransaction,
// gettransaction — from a table, so the dry-locate paths are exercised WITHOUT a
// live bitcoind. Any method named in errMethods returns an rpc error (to prove
// the DEFINITIVE-vs-UNCERTAIN accounting); gettransaction on an unknown txid
// returns the real "non-wallet transaction id" error.
type fakeRPC struct {
	listunspent      interface{}
	listtransactions interface{}
	scantxoutset     interface{}
	rawByTxid        map[string]string // getrawtransaction(txid,false) -> hex ("" => node miss)
	getTxByTxid      map[string]string // gettransaction(txid,...) -> raw hex (wallet-aware)
	errMethods       map[string]bool   // methods that should return an rpc error
}

func (f fakeRPC) chain(t *testing.T) *BitcoinChain {
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
			case "listunspent":
				result = f.listunspent
			case "listtransactions":
				result = f.listtransactions
			case "scantxoutset":
				result = f.scantxoutset
			case "getrawtransaction":
				txid, _ := req.Params[0].(string)
				result = f.rawByTxid[txid] // "" (null) when absent -> node miss
			case "gettransaction":
				txid, _ := req.Params[0].(string)
				if raw, ok := f.getTxByTxid[txid]; ok {
					result = map[string]interface{}{"hex": raw}
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

// htlcFundingFixture builds a real HTLC redeemScript + a raw Bitcoin funding tx
// that pays its P2SH at vout 1 (a decoy output sits at vout 0, so the test also
// proves the correct vout is located). Returns the script, its P2SH address, the
// funding txid (display order), the raw tx hex, and the funded amount.
func htlcFundingFixture(t *testing.T) (script []byte, p2sh, txid, rawHex string, amount uint64) {
	t.Helper()
	prim := NewHashLockFromHash(bytes.Repeat([]byte{0xab}, 32))
	leg := NewBitcoinLeg(prim, &chaincfg.RegressionNetParams)
	claim := KeyFromBytes(bytes.Repeat([]byte{0x11}, 32)).PubKey()
	refund := KeyFromBytes(bytes.Repeat([]byte{0x22}, 32)).PubKey()
	var err error
	script, err = leg.HTLCScript(claim, refund, 500)
	if err != nil {
		t.Fatalf("HTLCScript: %v", err)
	}
	spk, err := leg.P2SHScriptPubKey(script)
	if err != nil {
		t.Fatalf("P2SHScriptPubKey: %v", err)
	}
	p2sh, err = leg.P2SHAddress(script)
	if err != nil {
		t.Fatalf("P2SHAddress: %v", err)
	}
	amount = 250000

	tx := wire.NewMsgTx(2)
	prev, _ := chainhash.NewHashFromStr("11" + strings.Repeat("00", 31))
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(prev, 0), nil, nil))
	tx.AddTxOut(wire.NewTxOut(99000, []byte{0x51})) // decoy at vout 0 (OP_TRUE spk)
	tx.AddTxOut(wire.NewTxOut(int64(amount), spk))  // HTLC at vout 1
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize funding tx: %v", err)
	}
	rawHex = hex.EncodeToString(buf.Bytes())
	txid = tx.TxHash().String()
	return
}

// TestLocateFundedHTLC_ViaWalletTxOnly proves the wallet-aware path: a
// self-broadcast, still-in-mempool funding that getrawtransaction MISSES on a
// non-txindex / mempool-dropped node is STILL found and bound via the wallet's
// gettransaction. listtransactions surfaces the funding txid; getrawtransaction
// returns null; gettransaction supplies the raw hex; the exact vout/value binds.
func TestLocateFundedHTLC_ViaWalletTxOnly(t *testing.T) {
	script, p2sh, txid, rawHex, amount := htlcFundingFixture(t)
	f := fakeRPC{
		listtransactions: []map[string]interface{}{
			{"txid": "dead" + strings.Repeat("00", 30), "address": "someOtherAddr", "category": "send"}, // unrelated
			{"txid": txid, "address": p2sh, "category": "send"},                                         // the funding
		},
		listunspent:  []map[string]interface{}{},
		scantxoutset: map[string]interface{}{"success": true},
		rawByTxid:    map[string]string{}, // getrawtransaction MISSES it (non-txindex / evicted)
		getTxByTxid:  map[string]string{txid: rawHex},
	}
	loc, definitive, err := f.chain(t).LocateFundedHTLC(script)
	if err != nil {
		t.Fatalf("LocateFundedHTLC: %v", err)
	}
	if !definitive {
		t.Fatal("expected a definitive result (a funding was found)")
	}
	if loc == nil {
		t.Fatal("expected the wallet-tx funding to be located via gettransaction, got nil")
	}
	if loc.TxID != txid || loc.Vout != 1 || loc.Amount != amount {
		t.Fatalf("located %+v, want txid=%s vout=1 amount=%d", loc, txid, amount)
	}
}

// TestLocateFundedHTLC_ViaListUnspent proves the listunspent lookup: the wallet's
// own unspent output at the HTLC P2SH is surfaced (minconf 0) and bound by exact
// redeemScript to the right outpoint.
func TestLocateFundedHTLC_ViaListUnspent(t *testing.T) {
	script, p2sh, txid, rawHex, amount := htlcFundingFixture(t)
	f := fakeRPC{
		listunspent: []map[string]interface{}{
			{"txid": txid, "vout": 1, "address": p2sh, "amount": 0.0025},
		},
		listtransactions: []map[string]interface{}{},
		scantxoutset:     map[string]interface{}{"success": true},
		rawByTxid:        map[string]string{txid: rawHex},
	}
	loc, definitive, err := f.chain(t).LocateFundedHTLC(script)
	if err != nil {
		t.Fatalf("LocateFundedHTLC: %v", err)
	}
	if !definitive {
		t.Fatal("expected a definitive result (a funding was found)")
	}
	if loc == nil {
		t.Fatal("expected the listunspent funding to be located, got nil")
	}
	if loc.TxID != txid || loc.Vout != 1 || loc.Amount != amount {
		t.Fatalf("located %+v, want txid=%s vout=1 amount=%d", loc, txid, amount)
	}
}

// TestLocateFundedHTLC_ViaScanTxoutSet proves the belt-and-suspenders confirmed
// path: with nothing in the wallet, a confirmed HTLC output is found via
// scantxoutset and bound by the same exact-script locate.
func TestLocateFundedHTLC_ViaScanTxoutSet(t *testing.T) {
	script, _, txid, rawHex, amount := htlcFundingFixture(t)
	f := fakeRPC{
		listunspent:      []map[string]interface{}{},
		listtransactions: []map[string]interface{}{},
		scantxoutset: map[string]interface{}{
			"success":  true,
			"unspents": []map[string]interface{}{{"txid": txid, "vout": 1, "amount": 0.0025}},
		},
		rawByTxid: map[string]string{txid: rawHex},
	}
	loc, definitive, err := f.chain(t).LocateFundedHTLC(script)
	if err != nil {
		t.Fatalf("LocateFundedHTLC: %v", err)
	}
	if !definitive {
		t.Fatal("expected a definitive result (a funding was found)")
	}
	if loc == nil {
		t.Fatal("expected the scantxoutset funding to be located, got nil")
	}
	if loc.TxID != txid || loc.Vout != 1 || loc.Amount != amount {
		t.Fatalf("located %+v, want txid=%s vout=1 amount=%d", loc, txid, amount)
	}
}

// TestLocateFundedHTLC_DefinitiveNotFunded proves the all-clear certain negative:
// every lookup runs cleanly and none shows a funding -> (nil, true, nil), so the
// caller may safely broadcast.
func TestLocateFundedHTLC_DefinitiveNotFunded(t *testing.T) {
	script, _, _, _, _ := htlcFundingFixture(t)
	f := fakeRPC{
		listunspent: []map[string]interface{}{},
		listtransactions: []map[string]interface{}{
			{"txid": "aa" + strings.Repeat("00", 31), "address": "unrelated", "category": "send"},
		},
		scantxoutset: map[string]interface{}{"success": true, "unspents": []map[string]interface{}{}},
	}
	loc, definitive, err := f.chain(t).LocateFundedHTLC(script)
	if err != nil {
		t.Fatalf("LocateFundedHTLC: %v", err)
	}
	if !definitive {
		t.Fatal("expected DEFINITIVE not-funded (all lookups succeeded, none funded)")
	}
	if loc != nil {
		t.Fatalf("expected nil (nothing funded), got %+v", loc)
	}
}

// TestLocateFundedHTLC_LookupErrorNotDefinitive proves fail-closed on uncertainty:
// a single failing lookup (scantxoutset here) makes the result NOT definitive even
// though nothing was found, so the caller must NOT treat it as "not funded" and
// must NOT broadcast. err names the failed lookup.
func TestLocateFundedHTLC_LookupErrorNotDefinitive(t *testing.T) {
	script, _, _, _, _ := htlcFundingFixture(t)
	f := fakeRPC{
		listunspent:      []map[string]interface{}{},
		listtransactions: []map[string]interface{}{},
		errMethods:       map[string]bool{"scantxoutset": true},
	}
	loc, definitive, err := f.chain(t).LocateFundedHTLC(script)
	if loc != nil {
		t.Fatalf("expected nil outpoint on an uncertain locate, got %+v", loc)
	}
	if definitive {
		t.Fatal("a failed lookup must make the result NOT definitive (fail closed)")
	}
	if err == nil {
		t.Fatal("expected a non-nil error naming the failed lookup when not definitive")
	}
	if !strings.Contains(err.Error(), "scantxoutset") {
		t.Fatalf("error should name the failed lookup, got %v", err)
	}
}

// TestLocateFundedHTLC_CandidateUnfetchableNotDefinitive proves a subtler
// fail-closed case: a lookup ties a txid to the P2SH but neither getrawtransaction
// NOR gettransaction can produce its raw tx, so the funding cannot be confirmed —
// that is UNCERTAIN (never "not funded"), because the lookup asserted a payment to
// the P2SH that we simply cannot see.
func TestLocateFundedHTLC_CandidateUnfetchableNotDefinitive(t *testing.T) {
	script, p2sh, txid, _, _ := htlcFundingFixture(t)
	f := fakeRPC{
		listunspent: []map[string]interface{}{},
		listtransactions: []map[string]interface{}{
			{"txid": txid, "address": p2sh, "category": "send"}, // asserts a payment to the P2SH...
		},
		scantxoutset: map[string]interface{}{"success": true},
		rawByTxid:    map[string]string{}, // ...but getrawtransaction misses it...
		getTxByTxid:  map[string]string{}, // ...and gettransaction cannot supply it either.
	}
	loc, definitive, err := f.chain(t).LocateFundedHTLC(script)
	if loc != nil {
		t.Fatalf("expected nil outpoint, got %+v", loc)
	}
	if definitive {
		t.Fatal("an unfetchable P2SH-tied candidate must make the result NOT definitive")
	}
	if err == nil || !strings.Contains(err.Error(), txid) {
		t.Fatalf("error should name the unbindable candidate txid, got %v", err)
	}
}
