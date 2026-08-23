package main

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

// resumeTimelockSession recovers the three directions whose only recovery is a
// timelock refund or a claim with a persisted preimage:
//
//   - "submarine-reverse": the maker locked the asset for a taker that never paid.
//     After T_seq the asset is refunded with the identity-derived key.
//   - "subasset-sell":     the maker locked BTC (or the quote asset) for a taker that
//     never paid the asset hold. After T_btc the leg is refunded.
//   - "subasset":          the maker paid the asset over Lightning and learned P but
//     died before claiming the taker's on-chain leg. Claimed with P and the
//     identity-derived claim key at any time before T_btc.
//
// Each pass is idempotent: a leg already spent is recorded as terminal, a timelock
// not yet reached is reported and left for the next pass (every maker start runs
// this), and nothing is broadcast twice (the engine's spend cache re-sends the same
// transaction).
func resumeTimelockSession(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain, spendFee uint64, dir, name string) {
	switch st.Direction {
	case "submarine-reverse":
		resumeSubmarineReverseRefund(st, seqChain, spendFee, dir, name)
	case "subasset-sell":
		resumeSubAssetSellRefund(st, btcChain, seqChain, spendFee, dir, name)
	case "subasset":
		resumeSubAssetClaim(st, btcChain, seqChain, spendFee, dir, name)
	}
}

func legFromState(txid string, vout uint32, amount uint64, asset, scriptHex string, locktime uint32) (*xchain.LegLock, error) {
	script, err := hex.DecodeString(scriptHex)
	if err != nil || len(script) == 0 {
		return nil, fmt.Errorf("bad leg script hex")
	}
	return &xchain.LegLock{
		Script:   script,
		Locktime: locktime,
		Funded:   &xchain.FundedHTLC{TxID: txid, Vout: vout, Amount: amount, AssetID: asset},
	}, nil
}

func resumeSubmarineReverseRefund(st *xmakerSessionState, seqChain *xchain.Chain, spendFee uint64, dir, name string) {
	if st.SeqLegTxid == "" || st.SeqLegScriptHex == "" || st.SeqRefundPrivHex == "" {
		fmt.Printf("%s: reverse submarine session with no locked asset leg / refund key; nothing to recover\n", name)
		return
	}
	unspent, err := seqChain.OutputUnspent(st.SeqLegTxid, st.SeqLegVout)
	if err != nil {
		fmt.Printf("%s: gettxout: %v\n", name, err)
		return
	}
	if !unspent {
		fmt.Printf("%s: asset leg %s:%d already spent (the taker paid and claimed, or a refund landed); marking terminal\n", name, st.SeqLegTxid, st.SeqLegVout)
		st.Settled = true
		writeSessionState(dir, st.SessionID, st)
		return
	}
	tip, err := seqChain.BlockCount()
	if err != nil {
		fmt.Printf("%s: seq tip: %v\n", name, err)
		return
	}
	if int64(st.SeqLocktime) > tip {
		fmt.Printf("%s: asset leg refundable at T_seq=%d (tip %d); waiting for a later pass\n", name, st.SeqLocktime, tip)
		return
	}
	keyBytes, err := hex.DecodeString(st.SeqRefundPrivHex)
	if err != nil || len(keyBytes) != 32 {
		fmt.Printf("%s: bad refund key hex\n", name)
		return
	}
	leg, err := legFromState(st.SeqLegTxid, st.SeqLegVout, st.SeqLegAmount, st.SeqLegAsset, st.SeqLegScriptHex, st.SeqLocktime)
	if err != nil {
		fmt.Printf("%s: %v\n", name, err)
		return
	}
	hashH, _ := hex.DecodeString(st.HashHex)
	sub := xchain.NewSubmarineSwap(seqChain, nil, xchain.NewHashLockFromHash(hashH))
	raw, err := sub.RefundReverseSEQ(leg, xchain.KeyFromBytes(keyBytes), st.SeqLocktime, xcSafeFeeAtoms(spendFee, leg.Funded.Amount))
	if err != nil {
		fmt.Printf("%s: build asset refund: %v\n", name, err)
		return
	}
	txid, err := seqChain.Broadcast(raw)
	if err != nil {
		fmt.Printf("%s: broadcast asset refund: %v\n", name, err)
		return
	}
	st.SeqRefundTx = txid
	writeSessionState(dir, st.SessionID, st)
	fmt.Printf("%s: ASSET LEG REFUNDED in %s (T_seq=%d passed, taker never paid)\n", name, txid, st.SeqLocktime)
}

// onchainSwapFor builds the on-chain-leg swap for a sub-asset rail from the
// persisted backend name, binding it to the session's H.
func onchainSwapFor(backend string, hashH []byte, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain) (*xchain.Swap, func() (int64, error), error) {
	lock := xchain.NewHashLockFromHash(hashH)
	switch {
	case backend == "" || backend == "bitcoin":
		if btcChain == nil {
			return nil, nil, fmt.Errorf("no bitcoind configured (-btc-rpc) for the BTC leg")
		}
		return xchain.NewSwapBitcoin(btcChain, nil, lock), btcChain.BlockCount, nil
	case strings.HasPrefix(backend, "seq:"):
		if seqChain == nil {
			return nil, nil, fmt.Errorf("no Sequentia node configured (-xseq-rpc) for the quote-asset leg")
		}
		return xchain.NewSwapAsset(seqChain, strings.TrimPrefix(backend, "seq:"), lock), seqChain.BlockCount, nil
	}
	return nil, nil, fmt.Errorf("unknown on-chain backend %q", backend)
}

func resumeSubAssetSellRefund(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain, spendFee uint64, dir, name string) {
	if st.BtcLegTxid == "" || st.BtcLegScriptHex == "" || st.BtcRefundPrivHex == "" {
		fmt.Printf("%s: sub-asset SELL session with no funded on-chain leg / refund key; nothing to recover\n", name)
		return
	}
	hashH, _ := hex.DecodeString(st.HashHex)
	swap, tipFn, err := onchainSwapFor(st.BtcLegBackend, hashH, btcChain, seqChain)
	if err != nil {
		fmt.Printf("%s: %v\n", name, err)
		return
	}
	tip, err := tipFn()
	if err != nil {
		fmt.Printf("%s: on-chain tip: %v\n", name, err)
		return
	}
	if int64(st.BtcLocktime) > tip {
		fmt.Printf("%s: on-chain leg refundable at T_btc=%d (tip %d); waiting for a later pass\n", name, st.BtcLocktime, tip)
		return
	}
	keyBytes, err := hex.DecodeString(st.BtcRefundPrivHex)
	if err != nil || len(keyBytes) != 32 {
		fmt.Printf("%s: bad refund key hex\n", name)
		return
	}
	leg, err := legFromState(st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount, "", st.BtcLegScriptHex, st.BtcLocktime)
	if err != nil {
		fmt.Printf("%s: %v\n", name, err)
		return
	}
	txid, err := swap.RefundBTCLeg(leg, xchain.KeyFromBytes(keyBytes), st.BtcLocktime, xcSafeFeeAtoms(spendFee, leg.Funded.Amount))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "missing") || strings.Contains(strings.ToLower(err.Error()), "spent") {
			fmt.Printf("%s: on-chain leg already spent (taker paid and claimed, or a refund landed): %v; marking terminal\n", name, err)
			st.Settled = true
			writeSessionState(dir, st.SessionID, st)
			return
		}
		fmt.Printf("%s: refund on-chain leg: %v\n", name, err)
		return
	}
	st.BtcRefundTx = txid
	writeSessionState(dir, st.SessionID, st)
	fmt.Printf("%s: ON-CHAIN LEG REFUNDED in %s (T_btc=%d passed, taker never paid the asset)\n", name, txid, st.BtcLocktime)
}

func resumeSubAssetClaim(st *xmakerSessionState, btcChain *xchain.BitcoinChain, seqChain *xchain.Chain, spendFee uint64, dir, name string) {
	if st.SecretHex == "" || st.BtcLegTxid == "" || st.BtcLegScriptHex == "" {
		fmt.Printf("%s: sub-asset BUY session without P / leg; nothing claimable\n", name)
		return
	}
	if st.BtcClaimTxid != "" {
		fmt.Printf("%s: already claimed in %s\n", name, st.BtcClaimTxid)
		return
	}
	secret, err := hex.DecodeString(st.SecretHex)
	if err != nil || len(secret) != xchain.PreimageLen {
		fmt.Printf("%s: bad persisted preimage\n", name)
		return
	}
	hashH, _ := hex.DecodeString(st.HashHex)
	swap, tipFn, err := onchainSwapFor(st.BtcLegBackend, hashH, btcChain, seqChain)
	if err != nil {
		fmt.Printf("%s: %v\n", name, err)
		return
	}
	if err := swap.InjectSecret(secret); err != nil {
		fmt.Printf("%s: %v\n", name, err)
		return
	}
	if tip, err := tipFn(); err == nil && int64(st.BtcLocktime) <= tip {
		fmt.Printf("%s: WARNING: T_btc=%d already reachable at tip %d; claiming anyway (first spend wins against the taker's refund)\n", name, st.BtcLocktime, tip)
	}
	leg, err := legFromState(st.BtcLegTxid, st.BtcLegVout, st.BtcLegAmount, "", st.BtcLegScriptHex, st.BtcLocktime)
	if err != nil {
		fmt.Printf("%s: %v\n", name, err)
		return
	}
	claimKey := resumeIdentityKey()
	if claimKey == nil {
		fmt.Printf("%s: the sub-asset BUY claim key is the maker identity key; pass -maker-priv to -resume\n", name)
		return
	}
	txid, err := swap.ClaimBTCLeg(leg, claimKey, xcSafeFeeAtoms(spendFee, leg.Funded.Amount))
	if err != nil {
		fmt.Printf("%s: claim on-chain leg with P: %v\n", name, err)
		return
	}
	st.BtcClaimTxid = txid
	st.Settled = true
	writeSessionState(dir, st.SessionID, st)
	fmt.Printf("%s: ON-CHAIN LEG CLAIMED in %s with the persisted preimage\n", name, txid)
}

// xcSafeFeeAtoms clamps a flat fee target to half the leg so the spend output stays
// positive (the engine's per-asset sizing applies on top where a chain is wired).
func xcSafeFeeAtoms(target, amount uint64) uint64 {
	if target == 0 {
		target = 1000
	}
	if max := amount / 2; target > max {
		return max
	}
	return target
}
