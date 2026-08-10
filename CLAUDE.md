# SeqDEX

The Sequentia DEX: a non-custodial order-book and atomic-swap system, in Go. Forked from TDEX
(`tdex-daemon`, `tdex-protobuf`) and Ocean.

Everything here is testnet software. Node and consensus conventions live in the
[`Sequentia`](https://github.com/GracedEternalKingCabbageMan/Sequentia) repo.

## Branches: read this first

The remote default is `main`. Active development has been on `phase3-pure-ln`, and the two have
**diverged** — each carries commits the other does not, including the same fix landed twice under
the same commit title.

The documentation rewrite lives only on `main`. `README.md`, `docs/ARCHITECTURE.md` and
`docs/DEV.md` on `phase3-pure-ln` are the older, much shorter pre-rewrite versions, and
`wallet/README.md` there is still verbatim upstream Ocean. **When you want to know how something
works, read the docs on `main`** (`git show main:docs/DEV.md`), then verify against code, because
even `main`'s docs predate the July retirement described below.

## Modules and layout

Seven Go modules; the root one only covers `proto/gen/go`. The two that matter are `daemon/` and
`wallet/`.

The Go import path is `github.com/aejkcs50/seqdex/...` while the GitHub remote is
`GracedEternalKingCabbageMan/seqdex`. That is a historical username, not a mistake — do not
"fix" the module path.

There is **no committed `go.work`**; it is kept local by design.

- `daemon/internal/seqob/` — the order book: `api`, `client`, `matcher`, `offer`, `offerstore`,
  `covfill`, `reorg`, `session`, `settler`, `validator`, `watcher`, `bridge`.
- `daemon/pkg/xchain/` — the shared cross-chain HTLC engine.
- `daemon/pkg/covenant/` — the covenant CLOB leaf builder.
- `wallet/` — the Ocean fork, the Sequentia wallet daemon.

## The naming trap: `xchain` is LIVE

The retired RFQ rail and the live order-book cross rail were **both** called "xchain". The RFQ
rail was deleted (its protos, its `xchainmaker` application package, and the `seqdex-xchaind` /
`seqdex-xchain-taker` / `seqdex-xchain-reverse-taker` binaries). What survived, deliberately:

- **`daemon/pkg/xchain/` is the live cross taker and is imported by 43 files** — `seqob-maker`,
  `seqob-cli`, `seqob-settler`, `seqob-crosser`, the `internal/seqob/client/xdriver*.go` drivers,
  and even `tdexd`'s fee-rate handler. Deleting it breaks nearly everything.
- `daemon/cmd/seqdex-xchain-swapdemo` is kept for the same reason: despite its name it exercises
  `pkg/xchain` and holds no reference to the retired service.
- The `x`-prefixed files under `daemon/internal/seqob/client/` (`xcourier*.go`, `xdriver*.go`,
  `xminslice.go`) are all live.
- "RFQ" survives **only in comments** describing the retired rail. There is no code named `rfq`.

**Draw the dead-code boundary by reachability from a live entry point, never by name match.**
`daemon/pkg/xchain/maker.go` carries an explicit warning header saying exactly this, because its
previous header claimed it served the retired service and that was already false when written.

One consequence: `daemon/api-spec/protobuf/seqdex/gen-seqdex.sh` still loops over an `xchain`
proto that no longer exists. Its file-existence guard makes it a silent no-op.

## What runs on the server

| Binary | Role |
|---|---|
| `seqobd` | The relay. Stores signed offers, serves the book over REST and WS, couriers opaque end-to-end-encrypted swap messages. **Holds no wallet, no keys and no funds.** The chain watcher runs as a goroutine inside it when given `-node-rpc`. |
| `seqob-maker` | The maker. Modes: `samechain`, `cross`, `lightning`, `pureln`, `subasset`, `subasset-sell`. |
| `seqob-crosser` | Crossing agent; polls every relay and takes both sides. |
| `seqob-settler` | Always-online non-custodial settler for the passive covenant CLOB (`plan`, `run`). |
| `seqob-bridge` | Cross-rail settlement bridge; the only seqob component holding its own inventory and LN node. |
| `tdexd` | The forked LP daemon. Deployed under the name `seqdexd` — there is no `cmd/seqdexd`. |
| `oceand` | The wallet daemon, from the `wallet/` module. |

The box runs **several `seqobd` relay instances**, one per rail; the `seqob-crosser` `-relays`
default documents the fleet (9955 cross, 9965 pure-LN and submarine, 9966 sub-asset buy, 9971
sub-asset sell, plus the covenant relay).

Everything else in `cmd/` is a one-shot CLI or a regtest helper.

**There are no systemd units, deploy scripts or supervisor scripts in this repo** — not now and
not in its history. Deployment is pull-only: the server pulls from GitHub and builds there. Never
edit source on the server, and never copy source onto it.

## Keys

Maker identity keys are supplied at launch as a hex flag (`-maker-priv`) or generated in-process.
Nothing reads a key from a path inside this repo. They live outside it, and they must never be
committed.

**Each maker needs its own identity key.** A shared key cross-wired live swaps three times on the
sub-asset rail, each time stranding a funded taker HTLC until its CLTV refund.

Per-swap recovery material (session keys and legs) is written under the maker's `-xstate-dir` and
the crosser's `-state-dir`. Those directories are the difference between a recoverable
interruption and a stranded swap.

## Build and test

There is **no CI**. Nothing runs unless you run it.

```sh
cd daemon && go build ./...
cd ../wallet && go build ./...

cd daemon
go build -o ../bin/seqobd       ./cmd/seqobd
go build -o ../bin/seqob-maker  ./cmd/seqob-maker
go build -o ../bin/seqob-cli    ./cmd/seqob-cli
go build -o ../bin/tdexd        ./cmd/tdexd

cd daemon  && go test ./internal/seqob/... ./pkg/xchain/...
cd ../wallet && go test ./pkg/...
```

`daemon/Makefile` also offers `test` (`-race`), `cov`, `vet`, `lint` (golangci-lint), `proto`,
`mock` and `integrationtest`.

Known non-signals, per `main`'s `docs/DEV.md`: `wallet/internal/core/...` tests fail with "unknown
network", and `daemon/test/e2e` (`make integrationtest`) has never been adapted to Sequentia.

`make run` is stale: it exports `TDEX_*`, but the config reader's env prefix is `SEQDEX`. The only
remaining `TDEX_` consumer is the operator CLI's own datadir variable. The upstream
docker-compose file is stale in the same way.

Build binaries with `-o` into `bin/`. Running `go build ./cmd/<name>` from inside `daemon/` drops
stray binaries in `daemon/`, which has happened often enough that `.gitignore` has entries for
them by name.

Regtest needs Elements mode (`-con_elementsmode=1`); a plain `elementsd -chain=regtest` runs in
Bitcoin serialization mode and the parsers cannot decode it.

## Correctness rules with a history behind them

- **Never die mid-swap.** `seqob-maker` drains in-flight lifts on SIGTERM before exiting.
- **Makers exit on purpose and must run under a supervisor.** An offer expiring silently emptied
  the book, so a maker now exits shortly before its own offers expire and expects to be restarted.
  The supervisor is not in this repo.
- **Counterparty-supplied values are clamped, always.** The driver marks the counterparty-controlled
  fields explicitly and never trusts a courier-supplied amount.
- **The native asset is not 1:1 for fees.** Sizing a fee as though it were under-pays. Fees are
  sized from the published rate, for every asset including the native one.
- **Offer canonical bytes are a cross-language contract.** The signed canonical form excludes the
  covenant outpoint. The JavaScript side in `sequentia-web-wallet` re-implements this and the
  covenant byte order; a change on either side must land on both.
- Byte order at the covenant boundary is display-vs-internal, and the wallet mirrors this repo's
  `reverseHex`/`displayHex` conventions. Golden vectors pin the two implementations together.

## Working in this repo

- **Repository is public.** Never commit keys, seeds, wallet files, RPC credentials, `.env` files
  or tokens.
- **Commit author:**
  `GracedEternalKingCabbageMan <151803062+GracedEternalKingCabbageMan@users.noreply.github.com>`
- **Always open a pull request, then merge it yourself immediately.** The PR exists so the change
  and its reasoning are recorded, not because anyone is waiting to review it. There is no review
  process. If you are ever told to leave one specific PR open, that applies to that PR only and
  never becomes the default.
