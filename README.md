# SeqDEX

A non-custodial, atomic-swap DEX for the [Sequentia](https://sequentia.io) Bitcoin
sidechain. SeqDEX is a fork of the [TDEX](https://github.com/tdex-network) stack
(`tdex-daemon` + `tdex-protobuf`) and the [Ocean](https://github.com/vulpemventures/ocean)
wallet daemon, adapted to Sequentia and extended with **native-BTC ↔ Sequentia-asset
cross-chain swaps** that lean on Sequentia's real-time Bitcoin anchoring.

It is a backend product first: a liquidity-provider daemon exposing a language-neutral
trade protocol, meant to be consumed by many client surfaces (the SWK web wallet first,
then the Ambra mobile wallet, a browser-extension wallet, and a desktop app that can run
alongside a Sequentia node).

## Layout

```
proto/      Protocol contract (buf). seqdex.v1 = public Trade + Swap + Transport
            (forked/renamed from tdex.v2, PSETv2). ocean.v1 = wallet backend
            contract, kept byte-identical so the daemon stays drop-in.
daemon/     [phase 3] LP/market-maker daemon — fork of tdex-daemon.
wallet/     [phase 2] Sequentia wallet daemon — thin fork of Ocean.
xchain/     [phase 5] BTC ↔ SEQ cross-chain swap service (Design A, on-chain HTLC,
            anchor-aware finality), abstracted over a LockPrimitive so PTLC can drop in.
docs/       Design docs. See docs/ARCHITECTURE.md.
```

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full design, the fork plan,
and the build phasing.

## Proto codegen

Requires [`buf`](https://buf.build) and Go.

```sh
cd proto
buf dep update
buf generate          # -> proto/gen/go
```

## License

MIT. SeqDEX retains the upstream TDEX and Ocean MIT notices — see [NOTICE](NOTICE).
