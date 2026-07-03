package xchain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// This file adds the LIGHTNING leg used by SeqLN Phase 2 submarine swaps
// (SequentiaByClaude/doc/sequentia/seqln-phase2-submarine-swaps.md). In a
// submarine swap the BTC leg is not an on-chain HTLC (btcBackend) but a Bitcoin
// Lightning payment bound to the SEQ on-chain HTLC by the SAME SHA256 preimage.
// The SEQ leg and the anchor-depth gate (VerifySeqLegSafe) are unchanged.
//
// Two directions (both Case A: Sequentia asset on-chain <-> BTC over vanilla LN):
//
//   - NORMAL (on-chain SEQ asset -> BTC-LN): the taker locks the SEQ asset HTLC
//     (claim=maker with P) and hands the maker a BOLT11 whose payment_hash = H.
//     The maker VERIFIES the SEQ leg is locked + anchor-deep, then PAYS the
//     BOLT11 -- which reveals P -- and uses P to CLAIM the SEQ leg. This needs
//     only CLN's core `pay`, so it works against a stock SeqLN/CLN node.
//
//   - REVERSE (BTC-LN -> on-chain SEQ asset): the maker issues a HOLD invoice on
//     H and locks the SEQ asset HTLC (claim=taker with P). The taker pays the
//     hold invoice (LN HTLC held), then claims the asset on-chain with P
//     (revealing it); the maker reads P off the SEQ claim and -- ONLY once that
//     claim is anchor-deep -- SETTLES the held invoice. Holding an HTLC needs the
//     `htlc_accepted` hook, provided by a holdinvoice plugin on the CLN node; the
//     hold methods below drive that plugin's RPC.
//
// The maker plays the Boltz role (non-custodial; worst case is a timelock refund).

// LNLeg abstracts the maker's Bitcoin-Lightning leg of a submarine swap. It is
// the LN analogue of btcBackend; it deliberately does NOT reuse btcBackend
// because an LN leg has no funded P2SH, txid or block height.
type LNLeg interface {
	// NodeID returns the node's Lightning id (hex pubkey).
	NodeID() (string, error)

	// --- NORMAL direction: the maker pays and thereby learns the preimage ---

	// Pay pays a BOLT11 whose payment_hash MUST equal wantHash and returns the
	// preimage the payee revealed. The caller uses that preimage to redeem the
	// SEQ leg. It fails if the invoice's hash != wantHash (so the maker never
	// pays an invoice not tied to the swap secret).
	Pay(bolt11 string, wantHash []byte, amountMsat uint64) (preimage []byte, err error)

	// --- REVERSE direction: the maker holds an invoice and settles on P ---

	// CreateHoldInvoice registers a hold invoice on paymentHash H for amountMsat
	// with the given cltv/label/description and returns the BOLT11 for the taker
	// to pay. Requires a holdinvoice plugin on the node.
	CreateHoldInvoice(paymentHash []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (bolt11 string, err error)

	// WaitHeld blocks until the taker's HTLC for paymentHash is accepted-and-held
	// (state "accepted") or the deadline. Returns the held amount (msat).
	WaitHeld(paymentHash []byte, timeout time.Duration) (heldMsat uint64, err error)

	// SettleHold resolves the held invoice with the preimage: the maker takes the
	// incoming BTC-LN. Only call once the SEQ-side reveal is anchor-deep.
	SettleHold(paymentHash, preimage []byte) error

	// CancelHold fails the held invoice back to the payer (timeout / refund path).
	CancelHold(paymentHash []byte) error
}

// --- CLN implementation over the lightning-rpc unix socket ------------------

// clnLNLeg implements LNLeg against a Core Lightning (SeqLN) node via its
// lightning-rpc unix socket (JSON-RPC 2.0). Point it at a node running
// --network=testnet4/bitcoin for real BTC.
type clnLNLeg struct {
	rpc *lnRPC
}

// clnLNLeg must satisfy LNLeg.
var _ LNLeg = (*clnLNLeg)(nil)

// NewCLNLNLeg builds a Lightning leg backed by the CLN node whose lightning-rpc
// socket is at socketPath (e.g. <lightning-dir>/<network>/lightning-rpc).
func NewCLNLNLeg(socketPath string) *clnLNLeg {
	return &clnLNLeg{rpc: &lnRPC{socketPath: socketPath, timeout: 120 * time.Second}}
}

func (l *clnLNLeg) NodeID() (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	if err := l.rpc.call(&res, "getinfo", map[string]interface{}{}); err != nil {
		return "", err
	}
	return res.ID, nil
}

func (l *clnLNLeg) Pay(bolt11 string, wantHash []byte, amountMsat uint64) ([]byte, error) {
	// Decode first so we never pay an invoice whose hash is not the swap secret,
	// and to sanity-check the amount. CLN's decoder command is `decode` (the older
	// `decodepay` is not present on all builds); it takes the invoice as `string`.
	var dec struct {
		Type        string `json:"type"`
		Valid       bool   `json:"valid"`
		PaymentHash string `json:"payment_hash"`
		AmountMsat  uint64 `json:"amount_msat"`
	}
	if err := l.rpc.call(&dec, "decode", map[string]interface{}{"string": bolt11}); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !dec.Valid {
		return nil, fmt.Errorf("%w: invoice does not decode as valid (%s)", ErrLNLegInvalid, dec.Type)
	}
	if !hexEq(dec.PaymentHash, wantHash) {
		return nil, fmt.Errorf("%w: invoice payment_hash %s != swap hash %x", ErrLNLegInvalid, dec.PaymentHash, wantHash)
	}
	if amountMsat != 0 && dec.AmountMsat != 0 && dec.AmountMsat != amountMsat {
		return nil, fmt.Errorf("%w: invoice amount %d msat != quoted %d", ErrLNLegInvalid, dec.AmountMsat, amountMsat)
	}
	var res struct {
		Status       string `json:"status"`
		PaymentPre   string `json:"payment_preimage"`
		AmountSentMs uint64 `json:"amount_sent_msat"`
	}
	// `pay` takes the invoice as `invstring` (renamed from `bolt11` to also cover
	// bolt12); pass `amount_msat` only for an amountless invoice.
	params := map[string]interface{}{"invstring": bolt11}
	if amountMsat != 0 && dec.AmountMsat == 0 {
		params["amount_msat"] = amountMsat // amountless invoice
	}
	if err := l.rpc.call(&res, "pay", params); err != nil {
		return nil, fmt.Errorf("pay: %w", err)
	}
	if res.Status != "complete" {
		return nil, fmt.Errorf("%w: pay status %q", ErrLNLegInvalid, res.Status)
	}
	pre, err := hex.DecodeString(res.PaymentPre)
	if err != nil {
		return nil, fmt.Errorf("bad preimage hex: %w", err)
	}
	// Defence in depth: the revealed preimage must actually hash to wantHash.
	if !hashEqualsPreimage(wantHash, pre) {
		return nil, fmt.Errorf("%w: revealed preimage does not hash to the swap H", ErrLNLegInvalid)
	}
	return pre, nil
}

func (l *clnLNLeg) CreateHoldInvoice(paymentHash []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (string, error) {
	// Drives a holdinvoice plugin. Method/param names follow the common CLN
	// holdinvoice plugin (create by payment_hash; the plugin owns the hold via
	// the htlc_accepted hook). Adjust to the deployed plugin if it differs.
	var res struct {
		Bolt11 string `json:"bolt11"`
	}
	params := map[string]interface{}{
		"payment_hash": hex.EncodeToString(paymentHash),
		"amount_msat":  amountMsat,
		"label":        label,
		"description":  description,
	}
	if cltvExpiry != 0 {
		params["cltv"] = cltvExpiry
	}
	if err := l.rpc.call(&res, "holdinvoice", params); err != nil {
		return "", fmt.Errorf("holdinvoice (plugin present?): %w", err)
	}
	return res.Bolt11, nil
}

func (l *clnLNLeg) WaitHeld(paymentHash []byte, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for {
		var res struct {
			State      string `json:"state"`
			AmountMsat uint64 `json:"amount_msat"`
		}
		err := l.rpc.call(&res, "holdinvoicelookup",
			map[string]interface{}{"payment_hash": hex.EncodeToString(paymentHash)})
		if err == nil && res.State == "accepted" {
			return res.AmountMsat, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("%w: hold invoice not accepted within %s (last err: %v)", ErrLNLegTimeout, timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func (l *clnLNLeg) SettleHold(paymentHash, preimage []byte) error {
	if !hashEqualsPreimage(paymentHash, preimage) {
		return fmt.Errorf("%w: preimage does not hash to the invoice payment_hash", ErrLNLegInvalid)
	}
	return l.rpc.call(nil, "holdinvoicesettle",
		map[string]interface{}{"payment_hash": hex.EncodeToString(paymentHash)})
}

func (l *clnLNLeg) CancelHold(paymentHash []byte) error {
	return l.rpc.call(nil, "holdinvoicecancel",
		map[string]interface{}{"payment_hash": hex.EncodeToString(paymentHash)})
}

// --- minimal CLN lightning-rpc client (unix socket, JSON-RPC 2.0) -----------

type lnRPC struct {
	socketPath string
	timeout    time.Duration
}

func (c *lnRPC) call(out interface{}, method string, params interface{}) error {
	conn, err := net.DialTimeout("unix", c.socketPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial lightning-rpc %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "seqob-xchain",
		"method":  method,
		"params":  params,
	}
	// Encode WITHOUT Go's default HTML escaping: it turns <, >, & into \uXXXX
	// escapes, and CLN's JSON parser rejects \u escapes in string params
	// (e.g. a description containing "->"). A JSON-RPC client must send literal
	// <>&.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return err
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("%s: write: %w", method, err)
	}

	// Read JSON objects until we get the response with our id (CLN may emit
	// notifications on the same socket).
	dec := json.NewDecoder(conn)
	for {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcErr         `json:"error"`
		}
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("%s: read response: %w", method, err)
		}
		if len(r.ID) == 0 || string(r.ID) == "null" {
			continue // a notification, not our reply
		}
		if r.Error != nil {
			return fmt.Errorf("%s: %w", method, r.Error)
		}
		if out != nil && len(r.Result) > 0 {
			if err := json.Unmarshal(r.Result, out); err != nil {
				return fmt.Errorf("%s: unmarshal result: %w", method, err)
			}
		}
		return nil
	}
}
