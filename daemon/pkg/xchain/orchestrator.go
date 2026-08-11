package xchain

import (
	"fmt"
	"time"
)

// Direction documents which way value flows. We implement and document ONE
// direction: the secret-holder (Alice) holds BTC and wants the SEQ asset; the
// counterparty (Bob) holds the SEQ asset and wants BTC. A single preimage,
// chosen by Alice, locks both legs (Design A).
//
// Flow (with the anchor-shortened ordering as the headline rule):
//
//  1. Alice (secret holder) LOCKS the BTC leg first. claim=Bob, refund=Alice,
//     CLTV = btcLocktime (the LONGER timeout — Alice refunds last).
//  2. Once the BTC leg is confirmed at parent height Hp, Bob LOCKS the SEQ leg.
//     claim=Alice, refund=Bob, CLTV = seqLocktime (the SHORTER timeout).
//  3. ORDERING CHECK (the Sequentia value-add): the SEQ leg must land in a
//     Sequentia block whose anchorheight >= Hp, and getanchorstatus must be
//     "ok". VerifySeqLegSafe enforces this; if it fails the orchestrator
//     refuses to treat the SEQ leg as safe (ErrAnchorOrdering). Because of
//     anchoring, once this holds the SEQ leg needs only ~1 confirmation with NO
//     extra reorg buffer: if the BTC leg is later reorged, the SEQ block
//     (anchored to the now-orphaned parent height) reorgs with it, reverting
//     BOTH legs together — proven in
//     test/functional/feature_anchor_swap_consistency.py.
//  4. Alice REDEEMS the SEQ leg with the preimage (revealing it on-chain).
//  5. Bob reads the preimage off Alice's SEQ redeem and REDEEMS the BTC leg.
//
// If a counterparty stalls, the locker REFUNDS via the CLTV branch after its
// timeout (RefundPath).
type Direction struct{}

// Party holds the per-party keys for a swap. claim keys sign the IF branch on
// the leg that party receives; refund keys sign the ELSE branch on the leg that
// party funded.
type Party struct {
	// Alice = secret holder, funds BTC, receives SEQ.
	AliceClaimSEQ  *Key // Alice claims the SEQ leg
	AliceRefundBTC *Key // Alice refunds the BTC leg she funded
	// Bob = counterparty, funds SEQ, receives BTC.
	BobClaimBTC  *Key // Bob claims the BTC leg
	BobRefundSEQ *Key // Bob refunds the SEQ leg he funded
}

// Swap orchestrates a single Design-A cross-chain HTLC swap. It is written
// purely against LockPrimitive and the leg builders, so swapping in a PTLC
// primitive later requires no orchestration change.
//
// The BTC (parent / anchor-source) leg is pluggable via btcBackend: it runs
// either against an Elements-mode parent (NewSwap) or a REAL bitcoind regtest/
// testnet4 (NewSwapBitcoin). The SEQ (anchored Sequentia) leg is always
// Elements-format.
type Swap struct {
	btcBackend btcBackend // the BTC (parent / anchor-source) leg, Elements or Bitcoin
	seq        *Chain     // the Sequentia (anchored) leg

	seqLeg *ElementsLeg

	hash *HashLock // the shared hashlock (hash known to both; secret to Alice)

	// verifyNotSeenWait bounds how long VerifyBTCLeg polls out a funding tx this
	// node cannot see AT ALL yet (ErrBTCLegNotSeen, propagation lag on a fresh
	// 0-conf leg) before declaring the leg invalid. 0 = DefaultVerifyNotSeenWait.
	// Settable via SetVerifyNotSeenWait.
	verifyNotSeenWait time.Duration
}

// NewSwap wires an orchestrator to an ELEMENTS-mode parent (BTC leg) and the
// anchored Sequentia node (SEQ leg). This is the original constructor and the
// default for back-compat.
func NewSwap(btc, seq *Chain, prim *HashLock) *Swap {
	return &Swap{
		btcBackend: newElementsBTCBackend(btc, prim),
		seq:        seq,
		seqLeg:     NewElementsLeg(LegSEQ, prim),
		hash:       prim,
	}
}

// NewSwapBitcoin wires an orchestrator to a REAL bitcoind parent (BTC leg, in
// Bitcoin transaction format) and the anchored Sequentia node (SEQ leg, still
// Elements-format). This is the "real-bitcoind-leg": use it when the parent is a
// genuine bitcoind (regtest or testnet4), where the taker funds/refunds the BTC
// HTLC with a real Bitcoin signer and the maker must verify/claim it in Bitcoin
// format.
func NewSwapBitcoin(btc *BitcoinChain, seq *Chain, prim *HashLock) *Swap {
	return &Swap{
		btcBackend: newBitcoinBTCBackend(btc, prim),
		seq:        seq,
		seqLeg:     NewElementsLeg(LegSEQ, prim),
		hash:       prim,
	}
}

// NewSwapAsset wires an orchestrator whose "BTC-position" leg is a Sequentia
// ISSUED asset on the SAME Sequentia chain — the mixed same-chain shape
// (rails 7/8): one leg over Lightning, the other an on-chain HTLC on `assetID`,
// standing exactly where BTC stands in the submarine/sub-asset constructions
// (Principle 3: no structurally privileged unit). Both "sides" run against the
// one Sequentia node; verify/claim/refund are Elements-format and check the
// leg's output pays `assetID`.
func NewSwapAsset(seq *Chain, assetID string, prim *HashLock) *Swap {
	return &Swap{
		btcBackend: newElementsBTCBackendAsset(seq, prim, assetID),
		seq:        seq,
		seqLeg:     NewElementsLeg(LegSEQ, prim),
		hash:       prim,
	}
}

// SEQHTLCScript rebuilds the SEQ-leg HTLC redeem script for (claimPub, refundPub,
// locktime) under this swap's hashlock. The resume path uses it to DISCOVER a
// counterparty's funded leg on-chain when the courier announce never arrived: the
// script is fully determined by material both sides already hold at terms time,
// so a maker that gave up on a detached taker can still find - and claim - the
// leg the taker actually funded.
func (s *Swap) SEQHTLCScript(claimPub, refundPub []byte, locktime uint32) ([]byte, error) {
	return s.seqLeg.HTLCScript(claimPub, refundPub, locktime)
}

// LegLock records a funded HTLC leg.
type LegLock struct {
	Script   []byte
	Funded   *FundedHTLC
	Locktime uint32
}

// LockBTCLeg performs step 1: Alice locks the BTC leg first. Returns the funded
// leg and the parent height at which it confirmed (Hp), which the SEQ-leg
// ordering check is measured against.
func (s *Swap) LockBTCLeg(claimPub, refundPub []byte, amountCoins string, locktime uint32) (*LegLock, int64, error) {
	script, err := s.btcBackend.HTLCScript(claimPub, refundPub, locktime)
	if err != nil {
		return nil, 0, err
	}
	return s.btcBackend.LockBTCLeg(script, amountCoins, locktime)
}

// LockSEQLeg performs step 2: Bob locks the SEQ leg only after the BTC leg is
// on-chain (paper principle 7). It mines one Sequentia block and returns the
// funded leg plus the hash of the block that confirmed it (for the ordering
// check). The caller must then call VerifySeqLegSafe before treating it as safe.
// lockSEQLegBroadcast builds the SEQ HTLC on H and funds (broadcasts) it, returning the funded leg WITHOUT
// waiting for confirmation. Shared by LockSEQLeg (which then waits for the anchored block) and
// LockSEQLegNoWait (which returns immediately so the caller can relay the leg at 0-conf).
func (s *Swap) lockSEQLegBroadcast(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*LegLock, error) {
	script, err := s.seqLeg.HTLCScript(claimPub, refundPub, locktime)
	if err != nil {
		return nil, err
	}
	funded, err := s.seq.LockHTLC(script, amountCoins, assetLabel)
	if err != nil {
		return nil, err
	}
	return &LegLock{Script: script, Funded: funded, Locktime: locktime}, nil
}

// LockSEQLegNoWait funds the SEQ HTLC and returns as soon as it is broadcast (no confirmation wait, empty
// block hash). Used by the bridged-SELL taker so it can relay the funded asset leg to the maker IMMEDIATELY
// — the maker polls VerifySEQLeg until the leg confirms — instead of holding the LSP's courier session idle
// through a whole SEQ block, which is what intermittently dropped the relay (the maker then never learned
// the leg and could not claim).
func (s *Swap) LockSEQLegNoWait(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*LegLock, error) {
	return s.lockSEQLegBroadcast(claimPub, refundPub, amountCoins, assetLabel, locktime)
}

func (s *Swap) LockSEQLeg(claimPub, refundPub []byte, amountCoins, assetLabel string, locktime uint32) (*LegLock, string, error) {
	leg, err := s.lockSEQLegBroadcast(claimPub, refundPub, amountCoins, assetLabel, locktime)
	if err != nil {
		return nil, "", err
	}
	// Best-effort local confirmation for the regtest harness; on a live network
	// blocks are staker-produced (generatetoaddress is unavailable) and the SEQ leg
	// confirms naturally, so a Mine failure must NOT fail the lock.
	_ = s.seq.Mine(1) // best-effort local confirmation (regtest harness only)
	// Wait for the SEQ funding tx to confirm so we can report its anchored block.
	// On a live network this is the next staker block (seconds to a couple minutes);
	// under the regtest harness the Mine above already confirmed it. Without this the
	// caller fails with "not confirmed (no blockhash)" the instant the leg is funded.
	var blockHash string
	for i := 0; i < 90; i++ { // up to ~3 min at 2s
		if blockHash, err = s.seq.BlockHashOfTx(leg.Funded.TxID); err == nil && blockHash != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if blockHash == "" {
		// The leg IS funded on-chain at this point: return it WITH the error so
		// the caller can persist the outpoint/script/locktime and refund after
		// the CLTV instead of orphaning coins behind an unknowable redeem script.
		return leg, "", fmt.Errorf("SEQ leg %s funded but not confirmed in time: %w", leg.Funded.TxID, err)
	}
	return leg, blockHash, nil
}

// AnchorEvidence captures what VerifySeqLegSafe checked, for proof/printing.
type AnchorEvidence struct {
	BTCLegHeight     int64
	SeqBlockHash     string
	SeqBlockAnchor   int64
	AnchorStatus     string
	NodeAnchorHeight int64
	Certified        bool // the SEQ block is quorum-certified (immediately final)
	CertPresent      bool // the node reported certification (false = pre-upgrade node)
	OK               bool
}

// VerifySeqLegSafe is the anchor-shortened ordering check (step 3): it confirms
// the Sequentia block carrying the SEQ leg anchors at a height >= the BTC-leg
// height AND that the node's anchor status is "ok". Returns ErrAnchorOrdering
// (wrapped) if not — the orchestrator must NOT let the SEQ-side claimant treat
// the leg as final unless this passes.
func (s *Swap) VerifySeqLegSafe(seqBlockHash string, btcLegHeight int64) (*AnchorEvidence, error) {
	anchor, err := s.seq.BlockAnchorHeight(seqBlockHash)
	if err != nil {
		return nil, err
	}
	status, err := s.seq.GetAnchorStatus()
	if err != nil {
		return nil, err
	}
	// Quorum certification, IN ADDITION to anchoring (Alberto 2026-07-02): a leg's
	// block must be immediately-final (committee quorum), not merely anchored.
	// NOTE: this does NOT add any min-anchor-DEPTH wait — that would defeat
	// Sequentia's real-time anchoring (kept 0-conf on the anchor axis on purpose).
	// Feature-detected: a node predating the poscertified field keeps anchor-only.
	certified, certPresent, cerr := s.seq.BlockCertification(seqBlockHash)
	if cerr != nil {
		return nil, cerr
	}
	ev := &AnchorEvidence{
		BTCLegHeight:     btcLegHeight,
		SeqBlockHash:     seqBlockHash,
		SeqBlockAnchor:   anchor,
		AnchorStatus:     status.AnchorStatus,
		NodeAnchorHeight: status.AnchorHeight,
		Certified:        certified,
		CertPresent:      certPresent,
		OK:               anchor >= btcLegHeight && status.AnchorStatus == "ok" && (!certPresent || certified),
	}
	if !ev.OK {
		// Which conjunct failed decides whether waiting can ever help. The ordering
		// term reads the leg block's COMMITTED anchorheight, so it is immutable for
		// this block hash: report it as TERMINAL so the caller refunds now instead
		// of re-reading the same number until its timelock window is gone. The
		// status/certification terms genuinely flap, so they stay retryable.
		// Same verdict either way — this only distinguishes futile from transient.
		sentinel := ErrAnchorOrdering
		if anchor < btcLegHeight {
			sentinel = ErrAnchorOrderingTerminal
		}
		return ev, fmt.Errorf("%w (seq block %s anchorheight=%d, btc-leg height=%d, node anchorheight=%d, anchorstatus=%q, quorum-certified=%v)",
			sentinel, seqBlockHash, anchor, btcLegHeight, status.AnchorHeight, status.AnchorStatus, certified)
	}
	return ev, nil
}

// ClaimSEQLeg performs step 4: Alice redeems the SEQ leg with the preimage,
// revealing it on-chain. Returns the redeem txid.
func (s *Swap) ClaimSEQLeg(leg *LegLock, aliceClaim *Key, fee uint64) (string, error) {
	dest, err := s.seq.NewDestScript()
	if err != nil {
		return "", err
	}
	rawHex, err := s.seqLeg.Redeem(leg.Script, ElementsSpendInput{
		TxID:    leg.Funded.TxID,
		Vout:    leg.Funded.Vout,
		Amount:  leg.Funded.Amount,
		AssetID: leg.Funded.AssetID,
		DestSPK: dest,
		Fee:     fee,
	}, aliceClaim)
	if err != nil {
		return "", err
	}
	txid, err := s.seq.Broadcast(rawHex)
	if err != nil {
		return "", err
	}
	// Best-effort local confirmation for the regtest harness; on a live network the
	// staker confirms the already-broadcast claim above, so a Mine failure must NOT
	// fail the claim (otherwise the reveal stalls and the secret is never surfaced).
	_ = s.seq.Mine(1)
	return txid, nil
}

// sizeSeqSpendFee converts a native-sats TARGET fee into the leg asset's own atoms
// via the open-fee-market exchange rate (asset_atoms = ceil(target*1e8/rate)),
// exactly like the RFQ watcher / order-book driver / wallet, then clamps it to half
// the leg so the spend output stays positive. Emitting the flat native target as a
// raw atom count of a VALUABLE asset (e.g. GOLD, rate ~5e12) yields an absurd
// native-equivalent fee that sendrawtransaction rejects (maxfeerate); sizing keeps the
// fee's native value inside the node's relay bounds — a valuable asset correctly pays
// FEWER atoms. Falls back to the flat target when no positive rate is published (the
// native asset, where atoms == native sats) or the SEQ chain is unavailable (tests).
func (s *Swap) sizeSeqSpendFee(assetHex string, legAmount, targetNativeFee uint64) uint64 {
	fee := targetNativeFee
	if s.seq != nil {
		if rate, ok := s.seq.FeeExchangeRate(assetHex); ok && rate > 0 {
			const scale = 100_000_000                       // 1e8; matches price_server.py / exchangerates.cpp
			fee = (targetNativeFee*scale + rate - 1) / rate // ceil
			if fee == 0 {
				fee = 1
			}
		}
	}
	if maxFee := legAmount / 2; fee > maxFee {
		fee = maxFee
	}
	return fee
}

// ClaimBTCLeg performs step 5: Bob redeems the BTC leg with the now-revealed
// preimage. Returns the redeem txid. The spend is built in the BTC backend's
// transaction format (Elements or Bitcoin).
func (s *Swap) ClaimBTCLeg(leg *LegLock, bobClaim *Key, fee uint64) (string, error) {
	return s.btcBackend.ClaimBTCLeg(leg, bobClaim, fee)
}

// RefundSEQLeg builds (but does not broadcast) the SEQ-leg CLTV refund for Bob,
// at the given nLockTime. Returns the raw tx hex so callers can demonstrate the
// pre-timeout rejection and the post-timeout acceptance.
func (s *Swap) RefundSEQLeg(leg *LegLock, bobRefund *Key, nLockTime uint32, fee uint64) (string, error) {
	dest, err := s.seq.NewDestScript()
	if err != nil {
		return "", err
	}
	return s.seqLeg.Refund(leg.Script, ElementsSpendInput{
		TxID:    leg.Funded.TxID,
		Vout:    leg.Funded.Vout,
		Amount:  leg.Funded.Amount,
		AssetID: leg.Funded.AssetID,
		DestSPK: dest,
		Fee:     fee,
	}, nLockTime, bobRefund)
}

// RefundBTCLeg builds, broadcasts, and returns the txid of the BTC-leg CLTV
// refund (ELSE branch) at the given nLockTime. It is the reverse-direction
// (asset->BTC) mirror of RefundSEQLeg: the maker funds the BTC leg, so it must
// be able to reclaim that BTC after btcLocktime if the taker never funds/claims
// the SEQ leg. Delegates to the BTC backend (Elements or Bitcoin tx format).
func (s *Swap) RefundBTCLeg(leg *LegLock, makerRefund *Key, nLockTime uint32, fee uint64) (string, error) {
	return s.btcBackend.RefundBTCLeg(leg, makerRefund, nLockTime, fee)
}

// SecretHex returns the swap preimage as hex (Alice side only).
func (s *Swap) SecretHex() string { return toHex(s.hash.Secret) }
