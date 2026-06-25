# SeqDEX architecture & fork plan

This document grounds the SeqDEX build in the actual upstream code (TDEX + Ocean)
and the Sequentia node, based on a direct read of all three.

## 1. What SeqDEX is

A liquidity-provider / market-maker DEX (not an on-chain orderbook), forked from
TDEX. Trades are cooperative atomic swaps. SeqDEX is **backend-first**: the durable
product is the daemon + protocol, consumed by many client surfaces (SWK web wallet =
the PoC client; later the Ambra mobile wallet, a browser-extension wallet, and a
desktop app bundling the daemon next to a Sequentia node).

Two layers:

1. **Backend (Go):** the forked `daemon` (LP server, public Trade `:9945` + private
   Operator `:9000`) talking to the `wallet` (thin-forked Ocean) over the `ocean.v1`
   gRPC contract. Plus `xchain`, the new BTC↔SEQ cross-chain swap service.
2. **Trader clients:** thin clients generated from the `seqdex.v1` protos, using a
   wallet library for PSET build/sign. The web PoC uses SWK's `lwk_wasm` (which already
   exports PSET/Signer/Esplora/Registry/Prices + a LiquidEx same-chain swap surface).

## 2. Why these forks (key recon findings)

- **The daemon is wallet-detached.** Everything is programmed against the interface in
  `internal/core/ports/wallet.go` (Wallet/Account/Transaction/Notification), implemented
  by `internal/infrastructure/ocean-wallet/*` over the `ocean.v1` protos. **We keep that
  seam unchanged** and point it at a Sequentia-backed wallet.
- **Ocean is a *thin* fork, not a rewrite.** Only ~3–5% of its ~30k LOC is Liquid-specific,
  and it is almost entirely config/params. Ocean's `elements` blockchain scanner speaks
  plain Elements/Bitcoin-Core JSON-RPC and works against a Sequentia node with **zero code
  change** — just point `NODE_RPC_ADDR` at it. The fork is: inject one `network.Network`
  value (Sequentia HRPs, version bytes, policy asset) + change ~5 config defaults. The
  blinding / PSETv2 / SLIP-77 / coin-selection core is generic Elements and reused as-is.
- **The policy (native) asset is not a source constant.** Sequentia computes it at runtime
  (`getsidechaininfo.pegged_asset`); it must be read live, never hardcoded.
- **The v2 protos already carry any-asset-fee fields** (`fee_asset`/`fee_amount`), so
  Sequentia's model maps directly onto `seqdex.v1` (collapsed from TDEX v2).
- **The atomic-swap PSET** is built maker-side in
  `internal/core/application/wallet/service.go::CompleteSwap()`; same-chain swaps are an
  Elements consensus property Sequentia inherits — no consensus/tx-format work needed.

## 3. Protocol (this repo, phase 1 — DONE)

- `proto/seqdex/v1/*` — public contract, forked from `tdex.v2` and renamed to `seqdex.v1`
  (a clean break; Sequentia is a separate network, no TDEX interop intended). PSETv2 only;
  legacy v1/PSETv0 swap variants dropped. REST paths reversioned `/v2/*` → `/v1/*`.
- `proto/ocean/v1/*` — copied **byte-identical** (package `ocean.v1`) so the forked daemon's
  generated wallet client matches and Ocean stays drop-in.
- Codegen is **local** via buf with remote plugins (`buf.gen.yaml`) → `proto/gen/go`,
  dropping TDEX's dependency on remote buf registries it doesn't control.

## 4. Cross-chain (phase 5): Design A, abstracted

The novel piece. TDEX is same-chain only; `xchain` adds a BTC↔SEQ swap service where the
daemon is the counterparty/market-maker.

- **Design A — on-chain HTLC (hashlock + CLTV refund).** Reuses the proven
  `contrib/sequentia/swap-demo.py` HTLC script verbatim. Chosen for the PoC because it is
  built/tested, maximally compatible across heterogeneous clients, and **composes with the
  future submarine-swap roadmap** (today's Lightning + Boltz submarine swaps are hashlock-based,
  sharing `payment_hash`).
- **Anchoring removes the SEQ-side reorg buffer.** `feature_anchor_swap_consistency.py`
  proves a SEQ block (and its swap leg) reverts iff its anchored Bitcoin block reverts. So:
  lock BTC first, then the SEQ leg with `anchorheight ≥ BTC-leg height`, and the SEQ side
  needs ~1 confirmation with no extra buffer. The claimant MUST verify the actual
  `anchorheight` and `getanchorstatus` before treating the SEQ leg as safe (producer fallback
  can reuse a stale parent anchor). The anchoring win is **script-independent** — it applies
  equally to a later PTLC design.
- **Abstract over a `LockPrimitive`** (hashlock | adaptor) so Design B (PTLC/Taproot
  scriptless, a privacy upgrade) drops in later without re-architecting. B is gated on:
  Taproot key-path + Tapscript-refund standard-relay on the Sequentia chains, a vetted
  adaptor-sig lib across clients, and CT-interplay validation.
- **Finality is anchor-bounded, never instant.** Settlement detection must be reorg-aware
  (a "settled" SEQ leg can revert with Bitcoin) — this corrects TDEX's Liquid-style
  immediate-finality assumption.
- **Lightning is a future sub-project** (pure-LN and submarine swaps; needs c-lightning
  adapted for Sequentia). The PoC settles purely on-chain, but the client swap UI/UX state
  machine is designed with LN "routes" in mind so they slot in without a redesign.

## 5. Any-asset fees

Sequentia has one fee whitelist (`ExchangeRateMap`, `src/exchangerates.h`), fed by the
price-server sidecar, with `nFeeAsset` valued at native-equivalent. The `seqdex.v1` fee
fields already model per-trade fee asset/amount. MVP keeps Ocean's native-asset network-fee
path; allowing the on-chain network-fee leg in any whitelisted asset is a later Ocean-side
enhancement (it touches `CompleteSwap` + the fee-account logic), not part of the params fork.

## 6. Build phasing

1. **Proto fork + codegen** — this repo skeleton. *(done)*
2. **Wallet** — thin-fork Ocean: Sequentia `network.Network` + config defaults; connect the
   `elements` scanner to the Sequentia testnet node; create wallet, derive addresses, list UTXOs.
3. **Daemon** — fork tdex-daemon: rebrand, repoint protos to local `seqdex.v1`, wire to the
   wallet over `ocean.v1`; serve Trade + Operator.
4. **Same-chain swap e2e** — create/fund a market, run an asset↔asset swap on testnet.
5. **Cross-chain HTLC (`xchain`)** — Design A, anchor-aware, reorg-aware settlement.
6. **Web UI** — a "Swap" section in SWK (Vite + React) over the daemon + lwk_wasm.

## 7. Open hinges to validate (before/within phase 2)

- **go-elements wire-compat:** is Sequentia byte-compatible with `go-elements v0.5.4`
  (Elements 23.x) for tx/asset serialization + PSETv2? If yes, upstream go-elements + a
  Sequentia `network.Network` suffices; if not, a go-elements fork is needed (larger).
- **`getblockfilter` (BIP157/158):** Ocean's `elements` scanner relies on it — confirm the
  Sequentia node runs with the block-filter index enabled, or pick an alternate scanner.
- **HRPs / version bytes:** Sequentia testnet bech32 `tb` / blech32 `tsqb`; mainnet `bc` /
  `sqb` (`src/chainparams.cpp`). Confirm go-elements address encoding matches.
- **Coin type:** Liquid uses SLIP-44 1776; pick a Sequentia coin type (or document reuse).
