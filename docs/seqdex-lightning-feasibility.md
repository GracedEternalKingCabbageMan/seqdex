# Lightning on Sequentia: Pure-LN and Submarine Swaps for Assets ↔ BTC — Feasibility, Safety, and DEX Implications

## 1. Direct answer — "Possible?" per scenario

**(a) Pure-LN swap, both sides off-chain (Sequentia-asset LN ↔ Bitcoin LN): YES-WITH-CAVEATS.** The atomicity primitive (one shared HTLC/PTLC secret reused across two independent LN payments, stitched at a translating node) is mechanically sound and already demonstrated for LN↔LN cases. The binding caveat is the asset leg: carrying an *issued asset* (not native BTC) over Lightning needs asset-aware channels, which were specced-but-never-shipped on Liquid; until that exists, the asset side cannot ride LN.

**(b) Submarine swap, one side LN / one on-chain (Sequentia-asset on-chain ↔ BTC over LN): YES.** This is the realistic near-term case. The on-chain HTLC locks a Sequentia asset (Elements script, asset-agnostic) and the off-chain leg is plain BTC over vanilla Lightning — exactly the Boltz normal/reverse-swap construction with the asset replacing on-chain BTC. No consensus change, no asset-LN dependency, no BOLT change.

**(c) Truly instant / 0-conf: YES-WITH-CAVEATS.** The Lightning leg is genuinely sub-second and, once the preimage is exchanged, that knowledge is permanent. But the moment a secret bridges Lightning and the *anchored Sequentia ledger*, the secret-revealing on-chain claim must reach Bitcoin-anchor depth before the other leg is settled (Principle 1). So "instant" is honest for the LN leg and for same-chain single-tx swaps; it is NOT honest for the cross-leg point where one party relies on a Sequentia on-chain claim as proof the secret is committed.

**(d) Disintermediated: YES (custody) / NO (liquidity).** The shared-secret HTLC + hold-invoice construction is fully non-custodial and trustless — no party can abscond; worst case is a timelock refund. But *someone* must supply liquidity on both networks; that role is a non-custodial market-maker / liquidity provider, not a trusted escrow. Custody is disintermediated; the need for a capitalized counterparty is structural and cannot be removed.

---

## 2. Lightning-on-Sequentia feasibility (asset-denominated channels) — Analysis A

Running Lightning on Sequentia is technically feasible for **single-asset channels** because every script Lightning needs (HTLC hashlock + CLTV, `to_self_delay` via CSV, revocation/penalty branch, anchor outputs, PTLC/Taproot) is plain Bitcoin Script, and Elements/Sequentia is a strict superset. It was done in limited form on Liquid in 2019 (c-lightning `--network liquid`, L-BTC only). **CORRECTION (2026-07-02 research): CLN did NOT drop Liquid** — Elements support (`liquid`/`liquid-regtest` in `bitcoin/chainparams.c`) is in-tree and CI-tested against Elements Core 23.2.1; it is L-BTC-only and dormant, not removed. What actually ships in production Liquid LN is still **swaps, not channels** (Blockstream's Boltz integration; PeerSwap). See the full fork spec `seqln-core-lightning-fork-spec.md`.

What it takes, in increasing difficulty:

- **Native Sequence-token channels:** essentially the 2019 L-BTC experiment — standard BOLT scripts, no new cryptography. Sequentia's open fee market is not a problem: on-chain LN txs (funding/force-close) pay proposer fees in any accepted asset; only the pre-signed commitment fee accounting must track the chosen fee asset.
- **Issued-asset channels:** *easier on-chain* than Bitcoin (Elements outputs are natively asset-tagged, so no client-side-validation overlay à la Taproot Assets/RGB), but *harder in the spec*. The BOLTs have no notion of an asset id; channel open, commitment construction, dust/trim thresholds, HTLC value math, and gossip/routing all assume a single implicit unit. Adding asset-awareness plus cross-asset routing is the bulk of the work — exactly the part Blockstream listed as "planned" and never delivered.
- **The genuine blocker — Confidential Transactions inside the commitment machinery.** CT-in-channel needs deterministic blinding (both parties must reconstruct byte-identical commitment txs), balloons every output with range/surjection proofs (large, expensive force-close), clashes with Elements' explicit-fee model vs BOLT-3's implicit-fee math, and buys little privacy (the counterparty already knows the balance). Every prior implementation deferred it; the pragmatic answer is **unblinded/explicit-amount channels**.

**Concrete recommendation:** the most viable path now is **Lightning↔asset HTLC swaps** (Boltz/PeerSwap model), which need no consensus or BOLT change and align with existing SeqDEX cross-chain HTLC work. Native single-asset channels (unblinded) are feasible with engineering. Confidential in-channel amounts are lowest-priority.

---

## 3. THE critical anchoring ↔ LN safety result (load-bearing) — Analysis B

**Verdict: Sequentia's lockstep reorg-following does NOT break Lightning — it HELPS, and is a net security upgrade over a generic sidechain.** There is no novel penalty-evasion attack. The single mechanical fact that drives every answer: a Sequentia reorg is always a **tail truncation** — the watcher invalidates the lowest-height block whose anchor went stale and disconnects it plus all descendants; anchor height is monotonic, so the orphaned set is always a contiguous suffix. You can never un-confirm an earlier Sequentia tx while keeping a later one confirmed.

Three structural properties:
- **(P1) Causal order preserved across reorgs.** If tx B spends tx A, B's height > A's; any reorg disconnecting B also disconnects A. They revert together into the mempool.
- **(P2) A reorg RESETS the relative-timelock clock.** A disconnected commitment loses its confirmation height, so its CSV counter restarts from zero on re-mining — a reorg re-extends the defender's window, never shortens it.
- **(P3) No Sequentia-only reorg exists.** Reverting certified history requires an actual Bitcoin reorg (Bitcoin-grade cost). Committee misbehavior threatens liveness only, never history. A generic PoW/PoS sidechain lacks this and could mount cheap short reorgs to grief LN. Secrets (revocation key, preimage) are off-chain and survive any reorg.

**The penalty-evasion / revoked-state-revival attack fails.** Bob force-closes with revoked `C_old` at height H_C; Alice's justice tx `J` confirms at H_J ≥ H_C + to_self_delay (strictly later). For Bob to orphan `J` but keep `C_old` matured is impossible by P1: any Bitcoin reorg deep enough to orphan `J` necessarily orphans `C_old` too (H_C < H_J). Both fall to the mempool, CSV resets (P2), Alice re-broadcasts `J` with a *full fresh* window. Bob ends strictly worse off. The cooperative-close-revival variant degenerates into an ordinary fresh force-close with the revocation secret intact.

**The only residual at-risk window is identical to Bitcoin LN:** defender offline beyond `to_self_delay`, or the block producers censoring the justice tx for the entire CSV window. Anchoring does not widen this; it only changes the *units*.

**Exact confirmation-depth + timelock policies (tied to Bitcoin-anchor depth):**

1. **Funding / channel-open finality:** treat funding final only when its block's Bitcoin **anchor has ≥ N Bitcoin confirmations** (N = 3–6). Implement `min_depth` as anchor confirmations on bitcoind (via `getanchorstatus` → anchor block confs), **never** as Sequentia tip-distance. Set `anchorminconf ≥ 1`.
2. **`to_self_delay` (CSV):** denominate in Sequentia blocks, size by wall-clock. Because a reorg resets the counter (P2), the dominant term is the ordinary LN response window plus headroom for the ~6–7× block-count amplification. Match Bitcoin LN's ~1-day default ⇒ `to_self_delay ≈ 960` blocks; recommend **~1000–2000 blocks (~1–2 days)** for routing nodes.
3. **Per-hop CLTV delta:** size to exceed Bitcoin's recommended delta in wall-clock (Bitcoin's ~40 blocks ≈ 6.7 h ⇒ **~270 Sequentia blocks**). For hops crossing a Bitcoin channel and a Sequentia channel, compute the absolute-CLTV gap in **wall-clock**, normalizing each leg by its own chain's block time. Do **not** add a cross-chain reorg buffer — the legs reorg in lockstep — but **do** normalize cadence.
4. **Watchtower policy:** re-derive and re-broadcast `J`/HTLC claims after every Bitcoin-driven reorg; time CSV from post-reorg re-confirmation; during a production stall, counters pause for attacker and defender alike (relative-timelock guarantee preserved); hold a committee-accepted fee asset so claims are includable under the open fee market.

**The one real footgun is denomination:** reckon depth as Bitcoin-anchor confirmations, and size timelocks in wall-clock accounting for the ~6–7× Sequentia/Bitcoin block-rate ratio. Get the units right and LN-on-Sequentia is as safe as LN-on-Bitcoin, and strictly safer cross-chain (both legs share one finality domain — Bitcoin).

---

## 4. Pure-LN cross-network atomic swap design — Analysis C

**Mechanism.** Both legs are off-chain LN payments on two *separate* Lightning networks (disjoint routing graphs). Atomicity comes from a single shared secret reused across both legs: HTLC version locks both to the same hash `H(x)` (both chains share SHA256); PTLC version locks both to the same point `T = x·G` (both have Schnorr/Taproot). The glue is a **hold invoice** plus a **translating node** ("Interchain Router") running an LN node on each network. For asset→BTC: taker pays the maker on Sequentia LN against a hold invoice on `H(x)` (locked, unsettled); the maker pays a BTC invoice on Bitcoin LN on the *same* `H(x)`; the BTC recipient releases `x` to claim; the maker uses the now-known `x` to settle the held Sequentia leg. The maker cannot take the asset leg without revealing `x`, and revealing `x` is exactly what pays the BTC leg. It is NOT one onion across both networks — each leg is independently onion-routed and stitched at the maker by the shared secret, which costs some privacy (each side knows the other's identity on at least one network). Timelock laddering now spans two LN networks: the maker's incoming (held) leg must carry a longer CLTV than the outgoing leg it pays. PTLCs are the better long-term target (no hash reuse, no wormhole attack, scriptless).

**Is an LP needed, and is it non-custodial?** You need a counterparty with liquidity on *both* networks — there is no way to conjure cross-network liquidity. But it is **liquidity provision, not custody**: the construction guarantees the LP cannot take one leg without settling the other. Boltz is the existence proof (non-custodial, no accounts, no balance custody). The LP can refuse a quote but can never abscond; worst case is a refund at timelock. A pure no-LP P2P swap is possible only in the rare case where both end users already hold opposite-network liquidity and route circularly.

**Instant-ness.** The per-swap happy path is genuinely instant — both legs are off-chain LN payments settling in hundreds of milliseconds, with **no on-chain transaction in the success path at all**, hence no 0-conf risk in the usual sense. The only non-instant touchpoints are out-of-band: channel funding (amortized infrastructure) and the rare dispute/refund path that falls back on-chain to the HTLC timeout — which is exactly where timelock laddering and anchoring protect you. Anchoring is irrelevant to the off-chain happy path; on the on-chain touchpoints it *aligns* both sides' finality to one domain (Bitcoin), which is stronger than a generic two-chain swap with independent reorg risks.

---

## 5. Submarine swap design — Analysis D

**Mechanism.** One Lightning (off-chain) leg atomically linked to one on-chain HTLC leg via a single shared preimage. The on-chain HTLC has a claim path (32-byte preimage `P` where `H = SHA256(P)` + receiver sig, spends immediately) and a refund path (CLTV timeout, funder's sig). The same `H` gates the LN HTLC, so revealing `P` to claim one leg exposes it for the other. Two directions: **normal swap (on-chain → LN)** the LN invoice issuer holds the secret; **reverse swap (LN → on-chain)** uses a **hold invoice** so the on-chain receiver generates and retains `P` — this is the important pattern for the DEX. Safety invariant: the leg claimed *second* (using the now-public secret) must have the *longer* timelock. **Case A (Sequentia asset on-chain ↔ BTC over vanilla LN)** is the v1 target — works today, no asset-LN needed. **Case B (Sequentia asset over LN ↔ BTC on-chain)** needs asset-aware Lightning; without it, Case B collapses into the on-chain↔on-chain chain swap the DEX already does.

**DEX makers as disintermediated counterparties.** A submarine swap is fundamentally a 2-party atomic swap (one on-chain HTLC, one LN HTLC); nothing requires a dedicated swap service. So **the maker plays the Boltz role** — runs a Lightning node, issues the hold invoice (or pays the BOLT11), and is the on-chain HTLC counterparty. The relay stays a pure non-custodial matchmaker. The maker is "just a well-capitalized peer." Honest caveats: the LN *receiver* role needs an LN node + inbound liquidity (a web-wallet user can be the LN payer but not easily the receiver); griefing / free-option risk is real (a party holding the secret or a long-timelock leg has a free option to abort), which is why reputable capitalized makers will dominate for liveness — not for trust; PTLCs are the more private/efficient future direction but not production-ready.

**0-conf risk and where anchor depth applies.** Two independent reversal mechanisms:
- **Mempool/RBF double-spend** — identical to Bitcoin, present on the Sequentia leg too. Mitigate exactly as Boltz: reject RBF (explicit and inherited), never broadcast RBF-signaling txs you fund, fee floor ≥ 80% of estimate, per-pair 0-conf amount caps. (Note Boltz disabled 0-conf on Bitcoin mainnet entirely under widespread `mempoolfullrbf`.)
- **Anchoring reorg — unique to Sequentia and stronger.** A Sequentia tx is final only to its **Bitcoin-anchor depth**, not its Sequentia confirmations; even a multi-conf Sequentia tx is discarded if its anchoring Bitcoin block is orphaned. The hazard is the cross-leg secret leak: `P` becomes public when the Sequentia asset leg is claimed on-chain; if the counterparty acts on it to pull BTC/LN and Bitcoin then reorgs out the anchor of the Sequentia claim, the asset claim un-happens while BTC has settled — atomicity broken. This is DEX review blocker **B4**: a cross-chain `min_conf` default of 1 is unsafe.

Net rule: the LN leg is safe to lean on as instant; the Sequentia leg is only safe-instant inside a single atomic same-chain tx. The moment a secret bridges LN/Bitcoin and the anchored Sequentia ledger, **the secret-revealing claim must reach Bitcoin-anchor depth (N > plausible reorg depth) before the other leg settles** — the maker must not settle the held BTC/LN invoice until the taker's asset-claim tx is anchor-deep. Anchoring's *benefit*: timelock slack is bounded by Bitcoin reorg depth (not the long conservative buffers two independent PoW chains need), so you get tighter, faster-safe settlement than a generic atomic swap — "tighter than generic" is not "zero."

---

## 6. DEX integration — extending the order-book DEX to LN

The existing design (`docs/seqdex-orderbook-design.md`) already carries an **intent, not a PSET**, in resting offers, with a `oneof settlement` (same_chain / cross_chain) and a relay that couriers opaque swap-session messages. A submarine swap is a third settlement variant that reuses all of that plumbing.

**Offer schema** — add a Lightning variant to the `settlement` oneof:
```protobuf
oneof settlement {
  SameChainTerms  same_chain  = 20;
  CrossChainTerms cross_chain = 21;
  LightningTerms  lightning   = 22;   // NEW
}
message LightningTerms {
  Direction ln_direction        = 1;  // ASSET_ONCHAIN_FOR_BTC_LN | BTC_LN_FOR_ASSET_ONCHAIN
  bytes  maker_ln_node_pubkey   = 2;  // the LP role
  repeated string ln_connect_hints = 3;
  bytes  maker_claim_pub        = 4;
  bytes  maker_refund_pub       = 5;
  uint32 onchain_cltv           = 6;  // CLTV on the asset HTLC (shorter leg)
  bool   maker_issues_hold_invoice = 7;
  uint64 max_0conf_amount       = 8;  // per-offer 0-conf cap (Boltz getpairs)
  uint32 min_anchor_depth       = 9;  // Bitcoin-anchor confs on secret-revealing
                                      //  claim before settling LN; MUST be > reorg floor
}
```
Crucially, `H` is **not** baked into the resting offer (just like `CrossChainTerms`); the secret-holder generates `P` at lift. The maker signs the offer fields to authenticate.

**Relay (courier)** — add one opaque courier message (`ln_swap_msg` To/From), end-to-end encrypted to the counterparty (review B1) so the relay sees only ciphertext + session id, never `P`, never settles anything. The relay needs **no LN node**; it reuses read-only Sequentia + Bitcoin RPC to verify the asset-HTLC funding and **check anchor depth** before marking the offer FILLED (review B3: don't declare FILLED at 1 conf; add a reorg watcher that re-opens the offer if the anchor is orphaned).

**Maker model** — the maker becomes the swap counterparty (the Boltz role collapsed into the LP): runs an LN node with hold-invoice support and inbound liquidity, issues/settles/cancels hold invoices, pays BOLT11, bridges the LN HTLC to the on-chain Sequentia HTLC. This is the genuinely new operational dependency.

**Reuse vs new:** reused as-is — offer-as-intent, the `oneof`, the relay lift-session router and To/From envelope, the HTLC primitive (`pkg/xchain/primitive.go`), preimage extraction (`pkg/xchain/maker.go ExtractPreimage`), the `xswap.js` wizard shape, and any-asset fee logic (Principle 4 untouched). Genuinely new — a `pkg/lnswap` talking to an LN node (LND/CLN gRPC) for hold invoices + BOLT11; validator additions (LN reachability, enforce `max_0conf_amount`, enforce `min_anchor_depth ≥ reorg floor`, reject 0/1).

**What to bake into Phase 1 NOW for LN-extensibility (keeping the on-chain core intact):**
1. **Settlement-type extensibility:** ensure `settlement` is a true `oneof` with reserved high field numbers (20/21 used, 22+ free) so adding `LightningTerms` later is additive and non-breaking. Sign the *whole* settlement oneof now so future variants are authenticated by construction.
2. **Anchor-depth as a first-class, per-offer field — not a hardcoded constant.** Phase 1's cross-chain path should already carry a `min_anchor_depth` (or equivalent) and enforce `> reorg floor` (reject 0/1, review B4). This is shared infrastructure LN reuses verbatim and is the single most important safety lever; building it now means LN inherits a proven anchor-depth gate and reorg-watcher rather than retrofitting one.
3. **Relay courier as an opaque, E2E-encrypted, session-bound envelope (review B1):** if the Phase 1 `SwapMsg`/`XchainMsg` couriers are already opaque ciphertext + session id with no payload introspection, the LN courier is "just another message type" — no relay trust changes.
4. **Maker descriptor with an optional, ignorable LN endpoint stub:** reserve fields (`maker_ln_node_pubkey`, `ln_connect_hints`) in the maker/offer descriptor now (unset, unused in Phase 1). Costs nothing, avoids a schema migration later.
5. **HTLC primitive kept asset-agnostic** (already true in `pkg/xchain/primitive.go`) so the LN submarine path locks Sequentia assets with no new script work.

Do NOT build LN node integration, hold-invoice plumbing, or `pkg/lnswap` in Phase 1 — keep the on-chain core (same-chain PSET co-sign + on-chain↔on-chain HTLC) intact and shippable. The Phase 1 ask is purely *schema/relay/validator extensibility*, not LN functionality.

---

## 7. The honest "truly instant" story

Be truthful per Principle 1 — never claim irreversible if an on-chain leg can reorg.

**Genuinely instant / 0-conf-safe (you may call this "instant"):**
- **The Lightning leg itself** — sub-second, and once `P` is exchanged the preimage knowledge is permanent; LN settlement does not reverse on a Sequentia anchor reorg (LN rides on Bitcoin L2, independent of Sequentia's anchor).
- **Same-chain Sequentia↔Sequentia atomic swaps in a single PSET** — both legs in one tx reverse together on any reorg, so 0-conf carries only ordinary RBF risk, no cross-leg atomicity hazard.
- **Pure-LN both-sides swaps (happy path)** — no on-chain tx in the success path at all.

**Anchor depth is load-bearing (do NOT call this "instant" / "final"):**
- Any point where one party relies on a **Sequentia on-chain claim as proof the secret is safely committed.** The secret-revealing claim must reach **N Bitcoin confirmations of its anchor** (N > plausible Bitcoin reorg depth) before the counterparty settles the other leg. Forbid `min_conf = 1` / 0-conf for the cross-leg path.

**How to present it to users:** distinguish "**instant payment**" (the LN leg / the trade execution feel) from "**settlement finality**" (when an on-chain leg is irreversible). For pure-LN both-sides and same-chain single-tx swaps, "instant and final" is honest. For submarine swaps, present the swap as "instant to execute" but show a finality indicator on the on-chain leg tied to **Bitcoin-anchor confirmations**, not Sequentia block count — e.g. "settling: 1/3 Bitcoin-anchor confirmations." Never display a Sequentia-confirmation count as finality. Frame Sequentia's advantage accurately: *faster safe settlement than a generic cross-chain swap* (timelocks bounded by Bitcoin reorg depth), not "zero-confirmation final."

---

## 8. Phased plan to add Lightning, with blockers called out

**Phase 0 — Make the DEX LN-extensible (do now; on-chain only, no LN code).** Settlement `oneof` with reserved fields + whole-oneof signing; per-offer `min_anchor_depth` enforced `> reorg floor` (review B4) with a reorg-watcher that re-opens offers on anchor orphan (B3); opaque E2E-encrypted relay couriers (B1); reserved maker LN-endpoint fields (unset); asset-agnostic HTLC primitive confirmed. *Blockers: none — purely additive schema/validator work. Keeps Phase 1 on-chain core intact.*

**Phase 1 — Submarine swaps, Case A (Sequentia asset on-chain ↔ BTC over vanilla LN).** Highest value, lowest dependency, no consensus/BOLT change. Build `pkg/lnswap` (LND/CLN gRPC): hold invoices, BOLT11 pay, bridge LN HTLC ↔ on-chain Sequentia HTLC. Makers run an LN node + inbound liquidity (the LP role). Reuse the existing HTLC primitive and preimage extraction. Enforce anchor-depth gate before settling the held invoice; copy Boltz 0-conf mitigations (RBF reject, fee floor, per-offer caps). *Blockers: makers must operate LN nodes with hold-invoice support + inbound liquidity; griefing/free-option risk (mitigate with reputation/capitalization and conservative timelocks); 0-conf must be capped and anchor-gated, never default-on for the cross-leg path.*

**Phase 2 — Native single-asset LN channels on Sequentia (unblinded).** Replicate and finish the 2019 Liquid path for the native Sequence token first, then issued assets, with **explicit/unblinded amounts**. Requires asset-id awareness in channel open / commitment / dust-trim / HTLC math / routing, plus adapting Elements' explicit-fee model to BOLT-3. Apply the anchor-depth funding policy and wall-clock-sized `to_self_delay`/CLTV from §3. *Blockers: the BOLTs have no asset-id concept — adding asset-aware channels + cross-asset routing is the bulk of the engineering and is the part Blockstream specced and never shipped; explicit-fee vs implicit-fee accounting rework; watchtowers must re-derive justice txs after Bitcoin-driven reorgs.*

**Phase 3 — Pure-LN cross-network atomic swaps (Sequentia-asset LN ↔ Bitcoin LN).** Depends on Phase 2 (asset must move over LN). Run a translating node / Interchain Router with an LN node on each network; bind two independently-routed LN payments by one shared secret (HTLC now, PTLC target); ladder timelocks across two LN networks. Fully non-custodial; needs a dual-network-capitalized LP. *Blockers: asset-aware Lightning must exist (Phase 2); thin cross-network liquidity; HTLC hash-reuse correlation/wormhole risk until PTLCs are production-ready; privacy cost of identity exposure across networks.*

**Permanently deferred / lowest priority — Confidential amounts inside channels.** Technically possible (deterministic blinding + range/surjection proofs) but heavy on tx weight and force-close cost for little privacy gain (the counterparty already knows the balance). Every prior implementation deferred it. Ship unblinded.

**Cross-cutting hard constraints (all phases):** denominate confirmation depth as **Bitcoin-anchor confirmations** (via `getanchorstatus` + bitcoind), never Sequentia tip-distance; size `to_self_delay`/CLTV in **wall-clock**, accounting for the ~6–7× Sequentia/Bitcoin block-rate ratio; defenders must hold a **committee-accepted fee asset** so penalty/claim txs are includable under the open fee market; standard LN watchtower/online liveness assumptions apply. None of these are anchoring bugs — they are the price of getting the units right, after which LN-on-Sequentia is as safe as LN-on-Bitcoin and strictly safer cross-chain.

Grounding files: `the Sequentia node repo: doc/sequentia/03-bitcoin-anchoring.md` (tail-truncation watcher §3, real-time/finality §4, swaps §6); `the Sequentia node repo: doc/sequentia/04-proof-of-stake.md` (finality/fork-choice); `the Sequentia node repo: src/anchor.cpp`, `the Sequentia node repo: src/anchor.h`; `docs/seqdex-orderbook-design.md` (offer schema §2, relay envelope/lift session §3, cross-chain HTLC reuse §6, blockers B1/B3/B4).