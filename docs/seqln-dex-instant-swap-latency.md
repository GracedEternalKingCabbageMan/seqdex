# SeqLN DEX — making asset↔BTC-LN swaps feel instant (latency design note)

STATUS: design note, to revisit WHEN/just-before wiring the LN-DEX into the wallets (Phoenix-like UX). Not
yet implemented. Captures the 2026-07-03 discussion so we build the fast constructions, not the slow one.

## The problem

The plugin-free submarine swaps that are live today (doc `seqln-phase2-dex-integration.md`) are gated by the
**anchor-depth wait**: the party taking the irreversible Lightning action must wait until the on-chain
Sequentia asset HTLC is buried `min_anchor_depth` Bitcoin blocks (~20–30 min at depth 2–3) before acting.
Lightning makes the BTC leg instant, but the *swap* is gated by Bitcoin confirmations.

Why it's fundamental (and counterintuitive): in a pure on-chain BTC↔asset swap, BOTH legs are Bitcoin-anchored,
so a reorg reverts them together (co-reorg) — that's what lets it settle at ~1 conf. A submarine swap moves
the BTC leg onto Lightning, which does NOT reorg; the co-reorg atomicity is gone, so the seam needs a
standalone confirmation buffer instead. "Instant + on-chain + strong-final" is impossible by anchoring
supremacy (on-chain finality *is* Bitcoin-anchor depth).

## The reframe

Don't try to delete the wait — **move it off the user's critical path onto the professional LP**, and make
the unwind atomic. The achievable target is *instant provisional delivery + trust-minimized atomic unwind*,
which is what real UX needs (the whole 0-conf retail world already runs on this).

## Buying an asset with BTC-LN — SOLVED by the hold-invoice reverse swap (near-term)

The live plain-invoice reverse mode is the SLOW one: a plain invoice is irreversible on payment, so the taker
must wait ~30 min BEFORE paying. The **hold-invoice** reverse mode (task #20; the Boltz structure) is the
LATENCY FIX — I earlier under-sold it as merely "more non-custodial":

1. User picks P, H. LP issues a HOLD invoice on H; user pays it → LN payment is HELD, not settled.
2. LP funds the asset HTLC (claim = user with P).
3. User claims the asset on-chain at **0–1 conf → user has the asset near-instantly.**
4. LP reads P off that claim, waits for it to bury to anchor depth, THEN settles the hold invoice.

The ~30 min lives on step 4 (the LP's settlement), not the user's receipt. Atomic: a reorg before the LP
settles → LP cancels the hold invoice, user refunded. This is the user's "LP interacts at any time" intuition,
achieved by DEFERRED SETTLEMENT (no pre-burying needed). Needs only a stock LN node + the holdinvoice plugin.

Note on literal pre-buried HTLCs (the first thing to reach for): they don't quite work — an HTLC is single-use,
bound to one hash + one claimant, so pre-funding for an unknown future user needs a fresh on-chain tx at trade
time (resets the depth); making it hash-only invites a claim-race / mempool-P-leak theft. Reusable pre-buried
collateral that serves arbitrary counterparties IS a payment channel → this generalizes into asset channels.

## Selling an asset for BTC-LN — harder

No hold-invoice trick (the user receives the irreversible BTC-LN unconditionally). Fast selling needs either:
- **LP fronts BTC-LN at 0-conf** and prices the small reorg risk below a cap — the `LightningTerms.max_0conf_amount`
  field exists for exactly this (Boltz-for-small-amounts). Our submarine driver currently floors
  min_anchor_depth at 2 and does NOT yet honor max_0conf_amount — wiring that is the fast-small-sell path.
- the **pure-LN path** below (the asset already lives in a channel, so the sale never touches an on-chain leg).

## The endgame — asset-aware channels (pure-LN value transfer + decoupled on-chain delivery)

Keep the LP's asset inventory in Sequentia asset-aware Lightning channels. Then:
- **Value transfer = pure-LN atomic swap** (BTC-LN ↔ asset-LN), instant + trustless, both directions. The user
  holds the asset in an asset-LN channel — final within LN, zero reorg risk (nothing on-chain happened).
- **On-chain materialization is DECOUPLED**: the user gets it on-chain whenever, via a cooperative channel close
  or a *same-chain* hold-invoice swap. That same-chain delivery no longer races an irreversible BTC-LN leg
  (the cross-chain atomicity already happened on LN), so it carries only the INTRINSIC Sequentia confirmation
  latency of any on-chain receipt — a non-blocking "your deposit is confirming," not a swap penalty.

The "instant" lives entirely in the pure-LN leg; the chain wait becomes ordinary and off-path. This needs
asset-aware channels (the long pole — being worked next).

## Honest residual trust

The hold-invoice construction is TRUST-MINIMIZED, not fully trustless against reorgs: a malicious LP could
settle the hold invoice early and pray for a reorg (keeping both the BTC and its returned collateral). But it
can't CAUSE a Bitcoin reorg, the payoff only lands on a rare deep reorg, and it burns reputation — same risk
model as 0-conf, priceable. Provable trustlessness-vs-reorg (binding the LN settlement itself to anchor depth,
e.g. PTLC/adaptor secret gated on a buried output) is research-grade; a note, not a blocker.

## Action items for the wallet LN-DEX integration (when we get there)

1. Prioritize the **hold-invoice reverse swap** so BUYING an on-chain asset with BTC-LN is fast + trust-minimized
   (user gets the asset at 0–1 conf; LP absorbs the anchor wait). Prove it live (task #20 unblocks this).
2. Wire **`max_0conf_amount`** so small SELLS are instant (LP fronts BTC-LN, prices the capped risk).
3. Surface finality HONESTLY in the wallet: provisional-at-receipt, strong-final at anchor depth — never "final"
   at 0-conf (DEX 0-conf policy).
4. The truly-instant-both-ways path is the pure-LN endgame → asset-aware channels.
