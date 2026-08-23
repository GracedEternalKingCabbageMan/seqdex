package watcher

import (
	"strconv"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// rpcChain is a ChainView backed by an Elements/Sequentia node's JSON-RPC. It
// uses only node-level, wallet-independent calls (getbestblockhash,
// getblockcount, gettxout, getrawtransaction, getrawmempool, getblockhash,
// getblock), so it needs no wallet and holds no keys. It reads the LIVE tip on
// every Inspect (gettxout hits the current UTXO set), never a cache — the node's
// own Bitcoin-anchor reorg-following (first principle) is what makes a vanished
// funding UTXO observable here.
type rpcChain struct {
	url  string
	user string
	pass string
	http *http.Client
	// scanWindow bounds how far back from the tip Inspect walks to find a
	// confirmed spender (regtest fills confirm within a few blocks).
	scanWindow int64
}

// NewRPCChain builds a ChainView for host:port with cookie basic-auth.
func NewRPCChain(host string, port int, user, pass string) ChainView {
	return &rpcChain{
		url:        fmt.Sprintf("http://%s:%d", host, port),
		user:       user,
		pass:       pass,
		http:       &http.Client{Timeout: 30 * time.Second},
		scanWindow: 100,
	}
}

func (c *rpcChain) call(out interface{}, method string, params ...interface{}) error {
	if params == nil {
		params = []interface{}{}
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0", "id": "seqob-watcher", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: decode (http %d): %w", method, resp.StatusCode, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: rpc %d: %s", method, r.Error.Code, r.Error.Message)
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("%s: unmarshal: %w", method, err)
		}
	}
	return nil
}

func (c *rpcChain) Tip() (string, int64, error) {
	var hash string
	if err := c.call(&hash, "getbestblockhash"); err != nil {
		return "", 0, err
	}
	var height int64
	if err := c.call(&height, "getblockcount"); err != nil {
		return "", 0, err
	}
	return hash, height, nil
}

type txout struct {
	Confirmations int         `json:"confirmations"`
	Value         json.Number `json:"value"`
	Asset         string      `json:"asset"`
	ScriptPubKey  struct {
		Hex string `json:"hex"`
	} `json:"scriptPubKey"`
}

type rawtx struct {
	Confirmations int `json:"confirmations"`
	Vin           []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	} `json:"vin"`
	Vout []struct {
		N            uint32      `json:"n"`
		Value        json.Number `json:"value"`
		Asset        string      `json:"asset"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

// Inspect reads the on-chain reality of a covenant outpoint at the current tip.
func (c *rpcChain) Inspect(txid string, vout uint32, orderSPKHex, assetADisplay string) (CovState, error) {
	var st CovState

	// 1) Is the outpoint in the current UTXO set (mempool-aware)? gettxout
	// returns null once spent or if the outpoint does not exist.
	var utxo *txout
	if err := c.call(&utxo, "gettxout", txid, vout, true); err != nil {
		return st, fmt.Errorf("gettxout %s:%d: %w", txid, vout, err)
	}
	if utxo != nil {
		st.Unspent = true
		st.Value = coinsToAtoms(utxo.Value)
		st.AssetDisplay = utxo.Asset
		st.SPKHex = utxo.ScriptPubKey.Hex
		st.ConfirmedUTXO = utxo.Confirmations >= 1
		return st, nil
	}

	// 2) Spent or absent. Find the spender (mempool first, then a bounded window
	// of recent blocks). No spender AND not a UTXO => the outpoint does not exist
	// at the current tip (reorg / never confirmed): GHOST.
	spender, spenderTx, err := c.findSpender(txid, vout)
	if err != nil {
		return st, err
	}
	if spender == "" {
		return st, nil // st.SpenderTxid == "" => classifier decides GHOST
	}
	st.SpenderTxid = spender
	st.SpenderConfirmed = spenderTx.Confirmations >= 1

	// 3) A remainder covenant lands at index 2k+1 where k is the covenant input's
	// index in the spender. Find k, then match the candidate output against the
	// covenant program (spk == orderSPK, asset == asset_a).
	k, found := spentInputIndex(spenderTx, txid, vout)
	if found {
		ri := 2*k + 1
		for _, o := range spenderTx.Vout {
			if o.N == ri &&
				strings.EqualFold(o.ScriptPubKey.Hex, orderSPKHex) &&
				strings.EqualFold(o.Asset, assetADisplay) {
				st.Remainder = RemainderOut{Present: true, Vout: o.N, Value: coinsToAtoms(o.Value)}
				break
			}
		}
	}
	return st, nil
}

// findSpender returns the txid + decoded tx that spends (txid,vout), searching
// the mempool then recent blocks. Returns ("", nil, nil) if none is found.
func (c *rpcChain) findSpender(txid string, vout uint32) (string, *rawtx, error) {
	spends := func(cand string) (*rawtx, bool) {
		var tx rawtx
		if err := c.call(&tx, "getrawtransaction", cand, true); err != nil {
			return nil, false // ignore txs the node cannot decode
		}
		for _, in := range tx.Vin {
			if strings.EqualFold(in.TxID, txid) && in.Vout == vout {
				return &tx, true
			}
		}
		return nil, false
	}

	// Mempool.
	var mempool []string
	if err := c.call(&mempool, "getrawmempool"); err == nil {
		for _, mt := range mempool {
			if tx, ok := spends(mt); ok {
				return mt, tx, nil
			}
		}
	}

	// Recent blocks, tip-first.
	var height int64
	if err := c.call(&height, "getblockcount"); err != nil {
		return "", nil, err
	}
	for h := height; h >= 0 && h > height-c.scanWindow; h-- {
		var bh string
		if err := c.call(&bh, "getblockhash", h); err != nil {
			break
		}
		var blk struct {
			Tx []string `json:"tx"`
		}
		if err := c.call(&blk, "getblock", bh); err != nil {
			continue
		}
		for _, bt := range blk.Tx {
			if tx, ok := spends(bt); ok {
				return bt, tx, nil
			}
		}
	}
	return "", nil, nil
}

// spentInputIndex returns the index k of the spender input that spends
// (txid,vout).
func spentInputIndex(tx *rawtx, txid string, vout uint32) (uint32, bool) {
	for i, in := range tx.Vin {
		if strings.EqualFold(in.TxID, txid) && in.Vout == vout {
			return uint32(i), true
		}
	}
	return 0, false
}

// coinsToAtoms converts the node's decimal-coins value to integer atoms (1e8 per
// coin) EXACTLY, from the decimal text. Going through float64 is exact only below
// 2^53 atoms (about 9e7 units at 8 decimals); a large issuance read that way
// mis-sizes the live covenant by whole atoms, and the watcher then "reconciles" the
// order to a wrong size. A malformed or negative value reads as 0 (the classifier
// treats that as an unfundable covenant, the safe direction).
func coinsToAtoms(v json.Number) uint64 {
	s := strings.TrimSpace(v.String())
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "eE") {
		return 0
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 8 {
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseUint(whole, 10, 64)
	if err != nil || w > math.MaxUint64/100_000_000 {
		return 0
	}
	f, err := strconv.ParseUint(frac, 10, 64)
	if err != nil {
		return 0
	}
	return w*1e8 + f
}
