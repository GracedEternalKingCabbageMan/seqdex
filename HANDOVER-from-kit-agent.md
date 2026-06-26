# Handover to the seqdex/seqob agent — from the dual-chain kit work

Two things landed on this repo's branch (`claude/seqob-orderbook-dex`) and on the SWK
kit that intersect your daemon. Both are adversarially verified + compile-clean but
NOT deployed, because deploying intersects your in-flight work. Please fold them into
your next `seqdexd` build/deploy.

## 1. Daemon: taker-funded any-asset swap network fee — `43f5b3c` (already pushed here)

A 2-file diff, sitting on top of your `eaeca1d`:
- `daemon/internal/core/application/wallet/service.go` — `CompleteSwap` detects a single
  empty-script taker-supplied fee output, validates it (fee-eligible + node-floor
  native-equiv >= the size-based requiredFee + every taker input revealed +
  asset != assetR), and SKIPS the maker's own fee block when present. Absent → the
  maker-funded path is unchanged.
- `daemon/internal/core/domain/market.go` — `Preview` fee-asset guard relaxed to any
  valid asset (commission still leg-only; a third asset gets 0 commission). Traded-asset
  guard kept.

Verified safe (a workflow + the verify agents): can't be underpaid, commission stays
correct, no double-fee. It integrates with your `blind` flag (grpc same-chain path is
`blind=true`, unchanged). **Paired changes already pushed elsewhere** that this expects:
- lwk `SWK@0f8e81a2`: `seqdex_swap_request` builds the taker fee input + explicit fee
  output; the wire `fee_amount` is sent as 0 (the network fee lives in the PSET).
- Ambra `ambra@57f3c1e`: swap composer sends wire `fee_amount=0`.
- **COUPLING:** the web wallet's `swap.js` must also set wire `fee_amount=0` (and the new
  SWK wasm has a `seqdexSwapRequest` signature with an added `fee_rate` arg) before the
  new wasm is deployed, or the live web swap breaks. Deploy daemon + lwk wasm + the two
  wallets together.

To deploy: build `cmd/tdexd` -> `seqdexd`, replace `/root/seqdex-bin/seqdexd` (back up
first), restart `seqdexd.service`. Test with a non-leg fee asset (e.g. GOLD on tSEQ/SILVR)
via `/v1/trade/propose`; decode the broadcast tx for a single GOLD fee vout + per-asset
conservation + the native fee account untouched.

## 2. Cross-chain (xchain): the kit now drives BTC<->asset swaps — needs a SEQ-funded maker

The SWK kit (`lwk_wollet::btc::xchain` + the `XchainSwap` wasm binding + Ambra's FFI)
now implements the full taker side of your `/v1/xchain/{markets,quote,propose,swap}`
flow: the reveal/anchor gate (ported verbatim, `anchor>=H_btc && anchorstatus==ok &&
depth>=D`, D=1 taker dial — NO reorg buffers/timelocks), SEQ claim, BTC refund, RBF,
rate-derived claim fee, sealed state. It's LIVE-VALIDATED read-only against your daemon
(markets parse; the gate computes `ok=true` at depth 1 vs the real SEQ tip anchor).

**BLOCKER for an E2E swap (your area):** every xchain market reports `seqReserve: 0`, so
a BTC->asset quote fails ("exceeds available SEQ reserve 0"). The maker's SEQ reserve =
wallet **`w`**'s balance on the SEQ node (RPC `127.0.0.1:18300`), per
`xchainmaker/maker.go::seqReserveAtoms`. To enable cross-chain swaps end-to-end, fund
wallet `w` with the relevant SEQ asset (e.g. GOLD) from the treasury. (The maker's BTC
reserve `tb1q8fed...` = 1811101 sats IS funded.) The xchainmaker `LockSEQLeg`/propose
path is yours + in flux — please confirm it's stable before a live swap is attempted.

The kit is done; the remaining cross-chain gap is maker SEQ funding + a stable xchain
maker, both on your side.

---
Contact point: this branch's commit `43f5b3c` + SWK branch `sequentia`
(`0f8e81a2`..`2618f86a`). Detailed design/verification in the kit-agent's notes.
