# SeqDEX

SeqDEX: non-custodial atomic-swap DEX for [Sequentia](https://sequentia.io), a
Bitcoin sidechain for asset tokenization and decentralized exchange. It provides
a peer-to-peer order book (SeqOB), same-chain asset-to-asset atomic swaps, and
cross-chain BTC-to-asset swaps whose safety comes from Sequentia's real-time
Bitcoin anchoring. No component ever takes custody of user funds: settlement is
always an atomic transaction (same-chain) or a hash-time-locked contract pair
(cross-chain), and the order-book relay holds no keys, no wallet, and no funds.

Everything here is **testnet software**. There is no Sequentia mainnet.

## Where this fits in the Sequentia ecosystem

| Repo | One-liner |
|---|---|
| [`Sequentia`](https://github.com/GracedEternalKingCabbageMan/Sequentia) | The Sequentia node (`elementsd` fork of Elements 23.3.3): consensus, anchoring, proof of stake, open fee market, plus the canonical protocol documentation in `doc/sequentia/`. |
| [`seqdex`](https://github.com/GracedEternalKingCabbageMan/seqdex) | SeqDEX: non-custodial atomic-swap DEX - P2P order book (seqob), same-chain swaps, and cross-chain BTC<->asset swaps made safe by Bitcoin anchoring. |
| [`SWK`](https://github.com/GracedEternalKingCabbageMan/SWK) | Sequentia Wallet Kit: a fork of Blockstream LWK - Rust wallet library, CLI, and WASM bindings for building Sequentia (and Bitcoin testnet4) wallets. |
| [`sequentia-web-wallet`](https://github.com/GracedEternalKingCabbageMan/sequentia-web-wallet) | Proof-of-concept browser wallet built on SWK, live at https://sequentiatestnet.com/wallet. |
| [`seqln`](https://github.com/GracedEternalKingCabbageMan/seqln) | SeqLN: a Core Lightning fork that runs on Sequentia and Bitcoin from the same binary - asset channels, any-asset payments, pure-Lightning swaps. |

Protocol-level background (anchoring, proof of stake, the open fee market) lives
in the node repo's documentation:
https://github.com/GracedEternalKingCabbageMan/Sequentia/tree/main/doc/sequentia

## What is in this repo

Two cooperating DEX subsystems plus a shared settlement engine, all in Go:

1. **SeqOB, the peer-to-peer order-book DEX** (`daemon/internal/seqob`,
   `daemon/cmd/seqobd`, `daemon/cmd/seqob-maker`, `daemon/cmd/seqob-cli`,
   `daemon/cmd/seqob-octl`). Resting orders are **signed intents** (price, size,
   expiry, keys), never pre-signed transactions and never named UTXOs. A
   non-custodial relay (`seqobd`) stores the signed offers, serves the per-pair
   book over REST and WebSocket, and couriers **opaque, end-to-end-encrypted**
   swap-session messages between maker and taker: it never decrypts, never
   signs, and never touches funds. Settlement happens directly between the two
   peers.

2. **The RFQ trade daemon** (`daemon/cmd/tdexd` plus the `wallet/` Ocean fork),
   a liquidity-provider daemon forked from the TDEX stack. A trader asks it for
   a quote and settles a cooperative atomic swap against the daemon's own
   market-maker wallet. It also hosts the cross-chain `XchainService` (gRPC,
   grpc-web, and REST) that the web wallet consumes.

3. **The cross-chain HTLC engine** (`daemon/pkg/xchain`), shared by both
   subsystems: BTC-to-asset atomic swaps between Bitcoin (testnet4 or a regtest
   stand-in) and Sequentia, in both directions, plus submarine swaps where the
   BTC leg travels over Lightning (via a SeqLN/CLN node) instead of on-chain.

Both subsystems settle same-chain swaps through the same proven path: the
`wallet/` daemon (a thin fork of Ocean) builds, blinds when requested, and
co-signs the atomic swap PSET.

## Status (2026-07-08)

Works today, exercised on the public testnet or in the committed test suites:

- SeqOB relay + maker + taker CLI: same-chain asset-to-asset lifts, partial
  fills, signed cancels, offer expiry, replay protection, rate limits.
- Cross-chain order-book settlement over the relay courier, both directions:
  forward (taker pays BTC, receives the asset) and reverse (taker sells the
  asset for BTC), with CLTV refund paths and maker crash-resume from persisted
  session state.
- RFQ same-chain trades (preview, propose, complete) and RFQ cross-chain swaps
  over gRPC/REST, including reverse swaps.
- Network fees payable in any fee-eligible asset, defaulting to the transacted
  asset (see "Fee model" below).
- Submarine swaps (asset on-chain against BTC over Lightning, both directions)
  are wired through the maker and CLI and were proven live, but they require a
  running SeqLN node and are the newest surface: treat them as experimental.

Deliberate gaps on this branch, stated honestly:

- The relay's maker-liveness probe and its settlement reorg watcher are no-op
  stubs. If a settling transaction's Bitcoin anchor were orphaned, the relay
  does not yet re-open the order automatically (the on-chain refund paths, not
  the relay, are the safety net; the relay never holds anything either way).
- Order and session state in the relay and the SeqOB maker's same-chain path is
  in-memory; cross-chain sessions are persisted (`-xstate-dir`) and resumable,
  same-chain lifts simply expire and the book re-forms.
- Covenant-enforced resting orders (Simplicity) and pure-Lightning swaps are
  **not** on `main`; see "Development branches" below.

## The live public testnet deployment

The public Sequentia testnet exposes both subsystems under
https://sequentiatestnet.com (verified 2026-07-08):

- **SeqOB relay**: `https://sequentiatestnet.com/seqob/v1/...` - for example
  `GET /seqob/v1/markets` currently lists 21 markets: 15 same-chain asset
  pairs and 6 cross-chain BTC pairs (a cross-chain pair's quote asset is the
  literal sentinel `"BTC"`).
- **RFQ trade daemon**: `https://sequentiatestnet.com/dex/v1/...` - for example
  `POST /dex/v1/markets` lists the RFQ markets, `POST /dex/v1/trade/preview`
  quotes a trade.

Try it:

```sh
curl -s https://sequentiatestnet.com/seqob/v1/markets | python3 -m json.tool | head -30
curl -s -X POST https://sequentiatestnet.com/dex/v1/markets -H 'Content-Type: application/json' -d '{}'
```

The web wallet at https://sequentiatestnet.com/wallet is the main graphical
client: its trade surface consumes these endpoints. The CLIs in this repo
(`seqob-cli`, `seqdex-taker`, the xchain takers) are the reference command-line
clients.

## How a trade works

**Same-chain, over the order book.** A maker posts a signed offer (for example,
sell 100 GOLD atoms for 4500 USDX atoms; the authoritative price is the integer
ratio `want_amount / offer_amount`, there is no float in the signed bytes). A
taker picks the offer from the book and opens a lift session. Relay-couriered,
end-to-end-encrypted messages carry a `SwapRequest` from the taker, a
`SwapAccept` (the maker's half-signed PSET) back, and a `SwapComplete`; the
taker signs and broadcasts one atomic transaction that pays both sides. At no
point can either party or the relay take the other's funds: an unsigned or
half-signed swap is worthless, and the worst outcome is a session timeout.

**Cross-chain (BTC on-chain).** The legs are two HTLCs sharing one SHA256
hashlock, with asymmetric timelocks (the BTC leg always refunds later than the
Sequentia leg). Forward direction: the taker locks BTC first, the maker locks
the Sequentia asset, the taker claims the asset (revealing the secret on
Sequentia), the maker claims the BTC. What makes this fast and safe is
Sequentia's anchoring: the claimant accepts the Sequentia leg only once it sits
in a block whose Bitcoin anchor height is at or above the BTC leg's
confirmation height, with `getanchorstatus` ok and the block quorum-certified.
Because a Sequentia block reverts if and only if its anchored Bitcoin block
reverts, no extra reorg buffer is needed on the Sequentia side. If anything
stalls, each party refunds its own leg after its timelock. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the exact message flows.

**Submarine (BTC over Lightning).** Same hashlock idea, but the BTC leg is a
BOLT11 Lightning payment instead of an on-chain HTLC. The maker plays the
non-custodial Boltz-style role and must not take the irreversible Lightning
action until the relevant Sequentia transaction is buried under a real Bitcoin
anchor depth (default 3, minimum 2).

## Finality honesty

Nothing on Sequentia is "final" at 0 confirmations. The honest finality measure
is **anchor depth**: how many Bitcoin blocks confirm the anchor of the
Sequentia block containing your settlement.

Every SeqOB offer carries a maker-chosen dial, `min_anchor_depth` (default 0):
the number of Bitcoin-anchor confirmations the maker requires on the settling
transaction before the order is marked `FILLED`. The DEX is 0-conf-tolerant by
design and enforces no floor; a maker who wants stronger guarantees (or a taker
choosing whom to trade with) raises the dial. Client UIs surface anchor depth
rather than pretending instant finality, and a status of `FILLED` at depth 0
can still revert if Bitcoin reorganizes: that is a property of every chain, and
Sequentia's design makes it visible instead of hiding it.

## Fee model

Sequentia has an open fee market: there is no privileged fee asset, and block
producers choose which assets they accept for fees and at what exchange rate
(published by the node as `getfeeexchangerates`). SeqDEX implements this
end-to-end:

- The on-chain network fee of a swap is paid **in any fee-eligible asset,
  defaulting to the transacted asset**, valued at the node's native-equivalent
  exchange rate. The legacy native-asset fee account is only a fallback.
- A taker can fund the network fee itself by adding an explicit fee output in
  its chosen asset; the maker's wallet validates it (correct value at the
  current rate) and then skips its own fee block.
- SeqOB offers carry a `fee_asset_hint`; it is a hint, never a privilege.
- Fee rates are always denominated in the chosen fee asset's own units per
  vByte, never "sat/vB".

## Running it yourself

Prerequisites: a recent Go toolchain (both modules build cleanly with Go 1.26;
the daemon's release build uses CGO), and for full local loops a built Sequentia
node (`elementsd` from
https://github.com/GracedEternalKingCabbageMan/Sequentia).

Build every binary:

```sh
cd daemon && go build ./... && cd ../wallet && go build ./...
# or specific binaries, e.g.:
cd daemon
go build -o ../bin/seqobd      ./cmd/seqobd
go build -o ../bin/seqob-maker ./cmd/seqob-maker
go build -o ../bin/seqob-cli   ./cmd/seqob-cli
go build -o ../bin/tdexd       ./cmd/tdexd
cd ../wallet
go build -o ../bin/seqdex-wallet ./cmd/oceand
```

Run the unit tests (no node required; the cross-chain integration test
self-skips unless it finds a node, see [docs/DEV.md](docs/DEV.md)):

```sh
cd daemon && go test ./internal/seqob/... ./pkg/xchain/...
```

[docs/DEV.md](docs/DEV.md) walks through the full local loop: a two-chain
regtest (parent chain + anchored Sequentia), the wallet daemon, the relay, a
maker, and a taker lift, plus the cross-chain and submarine variants.

## Repo layout

```
daemon/                The Go daemon module (fork of tdex-daemon).
  cmd/seqobd/          SeqOB relay: non-custodial order book + opaque courier.
  cmd/seqob-maker/     SeqOB maker: posts offers, settles lifts (same-chain,
                       cross-chain, submarine). Anyone can run one.
  cmd/seqob-cli/       SeqOB taker CLI: post/book/lift + cross-chain and
                       submarine taker commands.
  cmd/seqob-octl/      Helper for inspecting/preparing Ocean maker accounts.
  cmd/tdexd/           RFQ trade daemon (Trade :9945, Operator :9000, Xchain).
  cmd/tdex/            Upstream operator CLI for tdexd.
  cmd/seqdex-*         Cross-chain takers, demo drivers, regtest helpers.
  internal/seqob/      Order book: offer signing, store, validator, session
                       router, E2E courier client, lift drivers.
  pkg/xchain/          Cross-chain HTLC engine + submarine swaps + regtest
                       harness (testdata/start-regtest.sh).
  api-spec/protobuf/   The authoritative protocol contracts (seqdex.v1,
                       seqob.v1, ocean.v1, legacy tdex-daemon operator protos).
wallet/                Sequentia wallet daemon (thin fork of Ocean); tdexd and
                       seqob-maker settle through it. Builds `oceand`.
proto/                 Standalone phase-1 copy of the seqdex.v1 + ocean.v1
                       contracts with local buf codegen (predates the seqob.v1
                       and xchain additions; daemon/api-spec is current).
docs/                  Design and handover notes: ARCHITECTURE.md (design as
                       built), DEV.md (local runs), the order-book and terminal
                       specs, the covenant-offer design, the rail-crossing and
                       Lightning-latency notes.
test/regtest/          Consensus-level proofs of the covenant order book, run
                       against a real Sequentia node (see its README).
```

Component docs: [daemon/README.md](daemon/README.md),
[wallet/README.md](wallet/README.md).

## Development branches

`main` is the integration branch; PRs target `main`. Two feature branches exist
(states verified 2026-07-08):

- `phase2-submarine-ln`: historical; fully merged into `main` (its head is an
  ancestor of `main`). It carried the submarine-swap work now on `main`.
- `phase3-pure-ln`: `main` plus pure-Lightning swaps (both legs off-chain:
  asset over a SeqLN asset channel against BTC over Lightning, no on-chain
  HTLC), per-asset submarine claim fees with a 0-conf fronting cap, and a
  continuous CLOB matcher with a Go covenant builder for passive resting orders
  (covenant-enforced orders that settle even with the maker offline, proven on
  regtest). None of this is on `main` yet.

## License

MIT, see [LICENSE](LICENSE). SeqDEX is a fork of the
[TDEX](https://github.com/tdex-network) stack (`tdex-daemon`, `tdex-protobuf`)
and the [Ocean](https://github.com/vulpemventures/ocean) wallet daemon; the
upstream MIT notices are retained in [NOTICE](NOTICE).
