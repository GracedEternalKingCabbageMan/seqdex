# SeqDEX daemon module

The Go module holding everything server- and client-side for SeqDEX except the
wallet daemon: the SeqOB order-book relay and its maker/taker programs, the RFQ
trade daemon (forked from
[tdex-daemon](https://github.com/tdex-network/tdex-daemon)), and the
cross-chain HTLC engine. The wallet daemon it settles through lives in
[`../wallet`](../wallet). Design: [../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md);
runbooks: [../docs/DEV.md](../docs/DEV.md).

Everything here is testnet software; nothing in this module ever holds user
funds (the RFQ daemon holds only its operator's own market-making liquidity,
via the wallet daemon).

## Binaries

### `cmd/seqobd`: the order-book relay

Non-custodial: stores signed offers, serves the per-pair book, couriers opaque
end-to-end-encrypted swap-session frames. No wallet, no keys, no funds, never
decrypts.

```
seqobd [flags]
  -listen            HTTP listen address        (default ":9955", env SEQOB_LISTEN)
  -node-rpc          read-only Sequentia node RPC URL, reserved for the future
                     liveness/anchor watch      (env SEQOB_NODE_RPC; unused today)
  -session-deadline  lift co-sign deadline      (default 2m)
  -xsession-deadline courier deadline for cross-chain lift sessions
                     (default 3h; they span a real parent-chain confirmation)
  -expiry-sweep      offer expiry sweep interval        (default 15s)
  -session-sweep     session deadline sweep interval    (default 10s)
  -min-expiry / -max-expiry   allowed offer expiry horizon (30s .. 168h)
  -offers-per-min / -offers-per-min-ip   rate limits (60 per maker key, 120 per IP)
```

HTTP surface (bodies are protojson of `api-spec/protobuf/seqob/v1`):

```
POST /v1/offers                             submit a signed Offer
POST /v1/offers/cancel                      submit a signed OfferCancel
GET  /v1/offers?maker_pubkey=<hex>          a maker's own orders
GET  /v1/markets                            market summaries
GET  /v1/market/{base}/{quote}/orderbook    per-pair snapshot
POST /v1/lift                               open a lift session
GET  /v1/ws                                 WebSocket (To/From envelope): book
                                            subscriptions, offer submission,
                                            lift notifications, opaque SwapMsg
                                            courier
```

Live instance: `https://sequentiatestnet.com/seqob/v1/...`.

### `cmd/seqob-maker`: the maker participant

Posts one signed resting offer and serves lifts until stopped. Anyone can run
one; the relay grants makers no special standing. Three settlement modes:

- `-mode samechain` (default): asset-for-asset atomic swap, settled through the
  Ocean wallet's blind-aware `CompleteSwap`. Needs `-ocean` + `-account` (the
  account holding the offered asset) and `-node-rpc` for the open fee market.
  `-confidential=true` (default) publishes a blinding pubkey and settles
  blinded; `-confidential=false` settles explicit.
- `-mode cross`: BTC-for-asset on-chain HTLC swaps. Funds legs from node
  wallets directly: needs `-btc-rpc`/`-btc-wallet`/`-btc-chain`
  (testnet4|regtest) and `-xseq-rpc`/`-xseq-wallet`. Pair is always
  base = the asset (`-base`), quote = the `"BTC"` sentinel; `-base-amount` is
  asset atoms, `-quote-amount` BTC sats; no partial fills. Per-lift session
  state persists under `-xstate-dir`; `-resume` finishes every non-terminal
  session (claim or refund) after a restart, then exits. Safety dials:
  `-min-btc-conf` (default 1), `-btc-locktime-delta` (T_btc, default 100
  parent blocks), `-seq-locktime-delta` (T_seq, default 240 Sequentia blocks),
  `-btc-fee-rate`, `-spend-fee`.
- `-mode lightning`: submarine swaps (asset on-chain against BTC over
  Lightning). Needs `-ln-socket` (a SeqLN/CLN `lightning-rpc` on Bitcoin) plus
  `-xseq-rpc`/`-xseq-wallet`. `-side buy` serves the NORMAL direction (taker
  sells the asset, maker pays the taker's invoice), `-side sell` the REVERSE
  (maker locks the asset, taker pays the maker's invoice).
  `-sub-anchor-depth` (default 3, floor 2) is the Bitcoin-anchor depth the
  maker requires on the taker's Sequentia funding before taking the
  irreversible Lightning action.

Common flags: `-relay`, `-maker-priv` (32-byte hex identity/E2E key, generated
if empty), `-base`, `-quote`, `-side sell|buy`, `-base-amount`,
`-quote-amount`, `-fee-asset` (hint, any-asset fee market),
`-min-anchor-depth` (Bitcoin-anchor confirmations before the order is FILLED;
default 0 = 0-conf tolerant), `-expiry`, `-offer-id`, `-msats-per-byte`.

### `cmd/seqob-cli`: the taker (and ad-hoc maker) CLI

```
seqob-cli post         post a signed offer (no maker process; lifts need one)
seqob-cli book         print a market's order book
seqob-cli lift         lift a same-chain offer (real settlement with
                       -esplora -taker-priv -taker-blinding; stub-wallet
                       demo mode without them)
seqob-cli xlift        buy an asset with on-chain BTC (forward cross-chain)
seqob-cli xsell        sell an asset for on-chain BTC (reverse cross-chain)
seqob-cli xrefund      recover the BTC leg of an aborted xlift after T_btc
seqob-cli xrefund-seq  recover the asset leg of an aborted xsell after T_seq
seqob-cli xsublift     sell an asset for BTC over Lightning (normal submarine)
seqob-cli xsubbuy      buy an asset with BTC over Lightning (reverse submarine;
                       anchor-gates the maker's HTLC before paying)
```

Run any subcommand without flags for its exact flag list. Cross-chain
subcommands persist their session (secret, keys, legs, locktimes) to
`-state-file` before funding anything, so refunds survive a crash.

### `cmd/seqob-octl`: maker account helper

Read-mostly Ocean helper: `-action balance | address | create` against
`-ocean <addr> -account <name>`. No keys, no signing.

### `cmd/tdexd`: the RFQ trade daemon

The forked tdex-daemon. Configuration is env-only with prefix `SEQDEX_`
(upstream used `TDEX_`); the main keys (see `internal/config/config.go` for
all):

| Env | Meaning | Default |
|---|---|---|
| `SEQDEX_WALLET_ADDR` | ocean wallet gRPC endpoint | - |
| `SEQDEX_TRADE_LISTENING_PORT` | Trade interface (gRPC + grpc-web + REST) | `9945` |
| `SEQDEX_OPERATOR_LISTENING_PORT` | Operator interface | `9000` |
| `SEQDEX_NODE_RPC` | Sequentia node RPC URL for `getfeeexchangerates` (any-asset network fees); unset = legacy native-asset fee path | - |
| `SEQDEX_NO_MACAROONS`, `SEQDEX_NO_OPERATOR_TLS` | disable auth/TLS (dev) | `false` |
| `SEQDEX_DATADIR`, `SEQDEX_DB_TYPE`, `SEQDEX_LOG_LEVEL` | state and logging | `~/.seqdex-daemon`, `badger`, `4` |
| `SEQDEX_XCHAIN_PARENT_RPC` | parent-chain node RPC URL; **presence enables the integrated cross-chain maker** | - |
| `SEQDEX_XCHAIN_PARENT_KIND` | `elements` or `bitcoin` (real bitcoind) | `elements` |
| `SEQDEX_XCHAIN_PARENT_CHAIN` | when bitcoin: `regtest` or `testnet4` | `regtest` |
| `SEQDEX_XCHAIN_SEQ_RPC` | anchored Sequentia node RPC URL | - |
| `SEQDEX_XCHAIN_WALLET` | wallet name on both nodes | `w` |
| `SEQDEX_XCHAIN_SEQ_ASSET` or `SEQDEX_XCHAIN_MARKETS` | single asset id, or a JSON array of markets `[{"seq_asset":...,"name":"BTC/GOLD","price_seq_per_btc":...,"fee_bps":...}]` | - |
| `SEQDEX_XCHAIN_PRICE_SEQ_PER_BTC`, `SEQDEX_XCHAIN_FEE_BPS` | maker pricing | `100`, `0` |
| `SEQDEX_XCHAIN_BTC_LOCKTIME_DELTA`, `SEQDEX_XCHAIN_SEQ_LOCKTIME_DELTA` | T_btc / T_seq deltas | `100`, `50` |
| `SEQDEX_XCHAIN_MIN_BTC_CONF`, `SEQDEX_XCHAIN_SPEND_FEE`, `SEQDEX_XCHAIN_QUOTE_TTL_SECS` | safety/fees/quote TTL | `1`, `1000`, `3600` |

Interfaces served:

- **Trade** (`:9945`, TLS optional, permissive CORS): gRPC
  `seqdex.v1.TradeService` plus grpc-web and a grpc-gateway REST mapping:
  `POST /v1/markets`, `/v1/market/balance`, `/v1/market/price`,
  `/v1/trade/preview`, `/v1/trade/propose`, `/v1/trade/complete`. The Trade
  interface starts after the wallet is unlocked.
- **Xchain** (same listener, when `SEQDEX_XCHAIN_PARENT_RPC` is set):
  `seqdex.v1.XchainService`: `POST /v1/xchain/markets`, `/v1/xchain/quote`,
  `/v1/xchain/propose`, `/v1/xchain/swap`, `/v1/xchain/reverse/quote`,
  `/v1/xchain/reverse/open`, `/v1/xchain/reverse/submit`.
- **Operator** (`:9000`, macaroons unless disabled): wallet init/unlock, market
  lifecycle, deposits, withdrawals, fee config (protos under
  `api-spec/protobuf/tdex-daemon/v2`). Drive it with `cmd/tdex` or the
  `seqdex-initwallet` / `seqdex-unlock` / `seqdex-market` helpers.

Live instance (REST gateway): `https://sequentiatestnet.com/dex/v1/...`.

### Cross-chain CLIs and demos

- `cmd/seqdex-xchain-taker`: full forward (BTC to asset) swap over
  `XchainService`, including the anchor gate and on-chain claim.
- `cmd/seqdex-xchain-reverse-taker`: full reverse (asset to BTC) swap; the
  maker holds the secret and funds BTC first.
- `cmd/seqdex-xchaind`: standalone bare-gRPC cross-chain maker
  (env `XCHAIN_*`, listen `XCHAIN_LISTEN`, default `127.0.0.1:9955`, which
  collides with seqobd's default port; rebind one when running both).
  Superseded by tdexd's integrated XchainService for anything browser-facing.
- `cmd/seqdex-xchain-swapdemo`: runs the raw HTLC mechanism end to end against
  the two-chain regtest, including the negative test (a Sequentia leg with an
  insufficient anchor height is rejected).
- `cmd/seqdex-taker`, `cmd/seqdex-market`, `cmd/seqdex-initwallet`,
  `cmd/seqdex-unlock`: throwaway regtest helpers for the RFQ loop.

## Package map

```
internal/seqob/offer       offer/cancel canonical bytes, signing, verification,
                           the BTC sentinel, direction consistency
internal/seqob/offerstore  the book: keys, statuses, partial fills, anchor-aware
                           FILLED, reopen hook, expiry sweeper
internal/seqob/validator   admission: signature/schema/amounts/expiry checks,
                           rate limits, replay gate (liveness probe = stub)
internal/seqob/session     lift-session router: peer binding, opaque courier,
                           deadlines (reorg watcher = stub)
internal/seqob/api         REST + WS server over the To/From envelope
internal/seqob/client      taker/maker drivers: same-chain (driver.go),
                           cross-chain (xdriver*.go), submarine
                           (xdriver_submarine*.go), E2E crypter, live wallet
                           backend over Ocean
pkg/xchain                 the HTLC engine: legs (elements/bitcoin/lightning),
                           orchestrator, VerifySeqLegSafe anchor gate,
                           submarine swaps, regtest harness (testdata/)
internal/core, internal/infrastructure, pkg/trade, pkg/swap, ...
                           the inherited tdex-daemon core (application services,
                           ocean-wallet port, taker SDK), adapted for Sequentia
                           (seqnet params, runtime policy asset, any-asset fees)
pkg/seqnet                 Sequentia network parameters + address handling
api-spec/protobuf          authoritative contracts: seqdex/v1 (+xchain),
                           seqob/v1, tdex-daemon/v1+v2, generated Go under gen/
```

## Build and test

```sh
go build ./...                                   # everything
make build                                       # tdexd via scripts/build (CGO, stripped)
go test ./internal/seqob/... ./pkg/xchain/...    # unit + (auto-skipping) integration
make lint                                        # golangci-lint
```

The `pkg/xchain` integration tests start their own two-chain regtest via
`pkg/xchain/testdata/start-regtest.sh` when `SEQUENTIA_REPO` points at a built
node clone (or use running nodes via `SEQDEX_XCHAIN_PARENT_RPC` /
`SEQDEX_XCHAIN_SEQ_RPC`); otherwise they skip. See
[../docs/DEV.md](../docs/DEV.md).

## Upstream

This module began as tdex-daemon (MIT); upstream docs live at
https://dev.tdex.network/. The `internal/seqob` and `pkg/xchain` trees and the
`seqob*`/`seqdex-*` commands are new here; `../NOTICE` retains the upstream
notices.
