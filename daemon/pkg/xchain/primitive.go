// Package xchain implements the SeqDEX cross-chain atomic-swap mechanism
// (Phase 5, milestone 1): a "Design A" single-secret HTLC swap between a
// Bitcoin-script leg (a Bitcoin/Elements parent chain) and a Sequentia-asset
// leg (the anchored Sequentia chain).
//
// Design A in one line: both legs are locked to the SAME hashlock H =
// sha256(secret) and the SAME secret. Redeeming either leg reveals the
// preimage on-chain, which the counterparty then uses to redeem the other —
// that single shared secret is what makes the swap atomic.
//
// The locking script is identical on both chains (it is plain Bitcoin Script,
// which elementsd evaluates unchanged in Elements mode):
//
//	OP_IF
//	    OP_SHA256 <H> OP_EQUALVERIFY <claim_pub> OP_CHECKSIG     # redeem branch
//	OP_ELSE
//	    <locktime> OP_CHECKLOCKTIMEVERIFY OP_DROP <refund_pub> OP_CHECKSIG  # refund
//	OP_ENDIF
//
// paid to P2SH. The redeem (IF) branch reveals the preimage; the refund (ELSE)
// branch spends back to the locker after nLockTime reaches <locktime> (CLTV).
//
// The Sequentia value-add — the whole point of this milestone — is the
// "anchor-shortened ordering" enforced by the Swap orchestrator (see
// orchestrator.go): lock the BTC leg first, then require the Sequentia leg to
// land in a Sequentia block whose anchorheight >= the BTC-leg's block height
// (paper Principle 7). Because of Bitcoin anchoring, if the BTC leg is later
// reorged the SEQ leg reorgs with it, so the SEQ leg needs only ~1 confirmation
// with NO extra reorg-protection buffer.
package xchain

import (
	"bytes"
	"crypto/sha256"

	"github.com/btcsuite/btcd/txscript"
)

// Leg identifies which chain a primitive is operating on. The two legs differ
// only in transaction serialization / sighash (Bitcoin-script vs Elements);
// the lock script itself is byte-for-byte identical, so a LockPrimitive is
// leg-agnostic and the leg-specific work lives in the *Leg builders.
type Leg int

const (
	// LegBTC is the Bitcoin-script leg (the parent / anchor-source chain).
	LegBTC Leg = iota
	// LegSEQ is the Sequentia-asset leg (the anchored chain).
	LegSEQ
)

func (l Leg) String() string {
	if l == LegBTC {
		return "BTC"
	}
	return "SEQ"
}

// LockPrimitive abstracts the cryptographic lock used by a swap leg. Today the
// only implementation is HashLock (a SHA256 hashlock HTLC), but the swap
// orchestration in orchestrator.go is written purely against this interface so
// a PTLC / adaptor-signature primitive can be slotted in later without touching
// the orchestration.
//
// A primitive produces three artefacts, all leg-agnostic raw scripts:
//   - LockScript: the redeemScript funded by both parties (-> P2SH address).
//   - RedeemUnlockItems: the data items that satisfy the "claim/IF" branch
//     given a signature over the spend (e.g. <sig> <preimage> 1 for a
//     hashlock; just <sig> for a PTLC). The leg builder is responsible for
//     wrapping these into a scriptSig/witness together with the redeemScript.
//   - RefundUnlockItems: the data items that satisfy the "refund/ELSE" branch
//     (e.g. <sig> 0 for the CLTV refund).
//
// Splitting "what unlocks the branch" (here) from "how it is serialized into a
// scriptSig vs a witness" (the leg) is what keeps the abstraction clean across
// btcd and go-elements.
type LockPrimitive interface {
	// Kind is a short human label ("hashlock", "ptlc", ...).
	Kind() string

	// LockScript builds the redeemScript for the given claim/refund pubkeys
	// and CLTV refund locktime.
	LockScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error)

	// RedeemUnlockItems returns the stack items (excluding the trailing
	// redeemScript push, which the leg adds) for the claim/IF branch, given a
	// signature already produced over the spend's sighash and any
	// primitive-specific secret material.
	RedeemUnlockItems(sig []byte) ([][]byte, error)

	// RefundUnlockItems returns the stack items (excluding the trailing
	// redeemScript push) for the refund/ELSE branch, given a signature over
	// the spend's sighash.
	RefundUnlockItems(sig []byte) ([][]byte, error)
}

// HashLock is the Design-A SHA256 hashlock primitive. It holds the hash H of
// the swap secret; only the redeeming party needs Secret set, and only when
// actually building a redeem spend (RedeemUnlockItems checks it).
type HashLock struct {
	Hash   []byte // 32-byte sha256(secret); the public part of the lock.
	Secret []byte // 32-byte preimage; required to build a redeem spend.
}

// NewHashLock builds a HashLock from a known preimage (the secret-holder side).
func NewHashLock(secret []byte) *HashLock {
	h := sha256.Sum256(secret)
	return &HashLock{Hash: h[:], Secret: append([]byte(nil), secret...)}
}

// NewHashLockFromHash builds a HashLock from only the hash (the counterparty
// side, before the secret is revealed on-chain).
func NewHashLockFromHash(hash []byte) *HashLock {
	return &HashLock{Hash: append([]byte(nil), hash...)}
}

func (h *HashLock) Kind() string { return "hashlock" }

// LockScript renders the Design-A HTLC redeemScript:
//
//	OP_IF OP_SIZE <32> OP_EQUALVERIFY OP_SHA256 <H> OP_EQUALVERIFY <claimPub> OP_CHECKSIG
//	OP_ELSE <T> OP_CHECKLOCKTIMEVERIFY OP_DROP <refundPub> OP_CHECKSIG OP_ENDIF
//
// The OP_SIZE guard pins the preimage to the 32 bytes Lightning settles. Without
// it the claim branch accepted ANY length whose sha256 matched, so in a flow where
// the counterparty picks P and reveals it on-chain before the other side settles a
// Lightning hold with it, a 33-byte P claimed the chain leg and could never settle
// the hold. The JS/WASM builders (SWK lwk_wasm, the LSP) emit the same bytes;
// LegacyLockScript is the pre-guard form that verifiers still accept from older
// counterparties.
func (h *HashLock) LockScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)
	b.AddOp(txscript.OP_SIZE).AddInt64(32).AddOp(txscript.OP_EQUALVERIFY)
	b.AddOp(txscript.OP_SHA256).AddData(h.Hash).AddOp(txscript.OP_EQUALVERIFY)
	b.AddData(claimPub).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ELSE)
	b.AddInt64(int64(locktime)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP)
	b.AddData(refundPub).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ENDIF)
	return b.Script()
}

// LegacyLockScript is the pre-guard Design-A script (no OP_SIZE), identical to
// contrib/sequentia/swap-demo.py's htlc_script(). Verifiers accept it from a
// counterparty that has not upgraded; nothing new is built with it.
func (h *HashLock) LegacyLockScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)
	b.AddOp(txscript.OP_SHA256).AddData(h.Hash).AddOp(txscript.OP_EQUALVERIFY)
	b.AddData(claimPub).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ELSE)
	b.AddInt64(int64(locktime)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP)
	b.AddData(refundPub).AddOp(txscript.OP_CHECKSIG)
	b.AddOp(txscript.OP_ENDIF)
	return b.Script()
}

// legacyLocker is implemented by primitives that still accept an older script form.
type legacyLocker interface {
	LegacyLockScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error)
}

// ScriptVariants returns every script form a verifier accepts for these
// parameters, current form first.
func ScriptVariants(p LockPrimitive, claimPub, refundPub []byte, locktime uint32) ([][]byte, error) {
	cur, err := p.LockScript(claimPub, refundPub, locktime)
	if err != nil {
		return nil, err
	}
	out := [][]byte{cur}
	if ll, ok := p.(legacyLocker); ok {
		if old, err := ll.LegacyLockScript(claimPub, refundPub, locktime); err == nil {
			out = append(out, old)
		}
	}
	return out, nil
}

// MatchScript returns provided if it equals one of the accepted variants, else nil.
func MatchScript(p LockPrimitive, provided, claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	vs, err := ScriptVariants(p, claimPub, refundPub, locktime)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		if bytes.Equal(v, provided) {
			return v, nil
		}
	}
	return nil, nil
}

// RedeemUnlockItems: <sig> <preimage> OP_TRUE — selects the IF branch and
// satisfies SHA256(<preimage>)==H plus the claim-key CHECKSIG. (The leg appends
// the redeemScript push afterwards.)
func (h *HashLock) RedeemUnlockItems(sig []byte) ([][]byte, error) {
	if len(h.Secret) == 0 {
		return nil, errNoSecret
	}
	// OP_TRUE is encoded as a 1-byte 0x01 push here; the leg serializers turn
	// an empty slice into OP_0 and {0x01} into the minimal true value, matching
	// CScript([... , 1, script]) in the demo.
	return [][]byte{sig, h.Secret, {0x01}}, nil
}

// RefundUnlockItems: <sig> OP_FALSE — selects the ELSE (refund) branch.
func (h *HashLock) RefundUnlockItems(sig []byte) ([][]byte, error) {
	return [][]byte{sig, {}}, nil
}

// compile-time assertion.
var _ LockPrimitive = (*HashLock)(nil)
