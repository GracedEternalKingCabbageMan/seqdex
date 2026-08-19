# SeqLN DEX instant-swap latency — followup: the endgame is built

STATUS: followup to `seqln-dex-instant-swap-latency.md` (2026-07-04). That note framed the problem, the
near-term mitigations (hold-invoice reverse buy, `max_0conf` small sell), and named the true fix — the
"endgame" of asset-aware channels + pure-LN value transfer — as "the long pole, being worked next." It is no
longer aspirational. This followup records how we are actually solving the latency problem end to end.

## Recap of the problem (one paragraph)

Submarine swaps put the BTC leg on Lightning (instant) but keep the asset leg ON-CHAIN, and Lightning does not
reorg, so the co-reorg atomicity that lets a pure on-chain swap settle at ~1 conf is gone. The seam therefore
needs a standalone Bitcoin-anchor-depth buffer (~20-30 min) before the party taking the irreversible LN action
is safe. By anchoring supremacy, "instant + on-chain + strong-final" is impossible: on-chain finality *is*
anchor depth. The mitigations move that wait off the user's critical path onto the LP, but the on-chain leg,
and its intrinsic latency, are still there.

## The solution, stated plainly: nothing goes on-chain per trade

The way to beat "instant + on-chain + strong-final is impossible" is to make the trade **not on-chain at all**.
That is the pure-LN path, and it is now proven:

**1. Pure-LN swaps are DONE (M0-M5, seqdex `phase3-pure-ln`).** Both legs are off-chain Lightning — a Sequentia
asset-LN leg and a BTC-LN leg, stitched by one shared preimage at a translating node. There is NO on-chain leg
and therefore NO anchor-depth gate. Proven live through the seqob order-book relay + encrypted courier: ~2.1s,
both directions (buy the asset with BTC / sell the asset for BTC), atomic refund on failure, and a REAL Bitcoin
**testnet4** BTC-LN leg (M5), not a stand-in. This is genuinely instant AND final AND zero-reorg-risk, because
nothing happened on any chain — the whole "impossible triangle" dissolves once the on-chain leg is removed.
(Detail: `seqln-step2-pure-ln-swaps-design.md`.)

So the endgame's *value-transfer* half — "the user holds the asset in an asset-LN channel, the sale never
touches an on-chain leg" — is real. On-chain materialization stays decoupled and off-path exactly as the
original note described: cooperative close or a same-chain hold-invoice swap, carrying only ordinary Sequentia
confirmation latency, never a swap penalty.

## The obstacle the original note left open: a thin wallet has no channel

Pure-LN needs the user's value to live in a Lightning channel with the user holding the keys. A browser wallet
and a phone cannot run a SeqLN node, so without more they would fall back to the slow submarine path. The
original note ended at "this needs asset-aware channels" without saying how a channel reaches a thin wallet.
That is the piece we have now built.

## The fix for that obstacle: Tier-2 hosted channels via a CLN native signer split (proven)

WE host the SeqLN node (channels, routing, liquidity, watchtower); the user's DEVICE holds the keys and
co-signs every channel state. Non-custodial by construction: the hosted node cannot move the user's funds
without the device's signature, and the user runs no node. This is the Phoenix/Greenlight model, done natively
on our fork. Status (2026-07-04, all committed to `seqln`; detail in `seqln-tier2-hosted-channels-design.md`):

- **The split is a first-class seam** (`--subdaemon=hsmd:PATH`) — no `lightningd` change. **[M0 done]**
- **`hsmd-proxy` ↔ `signerd`** splits the signer out of process; a full channel lifecycle signs out of process
  with valid Elements/asset signatures. **[M1 done]**
- **A Rust device signer** (`contrib/seqln-signer`, I/O-free + WASM-ready) reimplements the hsmd derivations AND
  transaction signing, proven **byte-for-byte** against the reference libhsmd and channel-proven: a node running
  the Rust signer ALONE completed open → payment → mutual close, the peer validating every signature. **[M2
  done]**
- **A network transport** so a HOSTED node with no local key derives its identity from, and signs through, a
  **remote** device signer over the wire — proven live, including on a **GOLD asset channel**. **[transport +
  M3 done]**

So the missing link — a non-custodial asset-LN channel a thin wallet can actually hold — exists and works.

## Where this leaves the latency picture

| construction | on-chain per trade | felt latency | strong-final | reorg risk | needs on device |
| --- | --- | --- | --- | --- | --- |
| plain-invoice submarine (old) | asset leg | ~30 min BEFORE acting | at anchor depth | seam risk until buried | nothing |
| hold-invoice reverse buy | asset leg | instant receipt, LP absorbs wait | at anchor depth | priced, capped | nothing |
| `max_0conf` small sell | asset leg | instant, capped | at anchor depth | priced, capped | nothing |
| **pure-LN + hosted channel** | **none** | **instant** | **immediately (off-chain)** | **none** | **only the keys (hosted-channel signer)** |

The submarine + hold-invoice constructions do not disappear — they become the **fallback**: for a user who has
no channel yet (first trade / pay-to-open pending), or a trade above the channel's capacity, or a counterparty
who only speaks on-chain. The pure-LN hosted-channel path is the **primary**, truly-instant one. Honest-finality
UX still applies to the fallbacks (provisional at receipt, strong-final at anchor depth, never "final" at
0-conf); the pure-LN path is the only state that may legitimately be shown final.

## What remains before it is shippable to users

The mechanism is proven; making it a product is the current work, in order:
1. **Secure the transport.** The device↔hosted link is raw TCP today (topology proof only). Production needs a
   Noise/TLS + per-wallet-auth channel before the device signer holds real keys. (Blocking for production.)
2. **Validating-signer policy (M4).** The device should refuse theft-shaped signatures (value conservation,
   output-destination whitelist), not merely sign on request — this is where "the device co-signs" earns its
   keep. Plus the ECDH hot-path latency decision (ECDH sits on peer-connect + every onion hop).
3. **Recovery / escape hatch (M5).** static-remotekey + SCB + peer-storage so a user whose hosted node vanishes
   can force-close and sweep with only their device.
4. **Hosted-channel LSP infra.** JIT / pay-to-open liquidity on both networks so a brand-new wallet gets inbound
   capacity, plus the wallet-facing API to open a channel + trade.
5. **Wallet integration.** The device signer as WASM (web) / Rust FFI (Ambra) + the SDK that runs the signer,
   connects the (secured) transport to the hosted node, and drives pure-LN trades. Under the LSP model in the
   UX audit (`ux-audit-spec-2026-07-02.md` §8): users never run a node.

## The one-line answer

We solved instant-swap latency by making the trade off-chain entirely (pure-LN, proven M0-M5 incl. real
testnet4), and we made that reachable from a thin, non-custodial wallet by hosting the node while the device
holds the keys (the CLN native signer split, proven end-to-end over the network). The anchor-depth wait does
not get hidden better; for the primary path it stops existing.
