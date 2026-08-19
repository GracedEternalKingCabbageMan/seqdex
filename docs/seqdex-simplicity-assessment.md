# Simplicity for SeqDEX — Does It Open Doors, and Which?

## 1. VERDICT — the doors Simplicity actually opens

Simplicity's decisive contribution is **introspection-based covenants**: a Taproot leaf (version `0xbe`) carrying a program that reads the spending transaction's outputs/amounts/assets/locktime and approves only transactions matching committed constraints. That single primitive opens three concrete, high-value doors for SeqDEX, plus one marginal one.

### Door 1 — Funded, self-enforcing resting offers (THE headline win) — FEASIBLE, HIGH VALUE
- **What it enables:** A maker locks asset A in a covenant UTXO whose spend rule is "anyone may spend this only if the tx pays the maker ≥ `rate × filled` of asset B." The order *is* the locked coin. Any taker builds and broadcasts the fill alone; the maker pre-signs nothing at fill time.
- **Why it beats today's model:** Today's offer is an *unfunded signed promise couriered by a trusted relay*. That makes oversell possible (no UTXO backs the quote), requires makers to be online to co-sign, and forces the relay into a trusted-matchmaker role that can fake depth or censor fills. A covenant order eliminates oversell and online-only makers by construction, and demotes the relay from matchmaker to pure discovery/gossip: it can still hide an order, but it cannot fabricate depth that isn't on-chain, nor stop a taker who learns the UTXO from filling it. This attacks limitations (a) relay trust, (b) unfunded offers, and (c) interactivity in one move.
- **Feasibility:** High — conditional on activation (§3) and on the offer being non-confidential (§2). The recursive-covenant pattern is demonstrated by `last_will.simf` and (in hand-rolled Tapscript) by Bitmatrix on Elements; Simplicity makes it statically analyzable and formally verifiable rather than hand-rolled.

### Door 2 — Non-interactive single-tx swap (same-chain) — FEASIBLE
- **What it enables:** The single-fill case of Door 1: one transaction spends the maker's covenant UTXO plus the taker's input and produces the demanded output. Atomicity is the transaction's own atomicity.
- **Why it beats today's model:** Replaces the live blinded-PSET co-sign dance (both parties online, interactive) with a unilateral taker action. Strictly stronger settlement guarantee than a two-party co-sign.
- **Feasibility:** High, same conditions. **Scope limit:** inherently same-chain — a single Sequentia tx cannot also move BTC on Bitcoin.

### Door 3 — On-chain partial-fill (and, harder, batch) matching — FEASIBLE, DESIGN-SUBTLE
- **What it enables:** The fill tx pays the maker the filled portion and returns the remainder to a *fresh copy of the same covenant at the same ratio* (the documented recursive-covenant state-update pattern), using `output_amount`/`output_asset` introspection plus 64-bit arithmetic jets to check `paid ≥ rate × filled` and `change = locked − filled`.
- **Why it beats today's model:** Partial fills become trustless and on-chain rather than relay-mediated bookkeeping.
- **Feasibility:** Partial fill — medium/high. **Batch matching (many orders in one tx) — medium and bug-prone:** each covenant input introspects shared outputs, so you must prevent *output aliasing* (two inputs claiming the same payment output) via fixed per-order output indices or a credit-output convention. This is exactly where Simplicity's Coq-formalized semantics earn their keep.

### Door 4 — Cross-chain HTLC / PTLC / submarine legs — MARGINAL (largely already open without Simplicity)
- **What it enables:** Richer, formally-verified hashlock/timelock/Schnorr-adaptor logic on the *Sequentia* leg only (`htlc.simf` is canonical; `bip_0340_verify` gates adaptor/PTLC claim paths).
- **Why it is only marginal:** The Bitcoin leg has no Simplicity and stays plain Script. PTLCs/adaptor signatures already work cross-chain today — both Bitcoin Taproot and Elements have Schnorr — so Simplicity unlocks no PTLC you couldn't already do. Submarine/LN legs are off-chain and Lightning doesn't run on Sequentia. Net: cleaner Sequentia-side scripts, not a category change. Cross-chain remains two-legged and interactive.

## 2. THE CONFIDENTIALITY VERDICT — covenants cannot read blinded values

**Can Simplicity covenants read blinded amounts/assets? No.** This is confirmed at the type level. Introspection jets return possibly-confidential values as a sum type (from the SimplicityHL compiler's `src/types.rs`):

```
Amount1 = Either<(u1, u256), u64>   // Left = Pedersen commitment point; Right = explicit u64
Asset1  = Either<(u1, u256), u256>  // Left = blinded; Right = explicit asset id
```

For a confidential output the program receives only the **Pedersen commitment point** (parity bit + x-coordinate) — there is **no jet to unblind it on-chain**; range/surjection proofs are exposed only as opaque hashes. To compare an amount/asset against a committed value the program must take the `Right` (explicit) branch. Pinning a *blinded* output to a fixed commitment point would require a fixed blinding factor, which is publicly reconstructible and so defeats confidentiality anyway.

**The explicit tradeoff:** the *enforced legs* of any covenant offer — the resting amount, the asset id, and the credited output — must be **explicit (non-confidential)**. A covenant cannot police an amount it cannot see. Confidentiality and covenant-enforced amounts are mutually exclusive on the same output.

**Is that acceptable, given that outputs may be blinded opt-in (Sequentia is transparent by default)?** Yes, as an *added tier*, not a replacement. The honest framing is a two-tier DEX:
- **Transparent covenant tier:** funded, non-interactive, relay-as-discovery — but resting size/asset are public on-chain (a transparent CLOB). Best for liquid price-discovery markets.
- **Confidential interactive tier (today's path):** blinded PSET co-sign / HTLC, preserving privacy at the cost of liveness and relay trust. Best for size/privacy-sensitive flow.

What you keep regardless: confidential change/unconstrained outputs stay blinded, and the consensus-layer homomorphic CT balance proof (inputs = outputs, no inflation) is untouched by the program. A swap that is *simultaneously* non-interactive **and** confidential is **CLOSED** — accept that and offer both tiers rather than pretending one mode wins. This trade (transparency-for-trustlessness) must be surfaced clearly given Sequentia's privacy ethos.

## 3. STATUS / BLOCKERS

- **On Liquid:** Simplicity is **activated and in production** — Liquid testnet since 9 Oct 2024, Liquid **mainnet since late July 2025** (shipped via the Elements 23.x line, leaf version `0xbe` alongside Script/Taproot). (Note: the Bitcoin Optech topic page reads as pre-deployment and is stale.) Caveat: no public named third-party security audit of the deployed stack was found; assurance rests on the Coq formal verification + Blockstream internal review/fuzzing. Treat external-audit status as unconfirmed before high-value use.
- **On Sequentia — already compiled in, just gated off.** The full Blockstream interpreter + jets are vendored (`src/simplicity/`), built (`Makefile.elementssimplicity.include`), and wired into consensus: `DEPLOYMENT_SIMPLICITY` (`consensus/params.h:37`), `SCRIPT_VERIFY_SIMPLICITY` (`interpreter.h:160`), `0xbe` dispatch (`interpreter.cpp:3355-3365`), gating in `validation.cpp:2098-2099`. Taproot is already `ALWAYS_ACTIVE`, so the `0xbe` substrate works with no prerequisite.
- **The one blocker:** `CSequentiaParams` sets `DEPLOYMENT_SIMPLICITY` to **`NEVER_ACTIVE`** (`chainparams.cpp:582-585`). Enabling it is effectively a one-flag change — set `ALWAYS_ACTIVE` (for a fresh testnet/regenesis) or a dated BIP9 schedule (for coordinated activation). It is a **consensus rule change** (soft-fork-style: makes `0xbe` leaves enforce Simplicity), so it must be coordinated across the producer/committee cluster — best folded into the planned testnet re-genesis, not flipped on a live chain.
- **First-principles check:** Simplicity is purely a tapleaf feature. It does not touch Bitcoin anchoring, the no-coin/open-fee-market model, asset standing, or staking — no conflict with the foundational axioms. (Analysis C's claim that Simplicity is "only on Elements test branches / not on Liquid mainnet" is outdated; per Analysis A it has been on Liquid mainnet since July 2025.)
- **Where interactivity is still required:** (1) **all cross-chain swaps** — the Bitcoin leg has no Simplicity, so the two-leg HTLC/PTLC dance stays interactive; (2) the **confidential tier** — blinding confidential settlement outputs needs both parties' blinding data; (3) carrying order **state** — Simplicity has no persistent storage, so the wallet must re-supply state as witness and re-validate it against a commitment in the next Taproot address (one u256 carry).
- **One new risk surface (anchoring-consistent):** an "anyone-can-fill-at-ratio" UTXO is a public sniping/race target, and a fill confirmed on Sequentia can be undone if its anchoring Bitcoin block is orphaned — at which point a different taker could win, or a taker re-fill at a stale price. This is *correct* behavior (discarding orphaned-anchor blocks is consensus law), but covenant order UX must bound it with maker min-price, locktime expiry (CLTV/CSV via introspection), and a taker finality policy (N Sequentia blocks **and** sufficient Bitcoin anchor depth before treating a fill as final). Front-running mitigation falls on locktime + min-price guards, since the relay no longer arbitrates ordering.

## 4. RECOMMENDATION — worth pursuing, but LATER; experiment now

**Worth pursuing: yes.** Covenant resting offers are a genuine category improvement — they convert an unfunded signed promise into a self-enforcing funded UTXO, fixing relay-trust, oversell, and interactivity for the same-chain case simultaneously. Nothing else on the roadmap does that.

**Now vs later: later, as an additive transparent tier — do not divert Phase-1.** Reasons:
1. It depends on a consensus activation that should ride the **planned testnet re-genesis**, not a live-chain soft fork. That sequencing alone defers it past Phase-1.
2. The Phase-1 order book (relay + interactive co-sign, confidential) remains the **only** path that is both confidential and cross-chain-capable. Covenant orders are same-chain and transparent — a complement, never a replacement. Finishing Phase-1 is still the right near-term priority.
3. Tooling is young (months in production, thin ecosystem, no confirmed external audit) — fine for an experiment, premature for high-value default flow.

**Concrete first experiment (low-cost, high-information), to run on a regtest/testnet with `DEPLOYMENT_SIMPLICITY = ALWAYS_ACTIVE`:**
1. Flip the regtest `CSequentiaParams` deployment to `ALWAYS_ACTIVE`, rebuild, confirm `validation.cpp` sets `SCRIPT_VERIFY_SIMPLICITY` and a trivial `0xbe` leaf validates.
2. Write a minimal **single-fill same-chain covenant offer** in SimplicityHL, modeled on `last_will.simf` + `htlc.simf`: lock N units of explicit asset A; spend permitted only if some output pays ≥ `rate × N` of explicit asset B to a committed `output_script_hash`, with a CLTV refund branch to the maker. Prove a taker can fill it unilaterally and that a wrong-amount/wrong-asset fill is rejected.
3. Then extend to a **partial-fill recursive covenant** (remainder returns to a fresh copy at the same ratio) and deliberately probe the output-aliasing failure mode.

This experiment is cheap, validates the activation path against our own tree, and produces the SimplicityHL artifacts a future transparent covenant tier would build on — without touching the Phase-1 critical path.

**Door status summary:**

| Door | Status |
|---|---|
| Funded self-enforcing resting offers | OPEN, high value (gated on activation + transparency) |
| Non-interactive single-tx swap (same-chain) | OPEN (same gating) |
| Partial-fill / batch covenants | OPEN, design-subtle (aliasing) |
| Cross-chain HTLC/PTLC/submarine | MARGINAL (Bitcoin has no Simplicity; PTLCs already work) |
| Confidential + covenant on same output | CLOSED (introspection can't read blinded values) |
| Activation on Sequentia | gated, one flag — best at testnet re-genesis |

Relevant codebase paths: `the Sequentia node repo: src/chainparams.cpp:582-585` (the `NEVER_ACTIVE` flag to flip), `the Sequentia node repo: src/validation.cpp:2098-2099`, `the Sequentia node repo: src/script/interpreter.cpp:3355-3365`, `the Sequentia node repo: src/simplicity/` (vendored interpreter + jets).