package reorg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// rpcProbe is a ChainProbe backed by an Elements/Sequentia node's JSON-RPC. It uses
// only getrawtransaction (verbose), a node-level, wallet-independent call, so it
// needs no wallet and holds no keys. "Present" means the node still knows the tx (in
// the best chain or the mempool); a tx dropped by a Bitcoin-driven reorg returns
// present=false. The node should run with -txindex so getrawtransaction resolves a
// confirmed tx that is not in the wallet; a mempool tx resolves regardless.
type rpcProbe struct {
	url  string
	user string
	pass string
	http *http.Client
}

// NewRPCProbe builds a ChainProbe for host:port with basic-auth cookie creds.
func NewRPCProbe(host string, port int, user, pass string) ChainProbe {
	return &rpcProbe{
		url:  fmt.Sprintf("http://%s:%d", host, port),
		user: user,
		pass: pass,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// TxPresent reports whether the node still knows txid. It maps the node's "No such
// mempool or blockchain transaction" (rpc -5) to present=false (a genuine absence, a
// candidate reorg-undo); any other error is transient and propagated (never a false
// orphan).
func (p *rpcProbe) TxPresent(txid string) (bool, error) {
	var raw json.RawMessage
	err := p.call(&raw, "getrawtransaction", txid, true)
	if err == nil {
		return true, nil
	}
	if isUnknownTx(err) {
		return false, nil
	}
	return false, err
}

// TxConfirmations returns how deep txid is (0 for a mempool tx). An absent tx is an
// error here; callers use TxPresent for absence.
func (p *rpcProbe) TxConfirmations(txid string) (int64, error) {
	var res struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := p.call(&res, "getrawtransaction", txid, true); err != nil {
		return 0, err
	}
	return res.Confirmations, nil
}

// isUnknownTx reports whether err is the node's "unknown transaction" response
// (rpc -5), i.e. the tx is genuinely absent rather than a transient failure.
//
// A node WITHOUT -txindex answers the same code for a CONFIRMED tx it cannot look
// up ("No such mempool transaction. Use -txindex ..."). That is not absence: on
// such a node every settlement would read as orphaned the moment it confirmed,
// re-opening the order and retracting a real trade. That message is a
// configuration error and is reported as one, never as an orphan.
func isUnknownTx(err error) bool {
	s := err.Error()
	if strings.Contains(s, "txindex") {
		return false
	}
	return strings.Contains(s, "No such mempool or blockchain transaction")
}

func (p *rpcProbe) call(out interface{}, method string, params ...interface{}) error {
	if params == nil {
		params = []interface{}{}
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0", "id": "seqob-reorg", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(p.user, p.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
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
