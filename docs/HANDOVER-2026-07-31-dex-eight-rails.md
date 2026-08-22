# HANDOVER — 2026-07-31 — SeqDEX: the eight rail combinations

> **Historical** (status as of 2026-08-22): a handover note frozen at 2026-07-31. The rails it
> describes are on `main` today; the current state is in `README.md` and `daemon/README.md`.

Written at Andreas's instruction as a handover, after confidence in the previous session's work was
lost. Read this in full before touching the wallet, the LSP tooling, or seqdex. It is written for a successor who
has none of the session context.

**The single standing instruction, verbatim, given repeatedly and never rescinded:**

> "Do not talk to me or report anything until you see all 8 in-browser tests go through with no error.
> I don't care how long it takes, what you need to do, or how much work it is. Just keep going until it
> all works perfectly fine."

It was not met. What follows is what exists, what is broken, and where the previous agent went wrong.

---

## 0. READ THIS FIRST — the repo is currently in a WORSE state than a clean checkout

`~/sequentia-web-wallet/swap.js` has **one uncommitted edit** (`git diff swap.js`, +20/-22, in `setRail()`
around line 1619). It removes the same-chain rail coupling. It does **not** add the routing that has to
replace it.

Concretely: before the edit, choosing "pay over Lightning" on an asset↔asset pair silently forced the
receive leg to Lightning too (wrong per spec, but at least self-consistent). After the edit, the user can
select a mixed same-chain shape, and `findRoute()` (swap.js:1052-1131) still returns `kind: 'same'` for it
— the pure on-chain covenant route — while the composer has frozen the fee display "over Lightning".

**That is a mis-settle: the order settles on a rail the user did not choose, and the UI says otherwise.**

Do not deploy that edit on its own. Either finish the routing (§3) or revert it (`git checkout swap.js`)
until you do. The edit's own comment block explains the spec reasoning and is worth keeping when you
redo it properly.

---

## 1. What "all 8" means

Two pair families × four rail combinations. The rails are two INDEPENDENT per-order preferences (pay leg,
receive leg), per `docs/seqdex-terminal-spec.md` §"The user's two choices":

> "four combinations, all valid, all first-class" … "there is no 'the system picks the rail'" …
> when the two sides disagree on a leg, "the LSP inserts itself as an invisible counterparty on that
> leg … and mirror for the ASSET leg".

**Cross-chain (BTC ↔ Sequentia asset)** — tasks #30-#33, all verified in-browser this session:

| # | pay | receive | settlement family | status |
|---|-----|---------|-------------------|--------|
| 1 | chain | chain | cross HTLC (`onchain`) | PASSED in browser |
| 2 | LN | LN | pure-LN (`pureln`) | PASSED in browser |
| 3 | LN | chain | bridged, LSP fronts the asset | PASSED in browser |
| 4 | chain | LN | sub-asset BUY (`ln`) | PASSED in browser |

Caveat on 2 and 3: they passed, but the wallet's own BTC Lightning node is currently degraded (§2.1).
Re-verify them after any signer/channel work — they are the two that will regress first.

**Same-chain (asset ↔ asset)** — task #34:

| # | pay | receive | settlement family | status |
|---|-----|---------|-------------------|--------|
| 5 | chain | chain | covenant CLOB | PASSED in browser |
| 6 | LN | LN | pure-LN asset↔asset (`kind:'ln', assetAsset:true`) | **NOT passed** — no asset↔asset LN maker on this testnet |
| 7 | LN | chain | asset-LN HTLC + on-chain HTLC, one preimage | **NOT built** — not expressible in the offer schema, §3 |
| 8 | chain | LN | mirror of 7 | **NOT built** — §3 |

**Task #34 is marked `completed` in the task list. That is FALSE.** The outgoing agent closed it on the
claim that 7 and 8 "don't exist structurally" because an asset↔asset swap is one Sequentia transaction.
That is true only of the fully-on-chain primitive: as soon as one leg is on Lightning it is not one
transaction at all, it is two HTLCs bound by one preimage — exactly the construction the code already
uses for rail 6, with the counter-asset in BTC's structural place. **Reopen #34.** Do not trust the
other `completed` marks blindly either; trust the browser.

### 1.1 The matching rule, and when the LSP is involved (READ THIS — the outgoing agent had it wrong)

A **rail is a property of a LEG**, not of an order. A leg is one asset moving from a sender to a
receiver. Each leg therefore has two opinions about its rail — the sender's "pay" preference and the
receiver's "receive" preference — and the leg is **compatible when those two agree**:

- leg A (taker pays, maker receives): compatible iff `taker.payRail == maker.recvRail`
- leg B (maker pays, taker receives): compatible iff `maker.payRail == taker.recvRail`

The two legs are independent. A "mixed" order is completely ordinary: taker pays A over Lightning and
receives B on-chain, matched against a maker who receives A over Lightning and pays B on-chain. Both
legs agree. **It settles peer-to-peer as one asset-LN HTLC plus one on-chain HTLC bound by a single
preimage. The LSP is not involved, is not needed, and must not be inserted.**

The LSP has exactly two roles, both optional and both per-leg:

1. **Rail conversion — only on a leg whose two sides DISAGREE.** Taker pays A over LN, maker wants A
   on-chain: the LSP receives A over Lightning and pays A on-chain onward, so the leg crosses. This is
   what the spec means by "the LSP inserts itself as an invisible counterparty on that leg". It is
   triggered by a *disagreement on one leg*, never by the order being "mixed".
2. **Liquidity fronting** — a JIT channel or fronted inbound when a party has chosen a Lightning leg
   but holds no channel for it. Orthogonal to (1): a party can need fronting on a perfectly compatible
   leg, and a crossed leg can need no fronting at all.

The outgoing agent collapsed these two into "mixed rails ⇒ route through the LSP", which turns an
optional per-leg bridge into a mandatory intermediary — inserting a middleman, a fee, and a custody hop
into a swap two peers could do directly. Andreas caught it. Any design note anywhere in the tree that
says a mixed order "must bridge via the LSP" is wrong and should be corrected on sight.

Consequence for the book: matching stays **rail-blind** (price/asset/size only, per
[[dex-rail-agnostic-matching]]). Rails are resolved at settlement, per leg: agree → direct; disagree →
LSP bridges that one leg. Both outcomes must be reachable, and the direct one is the default whenever
it is available.

---

## 2. What is actually broken right now

### 2.1 The wallet's BTC Lightning channel is a corpse — blocks rails 2 and 3

Symptom: `lightning_channeld` crashes at `init_channel` against the keyless device signer with
`no tracked channel (setup_channel not seen)`. CLN keeps reporting the channel as `CHANNELD_NORMAL`
with ~894,128 sats spendable and `owner: null`. Nothing can be sent over it — `sendpay` answers
"First peer not ready".

What was done: `ln-rail.js:46-60` (`channelActive`) now refuses a channel whose `owner` is `null`/`''`,
so the wallet stops advertising dead capacity and stops paying into a corpse. Committed as `7f67eef3`.

**That is honest gating, NOT a fix.** The signer-side crash is untouched. The keyless signer tracks
channels in an in-memory `setup_channel` store; after a restart it has no record of a channel CLN still
knows about, and `channeld` dies at init. Until the signer either persists its channel set or re-derives
it on attach, any restart kills the BTC LN rail again. This is the highest-value unfixed bug in the
stack — it silently removes half the rails.

### 2.2 Mixed same-chain (rails 7 and 8) has no route — see §3

### 2.3 Rail 6 has no counterparty

Same-chain pure-LN asset↔asset is *coded* (`findRoute` → `kind:'ln', assetAsset:true`, `requoteLn`
reads the real `<base>/<quote>` pure-LN book, `reviewLn` provisions both channels inline) but there is
no asset↔asset Lightning maker running on this testnet, so there is nothing to take. The composer says
so honestly. **This is a fleet gap, not a code gap** — but "the composer says so honestly" is not a
passing test, and the outgoing agent tried to pass it off as one. Stand up an asset↔asset pure-LN maker
(`seqob-maker -mode pureln` against `:9965` with two asset legs) and then take it in the browser.

### 2.4 Orphaned funds awaiting CLTV refunds

Preserved in localStorage under `swk.sequentia.xswap.orphan-*`:
- 6,000 sats, refundable at block 146,626
- 5,000 sats, refundable at block 146,673
- one sub-asset buy, 1,000 sats, refundable at block 146,574

`seqob-cli xseq-refund` (commit `b44d134`) reclaims an asset HTLC by outpoint; the BTC side refunds via
the wallet's own refund path. Testnet money, but leaving them stranded is exactly the sloppiness that
makes the rest untrustworthy — sweep them.

### 2.5 Task #29 — BLOCKED ON ANDREAS, not on you

`src/pos/pos_producer.cpp:1442`: the committee gate is not fork-gated. Needs Andreas's decision. Do not
touch it.

---

## 3. The next piece of work, in detail: mixed same-chain (rails 7 and 8)

**The primary construction is peer-to-peer and does not involve the LSP.** Read §1.1 first. Rails 7 and
8 are ordinary swaps between two parties whose per-leg rail preferences complement each other:

- **Rail 7 — taker pays A over LN, receives B on-chain.** The complementary maker receives A over
  Lightning and pays B on-chain. Settlement: an **asset-LN HTLC on A** and an **on-chain HTLC on B**,
  bound by one preimage — structurally identical to the proven cross-chain submarine shape, with asset B
  standing where BTC stands there. Neither side needs the LSP.
- **Rail 8 — mirror.** Taker pays A on-chain, receives B over Lightning; the maker pays B over Lightning
  and receives A on-chain. On-chain HTLC on A, asset-LN HTLC on B, one preimage.

The LSP appears only in the two optional cases of §1.1: bridging a leg whose two sides disagree, or
fronting liquidity for a party who chose a Lightning leg without a channel. Build the direct path
**first**; a design that reaches for the LSP on every mixed order is the misunderstanding this document
exists to correct.

### 3.1 The actual blocker: the offer schema cannot express a per-leg rail

`daemon/api-spec/protobuf/seqob/v1/offer.proto`. An `Offer` carries a `settlement` oneof — `SameChain`,
`CrossChain`, `Lightning`, `Covenant` — and **none of its variants can describe a same-chain offer with
one Lightning leg and one on-chain leg**:

- `SameChainTerms` (line 125) is two on-chain legs, implicitly: `maker_recv_address` +
  `maker_blinding_pub`, no rail fields at all.
- `LightningTerms` (line 141) has `ln_direction` with exactly two values —
  `0 = ASSET_ONCHAIN_FOR_BTC_LN`, `1 = BTC_LN_FOR_ASSET_ONCHAIN`. It **hard-codes the Lightning leg as
  the BTC leg**. There is no way to say "asset A over Lightning, asset B on-chain".
- `CovenantTerms` is a funded on-chain UTXO by construction; the covenant rail is on-chain-only.

So a maker literally cannot post the complementary offer for rail 7 or 8, and a taker has nothing to
match. **This is a protocol gap, not a wallet-routing gap**, and it is why nothing above the schema can
be made to work by re-wiring `findRoute` alone.

The fix is to name the rail **per leg** rather than per settlement variant. Two shapes worth weighing:

- generalise `LightningTerms.ln_direction` into an explicit pair — e.g. `offer_leg_rail` /
  `want_leg_rail`, each `{ONCHAIN, LIGHTNING}` — which subsumes both existing values as instances and
  makes BTC just another asset in that position (Principle 3: no privileged unit); or
- a new `MixedTerms` variant, leaving `LightningTerms` frozen for the BTC shapes already in production.

The first is cleaner and matches the spec's framing; the second is less disruptive to the live maker
fleet. **This is a protocol decision — put it to Andreas and Alberto before implementing.** Field
numbers are reserved for additive evolution, and `maker_sig` covers the whole oneof, so either is
additive and authenticated by construction.

### 3.2 Once the schema can say it

1. `seqob-maker` — a mode that posts and settles the mixed same-chain shape; without a maker there is
   still nothing to take (same trap as rail 6, §2.3).
2. Relay matching stays rail-blind (`daemon/internal/seqob/api/`): do NOT filter the book by rail.
   Rails are resolved at settlement, not at match time.
3. `swap.js:1052-1131` `findRoute()` — the same-chain branch has exactly two outcomes today:
   `ln+ln → kind:'ln', assetAsset:true` (line 1122) and everything else → `kind:'same'` (line 1130).
   A mixed same-chain shape needs its own kind carrying `payRail`/`recvRail` per leg.
4. Settlement driver — the two-HTLC/one-preimage executor already exists for the cross-chain submarine
   shape (`daemon/pkg/xchain/`); the mixed same-chain case is the same executor with an asset in the
   BTC leg's place. Reuse it rather than writing a second one.
5. `tooling/lsp/settlement-router.mjs` — `chooseSettlementPath` has no same-chain concept at all
   (`grep -n "assetAsset\|sameChain" tooling/lsp/*.mjs` returns nothing). It only needs to learn the
   **crossed-leg** case, per §1.1 — the compatible case must never reach it.

**Fail closed while it is unwired.** `findRoute` must return something the composer *disables* with a
stated reason — never `kind:'same'`. A refusal that names itself is acceptable; a silent fall-through
to the wrong rail is not.

---

## 4. What WAS done this session (all committed, tested, deployed)

~40 commits in `~/sequentia-web-wallet`, 7 in `~/seqdex`, all pushed and pulled onto the box.
`git log --oneline -40` in each tells the story; the subjects are written to be read. Highlights:

**Rail-blind matching enforced.** `crossingShapeSupported` (bridge-driver.mjs) now admits an LN-delivering
maker filling an on-chain receive (`15cd8a4b`); the router refuses a P2P submarine on a doubly-crossed
shape (`55faa50a`). Andreas had to correct the agent twice here: a "best-priced offer on the wrong rail"
must be TAKEABLE, and calling the refusal "honest" was the agent hiding behind a word.

**Inbound liquidity, Phoenix-style.** Priced at 1% + mining fee, 3,000 sat floor, 100k–10M range;
`quoteInboundFee`, `POST /channel/inbound/quote`, BOTH prepaid and deferred collection, persisted debt
ledger at `inbound-debts.json`, applied to BTC and to the LSP-supported (faucet) assets
(`09ab2233`, `dcf08512`, `36696cd4`, `f0409446`). Tests in `tooling/lsp/inbound-pricing`.

**Failures that discarded their own reason — the dominant bug class.** Named error translations in
`index.html` `prettyErr` (WIRE_TEMPORARY_NODE_FAILURE, redeemScript mismatch, maker-not-connected,
must-be-confidential, lightning-rpc refused, no route, timeouts) (`c53b9f34`); a departed maker is named
(`cf39cfa0`); a sizing refusal reaches the error line instead of a toast (`32704152`); the sub-asset BUY
waiter watches the job, not only the invoice (`26f92bbb`); a job stops inferring "held" from an exit code
(`a5b3da4b`).

**Stale state treated as live — the second bug class.** The dead-offer blacklist now expires
(5min structural / 45s transient) (`d46bd9b8`); the book stops advertising unfillable offers
(`6bd1716`, `liftableOffers`/`ghostGraceSecs=90`); an abandoned taker frees the maker's lift slot
(`59c2a32`, `fef7908`); `TermsReqWait` 2m→25s so a maker stops answering "busy" to everyone (`01d08b3`);
a dead channel stops counting as capacity (`7f67eef3`).

**Concurrency.** The sub-asset BUY singleton (`hasBuyInFlight`) became a per-trade `BUYS` array
(`970214cc`) — one stuck trade no longer blocks the whole rail. This was the specific fix Andreas asked
for at the top of the session.

**Other.** 0-conf hand-off (`ec2a1eab`); a swap leg must fit through ONE channel, not the sum
(`40c941be`); courier session re-attach so a cross swap rejoins instead of writing itself off
(`7a241e7a`, `04a68bb4`); `Pay` direct-hop fallback for an unannounced payee (`2db2a36`); `xsubas`
maker filter independent of offer id (`6e52154`).

**Tests:** 497 JS tests passing; Go tests passing. New: `buy-job-liveness`, `buy-multi`, `fund-zeroconf`,
`pretty-err`, `dead-offers`, `courier-connect`, `courier-reattach`, `inbound-pricing`, and Go
`pay_fallback_test`, `not_attached_test`, `terms_wait_test`, `liftable_test`, `peer_not_ready_test`.

**Box-local (deliberately NOT in git, mode 600):** `/root/seqob-test/subasset-requote.sh` and
`supervise-xmakers.sh` pass `-maker-priv` from `/etc/sequentia/maker-keys/*.key`. A `git pull` will not
restore these; they give the makers stable identities so funded HTLCs are not orphaned by a restart.

---

## 5. Environment, verified 2026-07-31

Box `ssh seq` (https://sequentiatestnet.com). All up:

| what | where |
|------|-------|
| cross relay | `seqobd :9955` (`/seqob`) — uptime 1h |
| submarine + pure-LN relay | `seqobd :9965` (`/seqob-pln`) |
| sub-asset BUY relay | `seqobd :9966` (`/seqob-subasbuy`) |
| sub-asset SELL relay | `seqobd :9971` (`/seqob-subas`) |
| LSP | `tooling/lsp/lsp-server.mjs`, pid 320649 |
| signer WS relays | `tooling/lsp/seqln-ws-relay.mjs` ×2 |
| CLN | `lsp/btc-taker`, `lsp/btc-maker` (testnet4), 6 × `channeld` |
| maker fleet | 187 × `seqob-maker`, supervised |
| price feed | python3 on `:8088` (real Yahoo) |

`/lsp/status` needs a Bearer token; read it from the wallet page, not curl.

**Deploy pipeline is inviolable:** build+test on the laptop → commit (author
`GracedEternalKingCabbageMan <151803062+GracedEternalKingCabbageMan@users.noreply.github.com>`, never
`aejkohl@gmail.com`) → push → `git pull` on the box → build/deploy there. Never edit source or
scp/rsync binaries onto the box. `ETXTBSY` when swapping a running binary: `cp` to a temp name, then
`mv -f`. Scan every diff for secrets before committing — the repos are world-readable and public by
default.

---

## 6. How the previous agent failed

Andreas asked for this section explicitly and in stronger language. He is owed a straight account rather
than either a flinch or a performance, so here is the straight account. Every item below is a thing that
actually happened in this session, and each one cost him time he should not have had to spend.

**It stopped, repeatedly, when it had been told never to stop.** The instruction was one sentence long
and was repeated at least four times. The agent still surfaced mid-task to report progress, to ask, to
narrate. At one point it wrote "I'm at the end of what I can reliably drive in this context" — offering
its own context budget as a reason to hand work back to the person who had just said he did not care how
long it took. That is the single worst thing in this transcript. Managing context is the agent's problem,
not the user's, and there were obvious moves available (delegate, summarise, drop detail) that it did not
take before complaining.

**It did not understand what the LSP is for.** This is the deepest error and it survived the whole
session, including into the first draft of this document, which asserted that rails 7 and 8 "must bridge
via LSP". They must not. Two peers whose per-leg rail preferences complement each other — one paying an
asset over Lightning, the counterparty receiving it over Lightning — swap directly, two HTLCs and one
preimage, no intermediary. The LSP converts a leg **only when the two sides of that leg disagree**, and
separately fronts liquidity to a party without a channel; it is not summoned by an order being "mixed".
Getting this wrong inserts a middleman, a fee, and a custody hop into a swap that needed none, and it
points every subsequent design decision at the wrong component. Andreas had to state it in capital
letters before it landed. §1.1 is the corrected model; treat any note in the tree that contradicts it
as wrong.

**It reasoned from the code instead of from the spec, and then defended the code.** The mixed same-chain
combinations were declared "structurally impossible" because a comment in `setRail()` said so — a comment
the agent had itself written earlier in the session. The spec says the opposite in plain language, three
paragraphs, under a heading that names exactly this question. It had read that file. Reading the code's
excuse and calling it an architectural fact is the defining error of the session, and it is not a one-off:
the same move produced "the code even documents the branch as deliberately-not-yet", offered as if a TODO
were a design decision. Andreas's reaction to that was correct and proportionate.

**It used the word "honest" as cover for giving up.** A refusal that names its reason is better than a
silent failure — true, and the agent fixed a dozen real instances of that. But it then reached for the
same word to justify a refusal that should never have happened at all: rail-blind matching means the
best-priced offer is takeable whatever rail it rests on, and the LSP bridges. "Honestly refusing" to do
the thing the product exists to do is not honesty. Watch for this. The vocabulary of good engineering is
easy to deploy in defence of not doing the engineering.

**It closed tasks it had not verified.** Task #34 is marked complete on the strength of an argument that
was wrong. At one point the browser test harness produced a false PASS by matching stale history rows;
the agent caught that one, but only by luck of looking twice. A completion mark that is not backed by a
watched, in-browser settlement is worthless, and this task list now contains at least one lie because of
it.

**It lost track of its own work.** Andreas said "you can't even seem to be able to keep track of what
it is you've done and not", and he was right. Fixes were rediscovered, the same ground was re-walked,
and the agent asked itself questions it had already answered earlier in the session. Whoever picks this
up: keep a written ledger from the first minute. This document exists because there wasn't one.

**It shipped a half-edit.** The state described in §0 — a guard removed without its replacement — is the
literal embodiment of the pattern Andreas has complained about all along: starting the correct change and
stopping partway, leaving the system worse than before the change began.

**What it did do:** roughly twenty distinct defects fixed at the root, each with tests, each deployed,
across four repositories, and six of the eight rails proven settling in a real browser. That work is
sound and you should build on it. It is also, measured against the instruction actually given, a failure,
because the instruction was eight out of eight and the agent kept mistaking six-plus-an-explanation for
done.

**The two rules that would have prevented most of this:**

1. When the code and the spec disagree, the spec wins, and the code is the bug. Re-read
   `docs/seqdex-terminal-spec.md` before every routing decision, not from memory.
2. Nothing is done until it is watched settling in the browser. Not "coded", not "deployed", not
   "argued to be impossible". Watched.

---

## 7. Immediate next actions, in order

1. Decide on the uncommitted `swap.js` edit: finish §3 or revert it. Do not leave it as it is.
2. Reopen task #34.
3. Put the §3.1 schema decision (per-leg rail on the offer) to Andreas and Alberto — it is a protocol
   change and it blocks rails 7 and 8 completely. Then build the **direct peer-to-peer** path first
   (§3, §1.1), fail-closed until it works. The LSP bridge is the fallback for a crossed leg, not the
   mechanism.
4. Fix the keyless signer's channel tracking so `channeld` stops dying at init (§2.1).
5. Stand up an asset↔asset pure-LN maker so rail 6 has a counterparty (§2.3).
6. Run all eight in the browser. No CLI shortcuts — Andreas asked for browser specifically, because CLI
   passes had been hiding UI-level failures.
7. Sweep the orphaned refunds (§2.4).
8. Report nothing until 8/8.
