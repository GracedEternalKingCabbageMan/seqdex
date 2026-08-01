package xchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
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

	// PayHash pays a BARE payment_hash (no BOLT11) to destNodeID and returns the
	// preimage the payee reveals on settle. Used to pay a counterparty's hold leg
	// in a pure-LN swap, where the payee has registered the hash with a hold
	// plugin and there is no invoice. Routes in this leg's asset.
	PayHash(destNodeID string, paymentHash []byte, amountMsat uint64, finalCltv uint32, paymentSecret []byte) (preimage []byte, err error)

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

	// --- REVERSE (maker-secret / taker-gated) direction: plain invoice ---
	//
	// This is the plugin-free reverse mode: the MAKER generates the preimage P,
	// so a PLAIN invoice suffices (no hold). The taker verifies + anchor-gates the
	// asset HTLC BEFORE paying; paying reveals P (payer learns the preimage), and
	// the taker claims the asset with it. See SubmarineSwap.OfferReverseMakerSecret.

	// CreateInvoice creates a plain BOLT11 whose payment_hash = SHA256(preimage)
	// for amountMsat. The node knows the preimage, so it settles normally on
	// payment (revealing P to the payer).
	CreateInvoice(preimage []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (bolt11 string, err error)

	// WaitInvoicePaid blocks until the invoice with the given label is paid (or
	// the deadline / invoice expiry), returning the received amount (msat).
	WaitInvoicePaid(label string, timeout time.Duration) (paidMsat uint64, err error)

	// WaitPaidByHash blocks until the invoice with the given payment_hash is paid,
	// polling listinvoices (the invoice may have been created out-of-band, e.g. by
	// a device with its OWN preimage, so the caller has only the hash, not a label
	// and NOT the preimage). Returns the received amount (msat).
	WaitPaidByHash(paymentHash []byte, timeout time.Duration) (paidMsat uint64, err error)
}

// --- CLN implementation over the lightning-rpc unix socket ------------------

// clnLNLeg implements LNLeg against a Core Lightning (SeqLN) node via its
// lightning-rpc unix socket (JSON-RPC 2.0). Point it at a node running
// --network=testnet4/bitcoin for real BTC.
type clnLNLeg struct {
	rpc *lnRPC
	// assetID is the 32-byte hex Sequentia asset id this leg is denominated in
	// (as displayed). "" means the policy asset (BTC / L-BTC). When set, Pay and
	// PayHash route in this asset via SeqLN's Step-1 `asset=` param, so the same
	// leg abstraction serves both the BTC leg and a Sequentia asset leg.
	assetID string
}

// clnLNLeg must satisfy LNLeg.
var _ LNLeg = (*clnLNLeg)(nil)

// NewCLNLNLeg builds a Lightning leg backed by the CLN node whose lightning-rpc
// socket is at socketPath (e.g. <lightning-dir>/<network>/lightning-rpc). It is
// denominated in the policy asset (BTC on a --network=bitcoin/testnet4 node).
func NewCLNLNLeg(socketPath string) *clnLNLeg {
	return &clnLNLeg{rpc: &lnRPC{socketPath: socketPath, timeout: 120 * time.Second}}
}

// NewCLNAssetLNLeg builds a Lightning leg denominated in a Sequentia issued
// asset (assetIDHex = the 32-byte hex asset id). Pay/PayHash route only over
// channels of that asset. This is the asset side of a pure-LN swap.
func NewCLNAssetLNLeg(socketPath, assetIDHex string) *clnLNLeg {
	return &clnLNLeg{
		rpc:     &lnRPC{socketPath: socketPath, timeout: 120 * time.Second},
		assetID: assetIDHex,
	}
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

// ReconnectPeers re-establishes the transport connection to any channel peer that
// has dropped. CLN silently tears down a peer's TCP/Noise connection when it goes
// idle (and a peer restart drops it too); a channel with a disconnected peer stays
// open on-chain but cannot route an HTLC, so a maker that re-quotes after a fill
// must reconnect first or the next leg fails to pay. It is best-effort: it lists
// peers and, for each disconnected one, issues `connect id` (CLN redials a known
// address from gossip / the peer's stored addr). Peers with no known address are
// skipped and surfaced in the returned error, but the ones that could reconnect
// still do. Returns the count reconnected. Idempotent — connecting an
// already-connected peer is a no-op — so it is safe to call before every re-quote.
func (l *clnLNLeg) ReconnectPeers() (int, error) {
	var res struct {
		Peers []struct {
			ID        string `json:"id"`
			Connected bool   `json:"connected"`
		} `json:"peers"`
	}
	if err := l.rpc.call(&res, "listpeers", map[string]interface{}{}); err != nil {
		return 0, fmt.Errorf("listpeers: %w", err)
	}
	reconnected := 0
	var firstErr error
	for _, p := range res.Peers {
		if p.Connected {
			continue
		}
		if err := l.rpc.call(nil, "connect", map[string]interface{}{"id": p.ID}); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("connect %s: %w", p.ID, err)
			}
			continue
		}
		reconnected++
	}
	return reconnected, firstErr
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
		// Only for the unannounced-channel fallback below; absent fields simply disable it.
		Payee         string `json:"payee"`
		PaymentSecret string `json:"payment_secret"`
		MinFinalCltv  uint32 `json:"min_final_cltv_expiry"`
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
	if l.assetID != "" {
		params["asset"] = l.assetID // route in this leg's Sequentia asset (Step 1)
	}
	if err := l.rpc.call(&res, "pay", params); err != nil {
		// UNANNOUNCED PAYEE FALLBACK. `pay` plans from the gossip graph plus the invoice's
		// routehints, so it cannot reach a payee whose only channels are PRIVATE -- it gives up with
		// "not reachable directly and all routehints were unusable" even when that payee is our own
		// direct peer. Every wallet-side node here is exactly that: JIT channels, unannounced.
		//
		// This broke the whole pure-LN rail. The maker registered its hold correctly, then failed to
		// pay the taker's asset invoice and cancelled the hold, so the swap ended with the taker's
		// payment refused and nothing said about why. Reproduced four times running against the same
		// taker node before the maker log made it visible.
		//
		// PayHash already solves this for bare-hash payments: asset-aware getroute, falling back to a
		// single hop over our own channel to a directly-connected payee. Paying a BOLT11 by its own
		// hash + payment_secret to its payee is the same payment, so reuse it rather than growing a
		// second router. The hash was already checked against the swap H above, and PayHash re-verifies
		// that the revealed preimage hashes to it.
		payee, secret, amt, ok := payFallbackPlan(dec.Payee, dec.PaymentSecret, dec.AmountMsat, amountMsat)
		if !ok {
			return nil, fmt.Errorf("pay: %w", err)
		}
		pre, perr := l.PayHash(payee, wantHash, amt, dec.MinFinalCltv, secret)
		if perr != nil {
			return nil, fmt.Errorf("pay: %w (direct-hop fallback: %v)", err, perr)
		}
		return pre, nil
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

// PayHash pays amountMsat to destNodeID for a BARE payment_hash, with NO BOLT11:
// the payee holds the hash via a hold plugin (there is no invoice on the payee).
// It routes in this leg's asset (getroute), sends the HTLC (sendpay), blocks
// until the payee settles (waitsendpay), and returns the revealed preimage.
//
// This is the taker's primitive for paying the maker's incoming hold leg in a
// pure-LN swap. paymentSecret is placed in the final-hop onion TLV; the payee's
// hold plugin resolves regardless of it, so an agreed-or-arbitrary value works
// (the maker may convey an expected secret over the courier). finalCltv is the
// final-hop cltv delta (0 => a small default).
// directHop builds a single-hop sendpay route over our OWN (possibly private/
// unannounced) channel to a directly-connected payee — the payee is the final hop,
// so it receives amountMsat with no routing fee. Used when getroute can't see the
// channel (private + asset-aware routing uses only the gossip graph).
func (l *clnLNLeg) directHop(destNodeID string, amountMsat uint64, finalCltv uint32) (json.RawMessage, error) {
	var res struct {
		Channels []struct {
			PeerID         string `json:"peer_id"`
			State          string `json:"state"`
			ShortChannelID string `json:"short_channel_id"`
			Direction      int    `json:"direction"`
			SpendableMsat  uint64 `json:"spendable_msat"`
		} `json:"channels"`
	}
	if err := l.rpc.call(&res, "listpeerchannels", map[string]interface{}{"id": destNodeID}); err != nil {
		return nil, fmt.Errorf("listpeerchannels: %w", err)
	}
	delay := finalCltv
	if delay == 0 {
		delay = 18
	}
	for _, c := range res.Channels {
		if c.PeerID == destNodeID && c.State == "CHANNELD_NORMAL" && c.ShortChannelID != "" && c.SpendableMsat >= amountMsat {
			hop := map[string]interface{}{
				"id": destNodeID, "channel": c.ShortChannelID, "direction": c.Direction,
				"amount_msat": amountMsat, "delay": delay, "style": "tlv",
			}
			return json.Marshal(hop)
		}
	}
	return nil, fmt.Errorf("no direct CHANNELD_NORMAL channel to %s with >= %d msat spendable", destNodeID, amountMsat)
}

func (l *clnLNLeg) PayHash(destNodeID string, paymentHash []byte, amountMsat uint64, finalCltv uint32, paymentSecret []byte) ([]byte, error) {
	phHex := hex.EncodeToString(paymentHash)

	// 1. Route to the destination in this leg's asset.
	rparams := map[string]interface{}{
		"id":          destNodeID,
		"amount_msat": amountMsat,
		"riskfactor":  10,
	}
	if finalCltv != 0 {
		rparams["cltv"] = finalCltv
	}
	if l.assetID != "" {
		rparams["asset"] = l.assetID
	}
	var route struct {
		Route []json.RawMessage `json:"route"`
	}
	rerr := l.rpc.call(&route, "getroute", rparams)
	if rerr != nil || len(route.Route) == 0 {
		// Asset-aware getroute uses only the gossip graph, so it misses an UNANNOUNCED
		// (private) channel to a directly-connected payee. Fall back to a single-hop
		// route over our own channel to the destination.
		hop, derr := l.directHop(destNodeID, amountMsat, finalCltv)
		if derr != nil {
			if rerr != nil {
				return nil, fmt.Errorf("getroute: %w (and no direct channel: %v)", rerr, derr)
			}
			return nil, fmt.Errorf("%w: no route to %s (asset %q): %v", ErrLNLegInvalid, destNodeID, l.assetID, derr)
		}
		route.Route = []json.RawMessage{hop}
	}

	// 2. Send the HTLC to the bare hash along that route.
	sparams := map[string]interface{}{
		"route":        route.Route,
		"payment_hash": phHex,
		"amount_msat":  amountMsat,
	}
	if len(paymentSecret) > 0 {
		sparams["payment_secret"] = hex.EncodeToString(paymentSecret)
	}
	if err := l.rpc.call(nil, "sendpay", sparams); err != nil {
		// "FIRST PEER NOT READY" IS A DROPPED TRANSPORT, NOT A DEAD ROUTE.
		//
		// CLN keeps the channel but tears down the peer's TCP/Noise connection when it goes idle (and a
		// peer restart drops it too). The channel is fine, the balance is fine — the first hop simply has
		// no live connection, and sendpay refuses with WIRE_TEMPORARY_CHANNEL_FAILURE.
		//
		// ReconnectPeers already existed for exactly this and was called from nowhere, so a wallet whose
		// node had been idle failed its swap outright: "pure-LN swap ended: pay hold: sendpay: rpc error
		// 204: failed: WIRE_TEMPORARY_CHANNEL_FAILURE (First peer not ready)". Reconnect and try once
		// more; if it still refuses, report the ORIGINAL error, since that is the one that describes the
		// payment rather than the recovery.
		if isPeerNotReady(err) {
			if _, rerr := l.ReconnectPeers(); rerr == nil {
				if err2 := l.rpc.call(nil, "sendpay", sparams); err2 == nil {
					goto sent
				}
			}
		}
		return nil, fmt.Errorf("sendpay: %w", err)
	}
sent:

	// 3. Block until the payee resolves it (settle => complete, cancel => fail).
	//    Use a dedicated connection whose deadline is this leg's timeout, since
	//    the hold can outlast a single RPC round-trip.
	wrpc := &lnRPC{socketPath: l.rpc.socketPath, timeout: l.rpc.timeout}
	var res struct {
		Status     string `json:"status"`
		PaymentPre string `json:"payment_preimage"`
	}
	if err := wrpc.call(&res, "waitsendpay", map[string]interface{}{"payment_hash": phHex}); err != nil {
		return nil, fmt.Errorf("waitsendpay: %w", err)
	}
	if res.Status != "complete" {
		return nil, fmt.Errorf("%w: sendpay status %q", ErrLNLegInvalid, res.Status)
	}
	pre, err := hex.DecodeString(res.PaymentPre)
	if err != nil {
		return nil, fmt.Errorf("bad preimage hex: %w", err)
	}
	if !hashEqualsPreimage(paymentHash, pre) {
		return nil, fmt.Errorf("%w: revealed preimage does not hash to paymentHash", ErrLNLegInvalid)
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
	// The hold plugin settles by revealing the preimage (the maker does not know
	// P until it learns it from the outgoing leg), so pass it explicitly.
	return l.rpc.call(nil, "holdinvoicesettle", map[string]interface{}{
		"payment_hash": hex.EncodeToString(paymentHash),
		"preimage":     hex.EncodeToString(preimage),
	})
}

func (l *clnLNLeg) CancelHold(paymentHash []byte) error {
	return l.rpc.call(nil, "holdinvoicecancel",
		map[string]interface{}{"payment_hash": hex.EncodeToString(paymentHash)})
}

// localScids lists the short_channel_ids of this node's LIVE channels, for
// forcing into an invoice's routehints. On a slow-confirming chain (testnet4)
// a PUBLIC channel can stay un-gossiped for hours (announcement needs depth 6),
// during which a payer relying on gossip cannot route even to a DIRECT peer
// ("Ran out of routes to try"). Exposing the channels explicitly makes every
// invoice self-routing regardless of announcement state.
func (l *clnLNLeg) localScids() []string {
	var res struct {
		Channels []struct {
			State string `json:"state"`
			Scid  string `json:"short_channel_id"`
		} `json:"channels"`
	}
	if err := l.rpc.call(&res, "listpeerchannels", map[string]interface{}{}); err != nil {
		return nil
	}
	var scids []string
	for _, c := range res.Channels {
		if c.State == "CHANNELD_NORMAL" && c.Scid != "" {
			scids = append(scids, c.Scid)
		}
	}
	return scids
}

func (l *clnLNLeg) CreateInvoice(preimage []byte, amountMsat uint64, cltvExpiry uint32, label, description string) (string, error) {
	params := map[string]interface{}{
		"amount_msat": amountMsat,
		"label":       label,
		"description": description,
		"preimage":    hex.EncodeToString(preimage),
	}
	if scids := l.localScids(); len(scids) > 0 {
		params["exposeprivatechannels"] = scids
	}
	if cltvExpiry != 0 {
		params["cltv"] = cltvExpiry
	}
	var res struct {
		Bolt11      string `json:"bolt11"`
		PaymentHash string `json:"payment_hash"`
	}
	if err := l.rpc.call(&res, "invoice", params); err != nil {
		return "", fmt.Errorf("invoice: %w", err)
	}
	// The created invoice's hash MUST equal SHA256(P) — otherwise the SEQ leg
	// (gated on the same H) and the LN leg would not be bound by one secret.
	want := sha256.Sum256(preimage)
	if !hexEq(res.PaymentHash, want[:]) {
		return "", fmt.Errorf("%w: created invoice hash %s != SHA256(P) %x", ErrLNLegInvalid, res.PaymentHash, want[:])
	}
	return res.Bolt11, nil
}

func (l *clnLNLeg) WaitInvoicePaid(label string, timeout time.Duration) (uint64, error) {
	// waitinvoice blocks server-side until the invoice is paid or expires; use a
	// dedicated connection whose deadline is the caller's timeout.
	rpc := &lnRPC{socketPath: l.rpc.socketPath, timeout: timeout}
	var res struct {
		Status             string `json:"status"`
		AmountReceivedMsat uint64 `json:"amount_received_msat"`
		AmountMsat         uint64 `json:"amount_msat"`
	}
	if err := rpc.call(&res, "waitinvoice", map[string]interface{}{"label": label}); err != nil {
		return 0, fmt.Errorf("waitinvoice: %w", err)
	}
	if res.Status != "paid" {
		return 0, fmt.Errorf("%w: invoice status %q (not paid)", ErrLNLegInvalid, res.Status)
	}
	if res.AmountReceivedMsat != 0 {
		return res.AmountReceivedMsat, nil
	}
	return res.AmountMsat, nil
}

// WaitPaidByHash polls listinvoices for an invoice with the given payment_hash
// until it is "paid" (or the deadline). Unlike WaitInvoicePaid it needs no label
// and never touches the preimage — used by the sub-asset external-invoice mode,
// where a DEVICE created the invoice with its own preimage and the driver only
// learns that the payment arrived (settlement is signaled by "paid", the preimage
// stays device-held).
func (l *clnLNLeg) WaitPaidByHash(paymentHash []byte, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	hashHex := hex.EncodeToString(paymentHash)
	for {
		var res struct {
			Invoices []struct {
				Status             string `json:"status"`
				AmountReceivedMsat uint64 `json:"amount_received_msat"`
				AmountMsat         uint64 `json:"amount_msat"`
			} `json:"invoices"`
		}
		err := l.rpc.call(&res, "listinvoices", map[string]interface{}{"payment_hash": hashHex})
		if err == nil {
			for _, inv := range res.Invoices {
				if inv.Status == "paid" {
					if inv.AmountReceivedMsat != 0 {
						return inv.AmountReceivedMsat, nil
					}
					return inv.AmountMsat, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("%w: invoice %s not paid within %s (last err: %v)", ErrLNLegTimeout, hashHex[:12], timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
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

// payFallbackPlan decides whether a failed `pay` can be retried as a direct-hop PayHash, and
// with what arguments. Split out from Pay so the decision is testable without a live node.
//
// It is deliberately CONSERVATIVE: without a payee and a payment_secret there is no well-formed
// payment to send, so the original `pay` error is reported unchanged rather than replaced by a
// second, more confusing one from a request that could never have worked.
//
// The amount follows the invoice: a quoted amount wins (it was already checked against the
// invoice above), and an amountless invoice falls back to the invoice's own amount. Sending 0
// would be silently wrong, so both being absent disables the fallback.
func payFallbackPlan(payee, paymentSecretHex string, invoiceMsat, quotedMsat uint64) (string, []byte, uint64, bool) {
	if payee == "" || paymentSecretHex == "" {
		return "", nil, 0, false
	}
	secret, err := hex.DecodeString(paymentSecretHex)
	if err != nil || len(secret) == 0 {
		return "", nil, 0, false
	}
	amt := quotedMsat
	if amt == 0 {
		amt = invoiceMsat
	}
	if amt == 0 {
		return "", nil, 0, false
	}
	return payee, secret, amt, true
}

// isPeerNotReady reports whether a CLN error is the "the first hop's transport is down" case rather
// than a genuine routing/liquidity failure. CLN keeps the channel and the balance in this state; only
// the peer's TCP/Noise connection is gone, which a reconnect fixes.
//
// Deliberately narrow: it only triggers a reconnect-and-retry, and retrying a payment for the wrong
// reason is exactly the kind of thing that should not be loose.
func isPeerNotReady(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "first peer not ready") ||
		(strings.Contains(m, "temporary_channel_failure") && strings.Contains(m, "peer"))
}
