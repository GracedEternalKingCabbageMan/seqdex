package xchain

import (
	"bytes"
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
// refund. Refund sets nLockTime + a non-final sequence (0xfffffffe) so CLTV
// passes; claim uses a final sequence (0xffffffff) and nLockTime 0. There is a
// single recipient output of (Amount-Fee) sats; the fee is implicit (no fee
// output, unlike Elements).
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
		txin.Sequence = 0xfffffffe // non-final: lets nLockTime/CLTV take effect
		tx.LockTime = locktime
	} else {
		txin.Sequence = 0xffffffff
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
