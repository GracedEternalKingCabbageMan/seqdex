package xchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// BitcoinLeg builds and spends a Design-A HTLC on a REAL Bitcoin chain (regtest
// or testnet4) — as opposed to ElementsLeg, which only works against an
// Elements-mode parent. It is the maker's "real-bitcoind leg": when the parent
// is a genuine bitcoind, the taker funds/refunds the BTC HTLC with a real
// Bitcoin signer (btc.js in the browser wallet) and the maker must VERIFY and
// CLAIM that HTLC in Bitcoin transaction format.
//
// The HTLC redeemScript is byte-for-byte the SAME generic Bitcoin Script the
// ElementsLeg uses (it comes from primitive.go's HashLock.LockScript). Only the
// transaction envelope differs:
//
//   - Bitcoin values are 8-byte little-endian satoshis; there are NO asset
//     commitments and NO explicit fee output (fee = sum(in) - sum(out)).
//   - Serialization is Bitcoin wire format (wire.MsgTx), parsed/built with
//     btcd, NOT go-elements.
//   - The legacy P2SH sighash is txscript.CalcSignatureHash with SIGHASH_ALL,
//     and the spend is a legacy (non-segwit) P2SH scriptSig:
//     <sig> <preimage> OP_TRUE <redeemScript>  (redeem / IF branch)
//     <sig> OP_FALSE <redeemScript>            (refund / ELSE branch)
//     — the SAME unlock-item order ElementsLeg uses (the RedeemUnlockItems /
//     RefundUnlockItems from the shared LockPrimitive), with the redeemScript
//     pushed last.
//
// Address/serialization parameters are selectable for regtest vs testnet4 via
// BitcoinChainParams (testnet4 reuses testnet3's address prefixes + "tb" HRP,
// which is all the maker needs — it never relies on the network magic or the
// genesis hash for HTLC work).
type BitcoinLeg struct {
	prim   LockPrimitive
	params *chaincfg.Params
}

// NewBitcoinLeg returns a BTC-leg builder for the given hashlock primitive and
// chain params (use BitcoinChainParams to pick regtest/testnet4).
func NewBitcoinLeg(prim LockPrimitive, params *chaincfg.Params) *BitcoinLeg {
	return &BitcoinLeg{prim: prim, params: params}
}

// Side reports which leg this builder serves (always the BTC leg).
func (l *BitcoinLeg) Side() Leg { return LegBTC }

// HTLCScript renders the redeemScript for the given pubkeys/CLTV locktime. It is
// identical to ElementsLeg.HTLCScript (same generic Bitcoin Script).
func (l *BitcoinLeg) HTLCScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	return l.prim.LockScript(claimPub, refundPub, locktime)
}

// P2SHAddress derives the base58 P2SH address for a redeemScript using this
// leg's chain params. Unlike the Elements leg (which asks the node via
// decodescript), the maker derives the Bitcoin HTLC address itself so it never
// depends on the bitcoind wallet importing the script.
func (l *BitcoinLeg) P2SHAddress(redeemScript []byte) (string, error) {
	addr, err := btcutil.NewAddressScriptHash(redeemScript, l.params)
	if err != nil {
		return "", err
	}
	return addr.EncodeAddress(), nil
}

// P2SHScriptPubKey returns the scriptPubKey (OP_HASH160 <h160> OP_EQUAL) that an
// output funding this redeemScript's P2SH must carry. Used by VerifyFundedHTLC
// to locate the HTLC output by exact scriptPubKey match.
func (l *BitcoinLeg) P2SHScriptPubKey(redeemScript []byte) ([]byte, error) {
	addr, err := btcutil.NewAddressScriptHash(redeemScript, l.params)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(addr)
}

// BitcoinSpendInput identifies the HTLC output being spent on the BTC leg.
type BitcoinSpendInput struct {
	TxID   string // funding txid (big-endian display order)
	Vout   uint32
	Amount uint64 // value of the HTLC output, in satoshis
	DestPK []byte // scriptPubKey of the redeem/refund destination
	Fee    uint64 // fee in satoshis (subtracted from Amount; no explicit fee output)
}

// BuildClaimTx builds a signed redeem (IF-branch) spend revealing the preimage,
// paying (Amount-Fee) to DestPK. This is the maker's BTC-leg claim. Returns the
// serialized Bitcoin tx hex for sendrawtransaction.
func (l *BitcoinLeg) BuildClaimTx(redeemScript []byte, in BitcoinSpendInput, key *Key) (string, error) {
	tx, err := l.buildSpendTx(in, 0, false)
	if err != nil {
		return "", err
	}
	sig, err := l.sign(tx, redeemScript, key)
	if err != nil {
		return "", err
	}
	items, err := l.prim.RedeemUnlockItems(sig)
	if err != nil {
		return "", err
	}
	return l.finalize(tx, redeemScript, items)
}

// BuildRefundTx builds a signed refund (ELSE-branch) spend, valid once nLockTime
// reaches the CLTV locktime. The maker does NOT refund BTC in the implemented
// direction (the taker refunds its own BTC leg), but this is provided for
// symmetry with ElementsLeg.Refund and for completeness.
func (l *BitcoinLeg) BuildRefundTx(redeemScript []byte, in BitcoinSpendInput, locktime uint32, key *Key) (string, error) {
	tx, err := l.buildSpendTx(in, locktime, true)
	if err != nil {
		return "", err
	}
	sig, err := l.sign(tx, redeemScript, key)
	if err != nil {
		return "", err
	}
	items, err := l.prim.RefundUnlockItems(sig)
	if err != nil {
		return "", err
	}
	return l.finalize(tx, redeemScript, items)
}

// buildSpendTx builds the unsigned Bitcoin spend skeleton shared by claim and
// refund. Refund sets nLockTime + an RBF-signalling non-final sequence
// (0xfffffffd) so CLTV passes AND the refund is fee-BUMPABLE (BIP125): the LSP
// payer leg-bridge must be able to RBF-replace a stalled 0-conf refund with a
// higher fee so it confirms inside the taker hold's remaining life (else the
// hold fails back to the taker and a maker claim takes the LSP's BTC HTLC). Any
// sequence <= 0xfffffffe is non-final (CLTV takes effect); <= 0xfffffffd ALSO
// opts into RBF, a strict superset that never weakens the timelock. Claim uses a
// final sequence (0xffffffff) and nLockTime 0. There is a single recipient output
// of (Amount-Fee) sats; the fee is implicit (no fee output, unlike Elements).
func (l *BitcoinLeg) buildSpendTx(in BitcoinSpendInput, locktime uint32, refund bool) (*wire.MsgTx, error) {
	if in.Fee >= in.Amount {
		return nil, fmt.Errorf("xchain/btc: fee %d >= amount %d", in.Fee, in.Amount)
	}
	h, err := chainhash.NewHashFromStr(in.TxID)
	if err != nil {
		return nil, fmt.Errorf("xchain/btc: bad txid %q: %w", in.TxID, err)
	}
	tx := wire.NewMsgTx(2)
	txin := wire.NewTxIn(wire.NewOutPoint(h, in.Vout), nil, nil)
	if refund {
		txin.Sequence = 0xfffffffd // non-final (lets nLockTime/CLTV take effect) AND RBF-signalling (BIP125) so the refund is fee-bumpable
		tx.LockTime = locktime
	} else {
		// The claim is the time-critical spend (it must confirm before the
		// counterparty's CLTV); with nLockTime 0 an RBF-signalling sequence weakens
		// nothing and lets a stuck claim be fee-bumped.
		txin.Sequence = 0xfffffffd
	}
	tx.AddTxIn(txin)
	tx.AddTxOut(wire.NewTxOut(int64(in.Amount-in.Fee), in.DestPK))
	return tx, nil
}

// sign computes the legacy SIGHASH_ALL sighash over the redeemScript (Bitcoin
// CalcSignatureHash) and returns DER(sig) || SIGHASH_ALL — the same low-S ECDSA
// construction the Elements leg uses, only with the Bitcoin sighash function.
func (l *BitcoinLeg) sign(tx *wire.MsgTx, redeemScript []byte, key *Key) ([]byte, error) {
	sh, err := txscript.CalcSignatureHash(redeemScript, txscript.SigHashAll, tx, 0)
	if err != nil {
		return nil, err
	}
	return append(key.SignDER(sh), byte(txscript.SigHashAll)), nil
}

// finalize assembles the legacy P2SH scriptSig (<unlock items...> <redeemScript>)
// and returns the serialized Bitcoin tx hex. An empty unlock item is encoded as
// OP_0 (selects the ELSE/refund branch), matching ElementsLeg.finalize.
func (l *BitcoinLeg) finalize(tx *wire.MsgTx, redeemScript []byte, items [][]byte) (string, error) {
	b := txscript.NewScriptBuilder()
	for _, it := range items {
		if len(it) == 0 {
			b.AddOp(txscript.OP_0)
		} else {
			b.AddData(it)
		}
	}
	b.AddData(redeemScript)
	sigScript, err := b.Script()
	if err != nil {
		return "", err
	}
	tx.TxIn[0].SignatureScript = sigScript

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

// FundedBTCHTLC is the maker's verified view of a real-Bitcoin HTLC funding.
type FundedBTCHTLC struct {
	TxID          string
	Vout          uint32
	Amount        uint64 // satoshis
	Confirmations int
	ScriptPubKey  []byte // the P2SH scriptPubKey the output paid
}

// VerifyFundedHTLC validates the taker's funded BTC-leg HTLC against the agreed
// parameters, working entirely in Bitcoin transaction format.
//
// Given the raw funding tx (Bitcoin-serialized hex, e.g. from getrawtransaction)
// and the agreed HTLC params (hashH, makerClaimPub, takerRefundPub, btcLocktime,
// expected amount in sats), it:
//
//  1. recomputes the canonical Design-A redeemScript from (hashH, claim, refund,
//     locktime) — identical to what btc.js builds;
//  2. computes that script's P2SH scriptPubKey under this leg's chain params;
//  3. parses the funding tx as a wire.MsgTx (Bitcoin format, NOT go-elements)
//     and locates the output paying that exact scriptPubKey;
//  4. checks the output value equals the agreed amount and that the funding tx
//     has at least minConf confirmations.
//
// On success it returns the funded outpoint + value so the maker can later
// BuildClaimTx against it.
func (l *BitcoinLeg) VerifyFundedHTLC(
	rawTxHex string,
	hashH, makerClaimPub, takerRefundPub []byte,
	btcLocktime uint32,
	wantAmount uint64,
	confirmations, minConf int,
) (*FundedBTCHTLC, error) {
	// 1) recompute the redeemScript.
	wantScript, err := l.HTLCScript(makerClaimPub, takerRefundPub, btcLocktime)
	if err != nil {
		return nil, err
	}
	// The redeemScript embeds H (OP_SHA256 <H> ...); a script byte match implies
	// the hashlock matches, but assert H explicitly for a clear error.
	if !scriptEmbedsHash(wantScript, hashH) {
		return nil, fmt.Errorf("%w: recomputed script does not embed H=%x", ErrBTCLegInvalid, hashH)
	}

	// 2) expected P2SH scriptPubKey.
	wantSPK, err := l.P2SHScriptPubKey(wantScript)
	if err != nil {
		return nil, err
	}

	// 3) parse the funding tx in Bitcoin format and find the HTLC output.
	raw, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return nil, fmt.Errorf("%w: bad raw tx hex: %v", ErrBTCLegInvalid, err)
	}
	var msg wire.MsgTx
	if err := msg.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("%w: parse bitcoin tx: %v", ErrBTCLegInvalid, err)
	}
	txid := msg.TxHash().String()

	var (
		vout  = -1
		value uint64
	)
	for i, out := range msg.TxOut {
		if bytes.Equal(out.PkScript, wantSPK) {
			vout = i
			value = uint64(out.Value)
			break
		}
	}
	if vout < 0 {
		return nil, fmt.Errorf("%w: tx %s has no output paying the HTLC P2SH", ErrBTCLegInvalid, txid)
	}

	// 4) value + confirmation checks.
	if value != wantAmount {
		return nil, fmt.Errorf("%w: btc-leg value %d != quoted %d", ErrBTCLegInvalid, value, wantAmount)
	}
	if confirmations < minConf {
		return nil, fmt.Errorf("%w: btc-leg has %d confirmations, need %d", ErrBTCLegUnconfirmed, confirmations, minConf)
	}

	return &FundedBTCHTLC{
		TxID:          txid,
		Vout:          uint32(vout),
		Amount:        value,
		Confirmations: confirmations,
		ScriptPubKey:  wantSPK,
	}, nil
}

// findOutputBySPK parses a raw Bitcoin tx hex and returns the (vout, value sats)
// of the first output whose scriptPubKey equals wantSPK. Used by the in-process
// harness to locate a just-funded HTLC output.
func findOutputBySPK(rawTxHex string, wantSPK []byte) (uint32, uint64, error) {
	raw, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return 0, 0, fmt.Errorf("bad raw tx hex: %w", err)
	}
	var msg wire.MsgTx
	if err := msg.Deserialize(bytes.NewReader(raw)); err != nil {
		return 0, 0, fmt.Errorf("parse bitcoin tx: %w", err)
	}
	for i, out := range msg.TxOut {
		if bytes.Equal(out.PkScript, wantSPK) {
			return uint32(i), uint64(out.Value), nil
		}
	}
	return 0, 0, fmt.Errorf("no output matches the target scriptPubKey")
}

// LocateHTLCOutputByScript parses a raw Bitcoin funding tx (Bitcoin-serialized
// hex, e.g. from getrawtransaction) and returns the (vout, value sats) of the
// output paying the P2SH of redeemScript under params. It is the
// redeem-script-only analog of VerifyFundedHTLC (which reconstructs the script
// from H + the claim/refund pubkeys): the REFUND path already holds the EXACT
// redeemScript it funded, so it locates its own HTLC output on-chain by that
// script alone — without needing H or the counterparty pubkey. Used to
// verify-not-trust a refund's outpoint + amount before spending the CLTV/ELSE
// (refund) branch back to the funder.
func LocateHTLCOutputByScript(params *chaincfg.Params, rawTxHex string, redeemScript []byte) (uint32, uint64, error) {
	addr, err := btcutil.NewAddressScriptHash(redeemScript, params)
	if err != nil {
		return 0, 0, fmt.Errorf("derive HTLC P2SH: %w", err)
	}
	spk, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return 0, 0, fmt.Errorf("HTLC scriptPubKey: %w", err)
	}
	return findOutputBySPK(rawTxHex, spk)
}

// scriptEmbedsHash reports whether redeemScript contains an exact 32-byte push
// of wantHash (the OP_SHA256 <H> in a Design-A HTLC). It is a cheap sanity check
// layered on top of the full byte-for-byte script comparison the caller does.
func scriptEmbedsHash(redeemScript, wantHash []byte) bool {
	if len(wantHash) != 32 {
		return false
	}
	tok := txscript.MakeScriptTokenizer(0, redeemScript)
	for tok.Next() {
		if bytes.Equal(tok.Data(), wantHash) {
			return true
		}
	}
	return false
}

// --- chain params selection -------------------------------------------------

// testNet4Params is a btcd Params for Bitcoin testnet4. btcd v0.23.4 predates
// testnet4, so we clone testnet3 (whose address prefixes + bech32 HRP testnet4
// shares) and only override the bits that differ — the network magic and the
// human name. The maker never depends on the genesis hash or magic for HTLC
// work (it derives the P2SH address from prefixes and parses txs structurally),
// so reusing testnet3's GenesisHash is harmless here.
func testNet4Params() *chaincfg.Params {
	p := chaincfg.TestNet3Params // value copy
	p.Name = "testnet4"
	p.Net = 0x283f161c // testnet4 message-start magic (0x1c161f28 little-endian)
	return &p
}

// --- HTLC-spend classification (authoritative on-chain fate) --------------

// BTCHTLCSpendStatus is the AUTHORITATIVE on-chain fate of a funded Design-A HTLC
// output, learned by inspecting the transaction that spent it. It is the signal
// the LSP payer leg-bridge keys its recoup-vs-refund decision on — REPLACING any
// persisted intent flag, which is racy across the persist/broadcast window
// (crash-before-broadcast => stale "refunded=true"; RPC-error-after-broadcast =>
// stale "refunded=false"). The chain's single spend is idempotent and cannot lie.
type BTCHTLCSpendStatus int

const (
	// BTCHTLCSpendUnknown is the FAIL-CLOSED sentinel: the fate could not be
	// DEFINITIVELY determined (a lookup errored, or a spend was seen but could not
	// be classified). It is ONLY ever paired with definitive=false; the caller must
	// treat it as neither unspent/claim/refund and re-drive later, never guess.
	BTCHTLCSpendUnknown BTCHTLCSpendStatus = iota
	// BTCHTLCSpendUnspent: the funded output is still in the UTXO set (no spend,
	// including the mempool). The HTLC is live; the higher-level P-public / T_btc
	// decision applies (P public => recoup; T_btc passed with no P => refund).
	BTCHTLCSpendUnspent
	// BTCHTLCSpendClaim: the output was spent via the CLAIM / IF (hashlock) branch —
	// the spending input revealed a preimage P with sha256(P)==H. The maker got
	// paid; the LSP MUST recoup the taker's hold. P is returned so the LSP can
	// settle the held LN.
	BTCHTLCSpendClaim
	// BTCHTLCSpendRefund: the output was spent via the REFUND / ELSE (CLTV) branch —
	// no preimage; the funder (the LSP) reclaimed its BTC. The maker is UNPAID, so
	// the taker's hold MUST be released, never captured.
	BTCHTLCSpendRefund
)

// String renders the status as the stable label the seqob-cli / LSP contract uses.
func (s BTCHTLCSpendStatus) String() string {
	switch s {
	case BTCHTLCSpendUnspent:
		return "UNSPENT"
	case BTCHTLCSpendClaim:
		return "SPENT_VIA_CLAIM"
	case BTCHTLCSpendRefund:
		return "SPENT_VIA_REFUND"
	default:
		return "UNKNOWN"
	}
}

// HashFromHTLCRedeemScript extracts the 32-byte hashlock image H from a Design-A
// HTLC redeemScript — the push immediately following OP_SHA256 (see primitive.go's
// LockScript: OP_IF OP_SHA256 <H> OP_EQUALVERIFY ...). It is the SINGLE source of
// truth the spend classifier keys claim-detection on, so H never has to be threaded
// separately alongside the redeemScript the caller already holds. A script that is
// not a Design-A HTLC (no OP_SHA256 <32-byte push>) is rejected rather than guessed.
func HashFromHTLCRedeemScript(redeemScript []byte) ([]byte, error) {
	tok := txscript.MakeScriptTokenizer(0, redeemScript)
	afterSha256 := false
	for tok.Next() {
		if afterSha256 {
			d := tok.Data()
			if len(d) != 32 {
				return nil, fmt.Errorf("expected a 32-byte hash after OP_SHA256, got a %d-byte item", len(d))
			}
			return append([]byte(nil), d...), nil
		}
		if tok.Opcode() == txscript.OP_SHA256 {
			afterSha256 = true
		}
	}
	if err := tok.Err(); err != nil {
		return nil, fmt.Errorf("parse redeem script: %w", err)
	}
	return nil, fmt.Errorf("redeem script has no OP_SHA256 <32-byte H> (not a Design-A HTLC)")
}

// ClassifyBTCHTLCSpendScriptSig classifies a Design-A HTLC spend PURELY from the
// spending input's legacy-P2SH scriptSig, with NO chain access — the pure primitive
// under BitcoinChain.ClassifyBTCHTLCSpend. The two branches serialize as:
//
//	CLAIM:  <sig> <preimage> OP_TRUE  <redeemScript>   (IF / hashlock)
//	REFUND: <sig>            OP_FALSE <redeemScript>   (ELSE / CLTV)
//
// (the redeemScript is always the FINAL push in a P2SH spend). It returns:
//
//   - (BTCHTLCSpendClaim, P, nil)   — a pushed item's sha256 == H, so the hashlock/IF
//     branch was taken; P is that preimage (sha256(P)==H holds by construction).
//   - (BTCHTLCSpendRefund, nil, nil)— no preimage present AND the branch selector is
//     script-FALSE, so the CLTV/ELSE branch was taken.
//   - (BTCHTLCSpendUnknown, nil, err)— unclassifiable: the final push is not our exact
//     redeemScript (P2SH binding failed), or no preimage AND a non-false selector
//     (a claim whose preimage we failed to recognize) — FAIL CLOSED, never guess.
//
// Preimage-presence is the AUTHORITATIVE claim signal: only the IF branch's
// OP_SHA256 <H> OP_EQUALVERIFY can put a valid preimage on-chain, so any accepted
// (mempool or confirmed) spend carrying one took the claim branch. The P2SH binding
// (final push == our exact redeemScript) proves the spend is of OUR HTLC, so a
// no-preimage spend is OUR refund branch — cross-checked against a false selector so
// a preimage we somehow missed is reported UNCERTAIN, not mislabeled REFUND.
func ClassifyBTCHTLCSpendScriptSig(sigScript, redeemScript []byte) (BTCHTLCSpendStatus, []byte, error) {
	hashH, err := HashFromHTLCRedeemScript(redeemScript)
	if err != nil {
		return BTCHTLCSpendUnknown, nil, fmt.Errorf("classify: %w", err)
	}

	type sigItem struct {
		op     byte
		data   []byte
		isData bool
	}
	var items []sigItem
	tok := txscript.MakeScriptTokenizer(0, sigScript)
	for tok.Next() {
		d := tok.Data()
		items = append(items, sigItem{op: tok.Opcode(), data: append([]byte(nil), d...), isData: d != nil})
	}
	if err := tok.Err(); err != nil {
		return BTCHTLCSpendUnknown, nil, fmt.Errorf("classify: parse scriptSig: %w", err)
	}
	// The minimal Design-A spend is <selector> <redeemScript> (refund) — two items;
	// anything shorter cannot be a P2SH HTLC spend.
	if len(items) < 2 {
		return BTCHTLCSpendUnknown, nil, fmt.Errorf("classify: scriptSig has %d item(s), too few for a Design-A P2SH spend", len(items))
	}
	// P2SH BINDING: the final push must reveal EXACTLY our redeemScript. That proves
	// this input spends OUR HTLC via its P2SH, so the branch read below is a branch of
	// OUR script and the H we extracted governs it.
	last := items[len(items)-1]
	if !last.isData || !bytes.Equal(last.data, redeemScript) {
		return BTCHTLCSpendUnknown, nil, fmt.Errorf("classify: spender's final push is not our redeem script (P2SH binding failed)")
	}
	// CLAIM (authoritative): any pushed item whose sha256 == H is the revealed
	// preimage; return it so the LSP can settle the taker's hold (recoup).
	for _, it := range items {
		if !it.isData || len(it.data) == 0 {
			continue
		}
		if len(it.data) != PreimageLen {
			continue // a sha256 match of another length cannot settle a Lightning hold
		}
		h := sha256.Sum256(it.data)
		if bytes.Equal(h[:], hashH) {
			return BTCHTLCSpendClaim, append([]byte(nil), it.data...), nil
		}
	}
	// No preimage. In a valid spend that reveals our redeemScript this can ONLY be
	// the ELSE/refund branch. Confirm the selector (the item before the redeemScript
	// push) is script-FALSE, so a claim whose preimage we failed to recognize is
	// reported UNCERTAIN rather than mislabeled REFUND (fail closed).
	sel := items[len(items)-2]
	if isScriptSelectorFalse(sel.op, sel.data, sel.isData) {
		return BTCHTLCSpendRefund, nil, nil
	}
	return BTCHTLCSpendUnknown, nil, fmt.Errorf("classify: spend reveals our redeem script but shows neither a valid preimage (H) nor a false refund-selector (unclassifiable, fail closed)")
}

// isScriptSelectorFalse reports whether an HTLC branch-selector stack item is
// script-FALSE (selects the ELSE / CLTV refund branch). OP_0 and an empty or
// all-zero data push (incl. a trailing 0x80 negative-zero sign byte) are false;
// OP_1..OP_16 / OP_1NEGATE and any non-zero data push are true.
func isScriptSelectorFalse(op byte, data []byte, isData bool) bool {
	if !isData {
		// A bare opcode: only OP_0 (== OP_FALSE) pushes an empty (false) value.
		return op == txscript.OP_0
	}
	if len(data) == 0 {
		return true
	}
	for i, b := range data {
		if b == 0x00 {
			continue
		}
		if b == 0x80 && i == len(data)-1 {
			continue // trailing sign byte of negative zero
		}
		return false
	}
	return true
}

// spenderSpendsOutpoint parses a raw Bitcoin tx hex and reports whether ANY of its
// inputs spends (fundingTxid, vout). Used to confirm a candidate tx is the spender
// of a funded HTLC output. A fetch/parse failure returns false (the candidate is
// skipped) — the classifier's fail-closed accounting lives in ClassifyBTCHTLCSpend.
func spenderSpendsOutpoint(rawTxHex, fundingTxid string, vout uint32) bool {
	fundHash, err := chainhash.NewHashFromStr(fundingTxid)
	if err != nil {
		return false
	}
	rawB, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return false
	}
	var msg wire.MsgTx
	if err := msg.Deserialize(bytes.NewReader(rawB)); err != nil {
		return false
	}
	for _, in := range msg.TxIn {
		if in.PreviousOutPoint.Hash.IsEqual(fundHash) && in.PreviousOutPoint.Index == vout {
			return true
		}
	}
	return false
}

// htlcSpendSigScript parses a raw Bitcoin spender tx and returns the scriptSig of
// the input that spends (fundingTxid, vout) — the witness the branch classifier
// reads. Errors if the tx does not actually spend that outpoint (so the caller
// fails closed rather than classifying an unrelated input).
func htlcSpendSigScript(rawTxHex, fundingTxid string, vout uint32) ([]byte, error) {
	fundHash, err := chainhash.NewHashFromStr(fundingTxid)
	if err != nil {
		return nil, fmt.Errorf("bad funding txid %q: %w", fundingTxid, err)
	}
	rawB, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return nil, fmt.Errorf("bad spender raw hex: %w", err)
	}
	var msg wire.MsgTx
	if err := msg.Deserialize(bytes.NewReader(rawB)); err != nil {
		return nil, fmt.Errorf("parse spender tx: %w", err)
	}
	for _, in := range msg.TxIn {
		if in.PreviousOutPoint.Hash.IsEqual(fundHash) && in.PreviousOutPoint.Index == vout {
			return in.SignatureScript, nil
		}
	}
	return nil, fmt.Errorf("spender tx does not spend %s:%d", fundingTxid, vout)
}

// BitcoinChainParams selects btcd chain params for the BTC parent leg.
//
//	"regtest"            -> RegressionNetParams (bcrt HRP, 0x6f/0xc4 prefixes)
//	"testnet4"           -> testNet4Params (tb HRP, testnet3 prefixes)
//	"testnet3"/"testnet" -> TestNet3Params
//	"mainnet"            -> MainNetParams
//
// regtest and testnet4 produce IDENTICAL transaction serialization (the task's
// "regtest == testnet4 format"); only address prefixes differ, and the maker's
// claim never needs to round-trip an address (it spends by outpoint).
func BitcoinChainParams(name string) (*chaincfg.Params, error) {
	switch name {
	case "", "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "testnet4":
		return testNet4Params(), nil
	case "testnet3", "testnet":
		return &chaincfg.TestNet3Params, nil
	case "mainnet", "main":
		return &chaincfg.MainNetParams, nil
	default:
		return nil, fmt.Errorf("xchain/btc: unknown chain %q (want regtest|testnet4|testnet3|mainnet)", name)
	}
}
