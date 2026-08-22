# Sequentia P2P Order-Book DEX — Implementable Design

> **Historical** (status as of 2026-08-22): the design this order book was built from. It is
> implemented on `main` (`daemon/internal/seqob`); where it and the code differ, the code and
> `README.md` are authoritative.

Status: design for review. No consensus change. Reuses the proven seqdex same-chain PSET co-signing and cross-chain HTLC settlement verbatim; adds a non-custodial order-book relay and a maker/taker order layer borrowing SideSwap's wire shape.

---

## 1. ARCHITECTURE OVERVIEW

### Design stance (from the four analyses)

- Analysis A: the seqdex PSET 3-phase swap (`SwapRequest → SwapAccept → SwapComplete`) is already party-symmetric and operator-agnostic; the only thing that is "operator-specific" is the *market/pricing/liquidity* layer (CPMM, fee accounts, slippage). We discard that layer and keep the swap + wallet plumbing.
- Analysis B: two settlement primitives exist, are tested, and are the things we MUST NOT rebuild — same-chain confidential PSET co-sign (`daemon/pkg/swap/*`, `daemon/pkg/trade/wallet.go`) and cross-chain HTLC (`daemon/pkg/xchain/*`).
- Analysis C: SideSwap proves the correct *relay* shape — a dumb, non-custodial matchmaker + PSET courier; an offer is a price+size+expiry promise that carries NO PSET/UTXO at post time (joint CT blinding must happen at settlement); the `two_step`/`private_id` offline-order trick lets a relay match an offline maker; `HistStatus` (`UTXO_INVALIDATED`, `REPLACED`, `ELAPSED`) is the exact stale-offer state machine a UTXO book needs.
- Analysis D: the web wallet already has the taker "propose → sign → complete" path and the cross-chain wizard; we reuse them and add browse-book / post-offer / manage-orders screens.

### Key architectural decision: the offer carries an *intent*, not a PSET

Following Analysis C's most load-bearing finding: a resting offer is a signed price/size/expiry/pair/keys promise. The PSET (same-chain) or HTLC legs (cross-chain) are constructed only at lift time, exactly as in seqdex `TradePropose/CompleteSwap`. This is mandatory for confidentiality: joint blinding requires both parties' blinding data, which only exists once a counterparty is known.

This means the relay never holds funds, never holds a PSET that can move funds, and cannot front-run — it holds signed text.

### Components

```
                         ┌──────────────────────────────────────────────┐
                         │   seq-orderbook RELAY  (NEW, Go)              │
                         │   non-custodial: stores+serves SIGNED offers  │
                         │   + couriers lift/swap messages between peers │
                         │                                               │
                         │   • OfferStore (per-pair book, snapshot+delta)│
                         │   • WS/gRPC pub-sub (To/From envelopes)       │
                         │   • Lift session router (relays swap.proto    │
                         │     Request/Accept/Complete between 2 peers)  │
                         │   • Validator (sig check, UTXO-liveness probe,│
                         │     expiry, rate-limit, anti-grief bond)      │
                         │   NEVER signs, NEVER custodies, NEVER blinds  │
                         └───▲───────────────▲───────────────▲──────────┘
            post/cancel/     │   stream book │     lift /     │  relay swap msgs
            edit (signed)    │  (snap+delta) │  swap session  │
                             │               │                │
        ┌────────────────────┴───┐   ┌───────┴────────┐  ┌────┴───────────────────┐
        │  MAKER client          │   │  Browser /     │  │  TAKER client          │
        │  (wallet OR our MM)    │   │  any watcher   │  │  (wallet)              │
        │                        │   │                │  │                        │
        │  seqdex libs (vendored)│   │                │  │  seqdex libs (vendored)│
        │  • pkg/swap (same-chain│   │                │  │  • pkg/swap            │
        │  • pkg/xchain (HTLC)   │   │                │  │  • pkg/xchain          │
        │  • wallet CompleteSwap  │   │                │  │  • wallet ProposeSwap  │
        │  signs OWN inputs only  │   │                │  │  signs OWN inputs only │
        └───────────┬────────────┘   └────────────────┘  └───────────┬───────────┘
                    │                                                  │
                    │   each party signs its own inputs; broadcasts    │
                    ▼                                                  ▼
        ┌──────────────────────────────────────────────────────────────────────┐
        │   Sequentia node (sequentia-qt) + Bitcoin testnet4 node (cross-chain)   │
        │   settled tx is anchor-bound to Bitcoin (Principle 1) — no change      │
        └──────────────────────────────────────────────────────────────────────┘
```

### Data flow, two cases

Same-chain lift:
1. Maker posts signed offer → relay stores in book for `pair`.
2. Watchers stream book (snapshot + deltas).
3. Taker picks an offer, opens a lift session via relay.
4. Relay opens a session and pipes the seqdex swap 3-phase between the two peers. Taker builds `SwapRequest` PSET half (its inputs + output to maker addr). Maker runs the exact `CompleteSwap` logic (Analysis A §2) → `SwapAccept`. Taker signs its inputs → `SwapComplete`. Taker broadcasts.
5. Tx confirms, anchor-bound. Relay marks offer `FILLED`/`PARTIAL`, removes/decrements from book.

Cross-chain lift (BTC↔asset, both directions):
1. Maker posts a cross-chain offer carrying its claim/refund pubkeys + locktime intent for its leg.
2. Taker lifts; relay opens a session piping the `pkg/xchain` orchestration (Lock → Lock → Verify anchor → Claim → Claim) messages. Settlement bytes never go custodial; relay only relays the HTLC params + funding txids.

---

## 2. OFFER SCHEMA

An offer is a signed, self-contained intent. Protobuf (relay wire) + canonical JSON (web wallet). One schema covers same-chain and cross-chain via a `settlement` oneof.

```protobuf
// proto/seqob/v1/offer.proto   (NEW)
message Offer {
  // Identity
  string  offer_id        = 1;  // 16-byte hex, maker-chosen, unique
  uint32  schema_version  = 2;  // = 1

  // Pair + terms (the price/size promise — NO UTXO/PSET here)
  AssetPair pair          = 3;  // {base_asset hex, quote_asset hex}; for X-chain, base or quote = BTC sentinel id
  TradeDir  trade_dir     = 4;  // SELL=1 (maker sells base), BUY=2 (maker buys base)
  uint64  base_amount     = 5;  // total size, base atoms
  double  price           = 6;  // quote per 1.0 base (display/sort); authoritative ratio = want/offer below
  uint64  offer_amount    = 7;  // exact atoms the maker will give
  string  offer_asset     = 8;  // hex (= base or quote depending on dir)
  uint64  want_amount     = 9;  // exact atoms the maker wants in return
  string  want_asset      = 10; // hex
  bool    allow_partial   = 11; // partial fills permitted
  uint64  min_fill        = 12; // min base atoms per lift (anti-dust)

  // Lifecycle
  uint64  created_at_unix = 13;
  uint64  expires_at_unix = 14; // 0 = until-cancelled (but see liveness probe §5)
  uint64  ttl_seconds     = 15; // soft expiry the relay enforces if expires_at=0

  // Maker keys / endpoints
  string  maker_pubkey    = 16; // 33-byte compressed; identity + sig key for this offer
  bool    online_required = 17; // true => maker co-signs live at lift (default)
  bool    two_step        = 18; // pre-signed offline order (see §5); rarely used v1

  // Fee preference (open any-asset fee market — hint only, per Principle 4)
  string  fee_asset_hint  = 19; // maker's preferred fee asset; taker/whitelist may override

  // Settlement-specific
  oneof settlement {
    SameChainTerms  same_chain  = 20;
    CrossChainTerms cross_chain = 21;
  }

  // Authentication
  bytes   maker_sig       = 31; // secp256k1 sig over sha256(canonical serialization of fields 1..21) by maker_pubkey
}

message SameChainTerms {
  // What the maker needs to receive the want_asset confidentially.
  string maker_recv_address  = 1; // confidential addr where taker pays want_asset
  string maker_blinding_pub  = 2; // so taker can blind the maker's output (mirrors seqdex unblinded-input handshake)
  // For two_step only: pre-signed maker half lives in the relay's lift session, NOT in the public offer.
}

message CrossChainTerms {
  string btc_sentinel        = 1; // which leg is BTC (base|quote)
  string maker_claim_pub     = 2; // maker claims its incoming leg
  string maker_refund_pub    = 3; // maker refunds its funded leg
  uint32 maker_leg_locktime  = 4; // maker's CLTV (longer if maker funds BTC)
  string maker_recv_address  = 5; // where maker receives the asset leg (if maker buys asset)
  uint32 min_conf            = 6; // confs maker requires on counterparty funding (default 1, anchor-shortened)
  Direction direction        = 7; // BTC_TO_ASSET | ASSET_TO_BTC  (reverse-direction maker work, §6)
}
```

How signed: the maker serializes fields 1..21 canonically (deterministic protobuf or sorted-key JSON), sha256, signs with `maker_pubkey`'s private key, fills `maker_sig`. The relay and any watcher verify this. Editing price/size = new signature (an `OrderEdit` is a re-signed offer with the same `offer_id`, monotonically increasing `created_at_unix`).

What it carries vs. seqdex: it carries the off-chain offer data Analysis B lists as "must be known to both parties" — pair, amounts, assets, maker recv address + blinding key (same-chain), or H-less HTLC params (cross-chain: pubkeys, locktimes; the hashlock `H` is generated by the taker/secret-holder at lift, not baked into the resting offer). It deliberately carries NO PSET, NO UTXO outpoints (online orders), matching Analysis C's "offer-as-price-promise."

---

## 3. RELAY / SERVER — non-custodial order book

### Role (strictly bounded)

Stores and serves signed offers; routes the swap-session messages between two peers; verifies signatures, expiry, liveness, rate limits. It never holds keys, never signs, never blinds, never holds a spendable PSET, never broadcasts user funds. If the relay vanishes, no funds are at risk; makers just re-post.

### Stack

Go, vendoring the seqdex `daemon/pkg/swap`, `daemon/pkg/trade`, `daemon/pkg/xchain` packages and the `proto/seqdex/v1/swap.proto` / `types.proto` messages (Analysis A, B file lists). Justification: (1) the swap/HTLC code we courier is Go and we want to validate message well-formedness using the same parsers (`pkg/swap/request.go`, `accept.go`, `complete.go`); (2) seqdex already builds with this toolchain; (3) reusing `seqdex` libs is an explicit requirement. New service name: `seqobd` (Sequentia Order-Book Daemon) in repo `seqdex` under `cmd/seqobd/` (or a sibling repo `seqob` if the owner prefers a clean split; recommend keeping in `seqdex` to share `go.mod` and the swap/xchain packages without a vendoring dance).

The relay also needs RPC read-only access to a Sequentia node (UTXO-liveness probe, §5) and the testnet4 Bitcoin node (cross-chain funding confirmation). It uses the existing node RPC creds from config; no wallet.

### Wire envelope

Borrow SideSwap's `To`/`From` `oneof` envelope (Analysis C §1.borrow) over WebSocket (browser) and gRPC (Go clients). Keep the message-numbering convention.

```protobuf
// proto/seqob/v1/relay.proto  (NEW)
message To {                       // client -> relay
  oneof msg {
    AssetPair    market_subscribe   = 100;  // stream this pair's book
    Empty        market_unsubscribe = 101;
    Offer        offer_submit       = 102;  // post (signed)
    Offer        offer_edit         = 103;  // re-signed, same offer_id
    OfferCancel  offer_cancel       = 104;  // {offer_id, sig}
    StartLift    start_lift         = 113;  // taker lifts an offer
    SwapMsg      swap_msg           = 130;  // courier: Request/Accept/Complete/Fail
    XchainMsg    xchain_msg         = 140;  // courier: HTLC lock/verify/claim notices
    Empty        list_markets       = 150;
  }
}
message From {                     // relay -> client
  oneof msg {
    MarketList   market_list        = 150;
    PublicBook   public_orders      = 105;  // per-pair snapshot
    Offer        public_order_created= 106; // delta
    OfferId      public_order_removed= 107; // delta
    OwnOrders    own_orders         = 120;
    LiftAccepted lift_accepted      = 142;  // session opened, peer endpoint bound
    SwapMsg      swap_msg           = 131;  // courier passthrough
    XchainMsg    xchain_msg         = 141;
    OfferStatus  order_status       = 160;  // OPEN|PARTIAL|FILLED|CANCELLED|EXPIRED|UTXO_INVALIDATED|REPLACED
    GenericError error              = 200;
  }
}
```

`SwapMsg`/`XchainMsg` wrap the existing seqdex `SwapRequest`/`SwapAccept`/`SwapComplete`/`SwapFail` (swap.proto) and xchain leg notices — the relay passes them opaquely between the two session peers, validating only that they parse and reference a live session.

### API (concrete)

- `POST /v1/offers` (or `To.offer_submit`) — body = signed `Offer`. Relay verifies sig, schema, expiry sane, balance-claim plausibility via liveness probe, rate limit. Returns `offer_id` + status `OPEN`. NON-CUSTODIAL: stores text only.
- `GET /v1/markets` — list pairs with `{base, quote, best_bid, best_ask, last_price, depth, n_orders}`. Aggregated from live offers (no operator reserves).
- `GET /v1/market/{base}/{quote}/orderbook?depth=N` and `WS .../stream` — snapshot (`public_orders`) then deltas (`public_order_created`/`removed`). Matches Analysis D Screen 2.
- `GET /v1/market/{base}/{quote}/order/{offer_id}` — full offer for lift.
- `POST /v1/lift` (or `To.start_lift`) `{offer_id, taker_amount, taker_fee_asset}` — relay checks offer still OPEN + liveness, opens a session, returns a session id and binds the two peers; from here the relay only relays `swap_msg`/`xchain_msg`.
- `POST /v1/offers/cancel` (or `To.offer_cancel`) `{offer_id, sig}` — sig by `maker_pubkey`. Removes from book; emits `public_order_removed`.
- `GET /v1/offers?maker_pubkey=...` — maker's own orders (Analysis D Screen 5).

### Abuse prevention

- Offer validity: reject if `maker_sig` invalid, `expires_at`/`ttl` absurd, amounts zero, assets unknown to registry, or `offer_amount/want_amount` disagrees with `price` beyond rounding.
- UTXO/balance liveness probe (§5): on submit and periodically, the relay checks (read-only) that the maker's address(es) for `offer_asset` plausibly hold ≥ `offer_amount`. For online orders the offer doesn't name UTXOs, so the probe is over the maker's declared `maker_recv_address`-linked scan or a maker-supplied (optional) `funding_hint` outpoint set; if the probe fails the offer is flagged `UTXO_INVALIDATED` and dropped. This is best-effort (confidential balances limit precision) — the real guarantee is atomic co-signing: a lift against a maker with no funds simply fails to co-sign, costing the taker nothing on-chain.
- Expiry: relay enforces `expires_at_unix` or `ttl_seconds`; expired offers emit `ELAPSED`/`EXPIRED` and are removed.
- Rate-limit: per `maker_pubkey` and per IP — N offers/min, M edits/min, K open offers/pair. Cheap to post text, so cap aggressively.
- Anti-griefing (failed lifts): a maker that repeatedly opens sessions then refuses to co-sign wastes taker time. Mitigations: (1) per-pubkey lift-success ratio; demote/temp-ban makers below a threshold; (2) short session co-sign deadline (mirror seqdex trade expiry, Analysis A `TradeComplete` "check not expired"); (3) optional anti-spam: requiring offers to reference a recent Bitcoin-anchored block height as freshness (no consensus change, just a recency token). No bonds in v1 (keep it permissionless and simple); reputation is purely informational and client-side-overridable.

---

## 4. MATCHING

### v1 — taker lifts a chosen resting offer

The taker browses the book, picks one offer, and lifts it. This is the simplest correct model and maps directly onto seqdex's existing `TradePropose/TradeComplete` (Analysis A §1) — the maker plays the "responder/CompleteSwap" role, the taker plays "proposer."

Lift → settle handshake (same-chain), reusing the swap PSET co-signing verbatim:

```
Taker                         Relay (courier)                 Maker
  | start_lift{offer_id, amt} -->                              
  |                             -- checks offer OPEN+live -->  
  |                             -- lift_accepted (session) --> | (maker online)
  | build SwapRequest:                                          
  |  select taker UTXOs (asset_p),                              
  |  output want->maker_recv_address,                          
  |  unblinded_inputs                                           
  | swap_msg{SwapRequest} ----> -- relay --------------------> |
  |                                                            | run CompleteSwap
  |                                                            |  (wallet/service.go
  |                                                            |   227-422): add own
  |                                                            |  UTXOs, derive change,
  |                                                            |  any-asset fee vout,
  |                                                            |  UpdatePset, BlindPset
  |                                                            |  with taker unblinded,
  |                                                            |  SignPset (own inputs)
  | <----------- relay --------- <-- swap_msg{SwapAccept} -----|
  | Blind own inputs, SignPset                                 
  |  own inputs, ValidateAll                                   
  | broadcast (self) OR                                        
  | swap_msg{SwapComplete} ----> -- relay (optional broadcast)-> 
  | <-- txid / order_status{FILLED|PARTIAL} ------------------ 
```

This is exactly the 3-phase protocol from Analysis A §2 / Analysis B Part 1, with the relay substituted for the "operator" as a dumb courier. No settlement code changes.

Partial fills: if `allow_partial`, the taker's `taker_amount < base_amount`; the maker's `CompleteSwap` selects only enough UTXOs and returns change; relay decrements the offer's `active_amount` (SideSwap `OwnOrder.active_amount`, Analysis C §2) and re-broadcasts the delta. The offer's signature still covers the original terms; the ratio is preserved, so a partial is just a smaller co-sign at the same price.

### v2 — price-time auto-match

Add a matching engine in the relay that, when a new offer crosses resting offers on the other side (bid ≥ ask), automatically opens a lift session between the two makers (one becomes taker). Price-time priority: best price first, then oldest `created_at_unix`. The settlement handshake is identical; only the trigger differs (engine-initiated vs. taker-initiated). Both makers must be online (or `two_step`). The engine never holds funds — it just nominates a proposer and opens the session.

---

## 5. UTXO RESERVATION + EXPIRY MODEL

The hard part of a UTXO order book (Analysis C §4). Approach, online-first:

Online orders (default, `online_required=true`): the offer reserves NOTHING on the relay. The maker keeps its coins free until a lift session opens; only then does its `CompleteSwap` call `SelectUtxos` (Analysis A §3.1), which locks UTXOs in the maker's own Ocean/LWK wallet for the session's lifetime. Consequences:
- No double-spend of a reserved input across sessions because the wallet locks at select-time and the relay serializes lift sessions per offer (one active session at a time per offer; queue others).
- Spent-before-match: if the maker spent the coins elsewhere between posting and lift, `SelectUtxos` fails → maker returns `SwapFail` → relay marks `UTXO_INVALIDATED`, drops the offer, notifies the taker (no on-chain cost). This is the `UTXO_INVALIDATED` state borrowed from SideSwap `HistStatus`.
- The taker's coins are likewise locked only during its own session.

Expiry has three layers:
1. Offer expiry: `expires_at_unix` / `ttl_seconds`, enforced by the relay → `EXPIRED`/`ELAPSED`.
2. Session co-sign deadline: each lift session has a short deadline (reuse seqdex trade expiry; `TradeComplete` already rejects expired, Analysis A §1). Miss it → session aborts, wallet locks released, offer returns to `OPEN`.
3. Wallet UTXO lock TTL: Ocean/LWK `SelectUtxos` returns a lock-expiration (Analysis A §3.1); set it ≥ session deadline so locks auto-release.

Liveness probe (relay, best-effort): periodic read-only check that the maker's offered asset balance still plausibly covers open offers; demote/flag if not. Confidential amounts make this imperfect — the atomic co-sign is the real safety net, the probe just keeps the book clean.

Optional two-step / offline orders (v2+, borrowed from SideSwap): the maker pre-signs its half and hands it to the relay session store keyed by `offer_id`+`private_id`; a taker lifts via `private_id`. Here the maker DOES commit specific UTXOs (named in the pre-signed half). The relay must then probe those exact outpoints for liveness and mark `UTXO_INVALIDATED`/`REPLACED` (RBF) if spent. Defer to v2 to keep v1 trust-minimized and simple.

---

## 6. SETTLEMENT INTEGRATION (cite the seqdex code)

Nothing in settlement is rebuilt; the lift session feeds the existing primitives.

### Same-chain (confidential PSET co-sign)

- Taker proposer side mirrors the seqdex proposer logic (Analysis A §2 "Proposer's Parallel Logic"; Analysis B `pkg/trade/buy.go:195-201` NewSwapTx, `pkg/trade/wallet.go:16-81`): build PSET v2 with taker inputs + output paying `want_asset` to `maker_recv_address`, collect `unblinded_inputs`, emit `SwapRequest` (`pkg/swap/request.go:89-132`).
- Maker responder side is exactly `CompleteSwap` (`daemon/internal/core/application/wallet/service.go:227-422`): parse incoming PSET (239), `SelectUtxos` for the asset it owes (265-287), derive change addr (289-292), build outputs (298-314), any-asset fee vout via `exchangeRateScale` (334-350, Principle 4 preserved), `UpdatePset` (400-405), `BlindPset` with taker's unblinded inputs (407-412), `SignPset` own inputs only (414-417) → `SwapAccept` (`pkg/swap/accept.go:61-103`).
- Taker finalizes: blind own inputs, `SignPset`, `ValidateAllSignatures` → `SwapComplete` (`pkg/swap/complete.go:72-131`), broadcast (`BroadcastTransaction`, Analysis A §3.8).
- Signatures: Elements legacy SIGHASH_ALL, DER low-S (Analysis B "Signature & Validation"). Anchor-bound automatically — it's just a Sequentia tx (Principle 1, no change).

The only seqdex code we DON'T carry over: the operator `Market`/CPMM/`Preview`/slippage/fee-account layer (Analysis A "MAKER-MODEL-SPECIFIC"). Price comes from the offer, not from reserves; there is no operator account.

### Cross-chain (HTLC, both directions)

Reuse `daemon/pkg/xchain/*` (Analysis B Part 2) end to end: `orchestrator.go` (Lock/Verify/Claim/Refund), `leg_elements.go`/`leg_bitcoin.go` (signing), `maker.go` (`VerifyBTCLeg`, `ExtractPreimage`, `InjectSecret`), `primitive.go` (HTLC script). The lift session couriers the leg notices (funding txids, scripts, pubkeys, `H`).

BTC→asset (MVP direction, already tested): taker is Alice (holds secret, funds BTC leg), maker is Bob (funds asset leg). Flow = `LockBTCLeg` → `LockSEQLeg` → `VerifySeqLegSafe` (anchor gate: anchorheight ≥ btcLegHeight, anchorstatus ok — Principle 1) → `ClaimSEQLeg` (reveals secret) → `ClaimBTCLeg`. The web wizard (Analysis D xswap.js 7-step) already drives this.

Asset→BTC (reverse direction — the maker reverse work): per Analysis B "MVP Direction vs. Reverse," the current CLTV design assumes BTC timeout > SEQ timeout and SEQ claims first. To support a maker selling BTC for an asset:
- The secret-holder/first-claimer must be the party whose leg is claimed second-to-last such that ordering + anchor safety still hold. Concretely, keep the invariant "the asset (anchored) leg is claimed first, revealing the secret; the BTC leg is claimed second," and assign the secret to whichever party receives the asset. So in asset→BTC, the taker receives BTC and the maker receives the asset; the maker (receiving the anchored asset) should hold the secret and claim the asset leg first, then the taker claims BTC. This swaps who-holds-secret vs the MVP.
- Locktime invariant stays "BTC refund (longer) > asset refund (shorter)"; the funder of the BTC leg uses the longer CLTV. Implement as a `Direction` enum on `CrossChainTerms` (§2) selecting which party generates the secret and the lock ordering; the orchestrator already has all five steps — we add a reversed-role driver that calls the same `LockBTCLeg/LockSEQLeg/Claim*` with roles swapped, plus a reversed `VerifySeqLegSafe` ordering check. This is net-new orchestration glue over existing primitives, NOT new settlement crypto. Flag for the owner: validate the anchor-ordering proof for the reversed case before shipping (Analysis B notes it "may need to run backward or use absolute timestamps" — recommend a focused test like `swap_integration_test.go` for the reverse path).

Confidentiality: same-chain settlement stays fully blinded; the book carries only off-chain terms; on-chain the fee vout is the only unblinded output (any-asset, native-equiv valued — Principle 4).

---

## 7. HOW OUR MAKER PLUGS IN

Our current market maker (the seqdex operator with CPMM markets and reserves) is refactored into a maker bot that posts offers into the same relay book as anyone else — no privilege.

- New component `cmd/seqob-mm/` (or refactor the existing MM service): on a loop, for each pair it wants to make, it computes bid/ask and posts signed `Offer`s via the relay `POST /v1/offers`, exactly like a wallet user. It cancels/edits as price moves.
- Pricing kept: reuse the existing pricing source (price-server-fed; CPMM-style spread around mid if desired) but it now expresses prices as discrete limit offers rather than a continuous `Preview` curve. The CPMM formula (`pkg/marketmaking/formula/balanced.go`) becomes an internal quoting helper that emits ladder offers; it is no longer consulted by takers.
- Co-signing: when a taker lifts one of the MM's offers, the MM runs the identical `CompleteSwap` responder path (§6) — same code the daemon already runs. The MM holds its own wallet; the relay never touches its funds.
- Cross-chain reserves: the MM keeps its testnet4 BTC reserve and posts cross-chain offers in both directions, implementing the reverse-direction role from §6.
- Removed: operator-only RPC (NewMarket, UpdateMarketPrice, WithdrawFeeFunds, slippage config — Analysis A "Scrap"). The MM is now just a well-capitalized peer.

Critical: the relay has zero special-casing for the MM. Its offers are validated, rate-limited, and ranked identically; takers can and should match other users' offers first if they're better priced (price-time priority).

---

## 8. WALLET UI CHANGES (from Analysis D)

Reuse the existing taker propose→sign→complete path (`swap.js`) and cross-chain wizard (`xswap.js`) unchanged for settlement; add browse/post/manage on top.

New panels (Analysis D Screens 1-5):
- `panel=dex-markets` (`dex-markets.js`, NEW): market list + live orderbook (snapshot+delta over WS from the relay). Bid/ask columns, spread, depth.
- `panel=dex-post` (`dex-post.js`, NEW; fork of `swap.js` composer): become a maker — pick offer/want assets+amounts, set rate (fixed, no daemon quote), fee-asset picker (any-asset, Principle 4), optional expiry. On submit: sign the `Offer` with the wallet key (`C.signer`) and `POST /v1/offers`. No PSET built at post time (Analysis C).
- `panel=dex-orders` (`dex-orders.js`, NEW): my open orders — status (OPEN/PARTIAL/FILLED/CANCELLED/EXPIRED/UTXO_INVALIDATED), edit, cancel (signed).
- `panel=swap` (existing composer): becomes the taker lift entry — pick a market → pick an order → enter amount (≤ available) → review → the existing `proposeSignComplete` runs, parameterized by `offer_id` instead of a daemon market quote (Analysis D Screen 3 "key delta").

Reused as-is: asset pickers, reference-currency hints (`C.refValueStr`), fee selectors, the `SignerRequest`-style net-effect review ("you send X / receive Y / fee Z" — borrowed from SideSwap, Analysis C §5), the cross-chain wizard.

Naming/copy (the Sequentia design principles, node repo `doc/sequentia/00-overview.md`): no "native asset"/"(native)" tags, SEQ is one row among equals, headline = total in reference currency; never call it "the SEQ chain"; fee-rate units are the chosen fee asset's own units/vByte, never "sat/vB."

---

## 9. REUSED vs BORROWED vs NEW

REUSED FROM SEQDEX (do not rebuild):
- Same-chain settlement: `pkg/swap/{request,accept,complete}.go`, `pkg/trade/{buy,sell,wallet}.go`, `wallet/service.go` `CompleteSwap` (227-422), `swap.proto`/`types.proto`.
- Cross-chain settlement: `pkg/xchain/{primitive,orchestrator,leg_elements,leg_bitcoin,maker,keys,errors}.go`, swap demo + integration test.
- Wallet ops: `SelectUtxos`, `UpdatePset`, `BlindPset`, `SignPset`, `BroadcastTransaction` (Ocean/LWK).
- Any-asset fee logic (`exchangeRateScale`, fee vout) — Principle 4 preserved.
- CPMM formula `pkg/marketmaking/formula/balanced.go` — demoted to an internal MM quoting helper only.

BORROWED FROM SIDESWAP (protocol shape, reimplemented in Go):
- `To`/`From` `oneof` envelope + message numbering; WS transport.
- Offer schema shape: `AssetPair`, `PublicOrder`/`OwnOrder` (with `active_amount`), `OrderId`, `TradeDir`; offer-as-price-promise (no UTXO/PSET at post).
- Book distribution: per-pair snapshot (`public_orders`) + deltas (`created`/`removed`), `marketSubscribe`/`unsubscribe`.
- Maker lifecycle: submit/edit/cancel with `ttl_seconds`, `private`, partial-fill `active_amount`.
- Stale-offer state machine: `HistStatus` → our `OfferStatus` (`UTXO_INVALIDATED`, `REPLACED`, `ELAPSED`, `CANCELLED`).
- Settlement-UX: net-effect `SignerRequest.Sign{balances, recipients, network_fee}` confirmation.
- (v2) two-step/offline order + `private_id` shared-by-link.

DROPPED from SideSwap (Principles): AMP/GAID gating, central dealer/RFQ-as-mandatory, server-set fee asset, L-BTC peg (no Sequentia analogue — we use Bitcoin anchoring; cross-chain is HTLC, designed separately per Analysis C).

NEW (build):
- `seqobd` relay (offer store, book pub-sub, lift session router, validator, liveness probe).
- `proto/seqob/v1/offer.proto` + `relay.proto`.
- Offer signing/verification, expiry, rate-limit, anti-grief reputation.
- v2 price-time matching engine.
- Reverse-direction (asset→BTC) cross-chain orchestration glue + anchor-ordering test.
- MM refactor `seqob-mm`.
- Web panels `dex-markets.js`, `dex-post.js`, `dex-orders.js`.

---

## 10. PHASED BUILD PLAN

### Phase 1 — minimal end-to-end, same-chain only (offer schema + relay skeleton + post/browse/lift + settle)

Scope: one same-chain pair (e.g. GOLD/USDX), online orders only, taker lifts a chosen offer, settles via existing PSET co-sign, anchor-bound. No cross-chain, no auto-match, no two-step, MM not yet refactored.

Files/services to create (all under repo `seqdex`):
- `proto/seqob/v1/offer.proto` — `Offer`, `AssetPair`, `SameChainTerms`, `TradeDir` (§2).
- `proto/seqob/v1/relay.proto` — `To`/`From` envelope, `StartLift`, `SwapMsg` wrapper (§3).
- `cmd/seqobd/main.go` — relay daemon: config (Sequentia node RPC read-only), WS + gRPC listeners.
- `internal/seqob/offerstore/store.go` — in-memory per-pair book (map[AssetPair][]Offer), snapshot+delta, sig verify, expiry sweeper.
- `internal/seqob/validator/validator.go` — offer sig check, schema/amount sanity, rate-limit, liveness probe (read-only node RPC).
- `internal/seqob/session/router.go` — lift session: bind taker+maker, courier `SwapMsg` (parse-validate via vendored `pkg/swap`), session deadline.
- `internal/seqob/api/ws.go` + `rest.go` — `POST /v1/offers`, `GET /v1/markets`, `GET /v1/market/{b}/{q}/orderbook`, `POST /v1/lift`, `POST /v1/offers/cancel`.
- Client-side maker+taker helpers reusing seqdex proposer/responder paths: `internal/seqob/client/maker.go` (wraps `CompleteSwap`), `client/taker.go` (wraps NewSwapTx + finalize). Used by a CLI `cmd/seqob-cli/` for the acceptance test.
- Web (repo `SWK`, branch `sequentia`): `dex-markets.js` (browse one pair), minimal `dex-post.js` (post signed offer), reuse `swap.js` for lift (parameterize by `offer_id`). Optional for Phase 1 if CLI proves the loop first.

Acceptance test (Phase 1):
1. Start `seqobd` against the testnet Sequentia node.
2. Maker CLI (wallet A, holds GOLD) posts a signed offer: sell 100 GOLD, want 45 USDX, no expiry.
3. `GET /v1/market/GOLD/USDX/orderbook` shows the offer; a second WS subscriber receives the snapshot then sees the offer as a `public_order_created` delta.
4. Taker CLI (wallet B, holds USDX) lifts the offer for 50 GOLD (partial). Relay opens a session; the seqdex 3-phase runs (taker `SwapRequest` → maker `CompleteSwap`→`SwapAccept` → taker `SwapComplete`); taker broadcasts.
5. Assert: a confidential Sequentia tx confirms; decode shows GOLD→B, USDX→A, change correct, exactly one unblinded any-asset fee vout; wallet B's GOLD balance +50, wallet A's USDX +22.5 (minus/plus fee).
6. Assert: the offer's `active_amount` drops to 50 GOLD; book delta emitted; `GET /v1/offers?maker_pubkey=A` shows status PARTIAL.
7. Negative: spend wallet A's GOLD elsewhere, then lift again → `SelectUtxos` fails → `SwapFail` → relay marks `UTXO_INVALIDATED`, removes offer, taker incurs no on-chain cost.
8. Confirm the settled tx is anchor-bound (`getblockheader` shows the anchor; no consensus change) — Principle 1.

### Phase 2 — cross-chain offers (both directions)
Add `CrossChainTerms`, courier `XchainMsg`, reuse `pkg/xchain` for BTC→asset; implement asset→BTC reversed-role orchestration + anchor-ordering test (§6). Wire the existing web `xswap.js` wizard to lift cross-chain offers. Acceptance: lift a BTC→asset and an asset→BTC offer end-to-end on testnet4 + Sequentia with the anchor gate enforced.

### Phase 3 — price-time auto-match (v2 matching)
Relay matching engine, price-time priority, engine-initiated lift sessions; partial-fill ladders.

### Phase 4 — MM refactor
Build `cmd/seqob-mm` posting offers into the book identically to users; remove operator-only RPC; keep price-server-fed pricing as a quoting helper. Verify the MM is not special-cased (a better-priced user offer is matched first).

### Phase 5 — polish + offline orders
Two-step/`private_id` offline orders (with exact-outpoint liveness + `REPLACED`/RBF handling), reputation/anti-grief tuning, full web UI (Screens 1-5), reference-currency + copy compliance (Principles 3 & naming), monitoring.

---

Open items for the owner to confirm before build: (1) relay in `seqdex` repo vs. new `seqob` repo (recommend `seqdex` to share `go.mod`/swap/xchain); (2) accept the best-effort liveness probe + atomic-co-sign-as-real-safety model for online orders (no bonds in v1); (3) sign off on validating the reversed-direction anchor-ordering proof (Analysis B's caveat) as a Phase 2 gate.

Relevant existing settlement files to reuse (paths in this repo): `daemon/pkg/swap/{request,accept,complete}.go`, `daemon/pkg/trade/{buy,wallet}.go`, `daemon/internal/core/application/wallet/service.go` (227-422), `daemon/pkg/xchain/{primitive,orchestrator,leg_elements,leg_bitcoin,maker}.go`, `daemon/api-spec/protobuf/seqdex/v1/{swap,types}.proto`.

---

# ADVERSARIAL REVIEW (red-team)

Confirmed: `BlindPset(ctx, pset, swapRequest.GetUnblindedInputs())` — the maker's `CompleteSwap` consumes the taker's `UnblindedInputs`, and `UnblindedInput` (request.go:13-18) carries `AssetBlinder` + `AmountBlinder`. The proposer (taker) reveals its input value/asset blinders to the responder, inside `SwapRequest`, which the relay courier parses. My top finding holds. Review below.

# Adversarial Review — Sequentia P2P Order-Book DEX

## BLOCKERS

### B1. The relay sees confidential amounts; the whole "non-custodial = private" claim is false as designed
The design states the relay "holds signed text" and "cannot front-run." But §3 also says the session router parses `SwapMsg` "validating only that they parse," and cites validating well-formedness via vendored `pkg/swap` as a *justification* for the Go/seqdex stack. Verified in code: `SwapRequest` carries `UnblindedInputs{AssetBlinder, AmountBlinder, Asset, Amount}` and `CompleteSwap` calls `BlindPset(ctx, pset, swapRequest.GetUnblindedInputs())`. So the proposer reveals the cleartext value and asset of every input it spends, plus the blinding factors. If the relay parses that message, **the relay operator learns the confidential amounts and can link spent outpoints to identities/offers** — a CT break shipped in Phase 1. The "cannot front-run" claim is also wrong: the relay sees lift intent, outpoints, amounts, and timing before the maker, which is exactly enough to front-run on another venue or via its own MM.
- **Why it bites:** confidentiality is the product's core promise; this hands it to the one centralized component, which is co-operated with the MM.
- **Fix:** end-to-end encrypt the inner swap payload to the maker's `maker_pubkey` (and taker's) so the relay couriers an opaque blob keyed only by session id. Accept that the relay then *cannot* validate PSET well-formedness — drop that as a design justification. Move E2E encryption into Phase 1, not "polish."

### B2. The blinding handshake is privacy-asymmetric and the maker can harvest taker privacy via abort
Because the responder (maker) blinds last using the taker's revealed input blinders, the protocol is asymmetric: **the maker preserves its input privacy; the taker sacrifices its input privacy to the maker.** A malicious maker can post attractive offers, accept lifts to collect `UnblindedInputs`, then `SwapFail`. Cost: zero. Yield: the cleartext values + asset blinders of the taker's coins, deanonymizing the taker's wallet. With permissionless makers and free pubkeys, this is a cheap mass-deanonymization harvester. Even with B1's E2E fix, the *maker* still learns it.
- **Why it bites:** retail takers are systematically de-privacied to sophisticated makers (incl. our own MM); failed lifts still leak.
- **Fix:** require the maker to commit/sign *before* the taker discloses full unblinded inputs (reverse who-reveals, or make disclosure contingent on the maker's signature being escrowed); rate-limit/penalize makers by accept-then-abort ratio specifically (not generic lift failures — see B3); document the residual maker-learns-input-value leak as a known limitation.

### B3. Anchoring reorg can un-settle a "FILLED" swap; the design treats 1-conf as final (Principle 1 relapse)
§6 says same-chain settlement is "anchor-bound automatically — no change," and the Phase 1 acceptance test declares `FILLED`/`PARTIAL` and decrements `active_amount` as soon as "tx confirms." Per Principle 1, a Sequentia tx whose anchor Bitcoin block is orphaned is *discarded* — the swap un-happens, the inputs become live again. There is **no reorg-undo path**: no re-opening of a `FILLED` offer, no rollback of `active_amount`, no anchor-depth gate before declaring a fill. The relay's book will diverge from chain state on every Bitcoin reorg.
- **Why it bites:** this is the exact "treat a Sequentia tx as final too early" trap. Filled orders silently reverse; partial-fill accounting corrupts; takers/makers see phantom settlements.
- **Fix:** the relay must not declare `FILLED`/decrement until the settling tx's anchor has N Bitcoin confirmations; add a reorg watcher that re-opens offers and restores `active_amount` when an anchor is orphaned. Add an anchor-depth assertion to the Phase 1 acceptance test (current step 8 only checks the anchor *exists*, not depth/reorg behavior).

### B4. Cross-chain `min_conf` default 1 ("anchor-shortened") is unsafe against Bitcoin reorgs
§2 `CrossChainTerms.min_conf` defaults to 1, "anchor-shortened." But the anchored (Sequentia) leg is *only as final as its anchor depth*. If the asset leg is claimed (secret revealed) at 1 anchor-conf and Bitcoin then reorgs, the asset-claim tx orphans while the secret is now public and the BTC leg is claimable by the counterparty — one leg reverses, the other settles. "Anchor-shortened to 1" directly contradicts the reorg-safety rationale (the point of real-time reorg following is that you *wait for sufficient anchor depth*, not that you can trust 1 conf).
- **Why it bites:** breaks cross-chain atomicity precisely in the reorg case the chain is designed to follow.
- **Fix:** `min_conf` (and HTLC timelock slack) must exceed plausible Bitcoin reorg depth; forbid `min_conf=1` for cross-chain; size timelocks so the claim window survives a reorg of that depth.

## MAJOR

### M1. Online-only orders aren't a resting order book — and capital is shared across offers (oversell)
`online_required=true` is the v1 default; two_step/offline is deferred to Phase 5. So v1 has **no resting passive liquidity** — both parties must be online simultaneously, which is RFQ-with-live-makers, not an order book. Worse, online offers reserve nothing, and the relay only serializes sessions *per offer*, not across a maker's offers. A maker with 100 GOLD can post ten 100-GOLD offers across pairs; the book shows 1000 GOLD of phantom depth; nine lifts fail at co-sign.
- **Why it bites:** displayed depth is illusory; takers waste session setup + UTXO locks; the "order book" framing oversells what Phase 1 delivers.
- **Fix:** account committed capital per maker across all their offers (relay-side soft accounting), or require online offers to be capped to a maker's probed free balance per asset; relabel v1 honestly as live-maker RFQ; consider pulling a minimal two_step into Phase 2 so the book has real resting liquidity.

### M2. Stale-quote / free-option problem collides with anti-grief reputation
A resting limit offer is a free American option for takers; on a relay with latency, makers get adversely selected on stale quotes. The maker's only defense is to refuse to co-sign a stale lift — but §3's anti-grief mechanism *demotes/temp-bans makers below a co-sign success ratio*. So the system **punishes makers for legitimately declining to be picked off.** Sophisticated makers will cancel aggressively (book churn) or widen spreads; honest makers get reputation-dinged.
- **Why it bites:** drives spreads wide / liquidity thin; the reputation metric is miscalibrated against a real economic behavior.
- **Fix:** distinguish "accepted then aborted after seeing taker data" (gameable, penalize) from "declined/expired at stale price" (legitimate); tie offers to short TTLs so stale quotes auto-expire rather than relying on co-sign refusal; consider taker-side fee/PoW on lift to price the option.

### M3. Relay is a censoring single point of failure with no federation
The relay is the sole matchmaker. It can: suppress a signed `OfferCancel` (phantom liquidity persists), selectively hide better-priced third-party offers to favor the co-operated MM (nothing *enforces* the "zero special-casing for MM" claim — same operator controls relay + MM code), reorder/delay couriered messages, or simply go down and halt all discovery. The plan has no multi-relay/gossip in any phase.
- **Why it bites:** centralization/trust and liveness SPOF undercut the "non-custodial/permissionless" positioning; cancel-suppression is a concrete book-integrity attack.
- **Fix:** make offers self-expiring via mandatory short `expires_at` so suppressed cancels die on their own; publish periodic signed book commitments clients can audit; design a multi-relay/gossip path (even if Phase 3+); let clients connect to multiple relays and cross-check signed offers.

### M4. Sybil maker spam defeats per-pubkey reputation and rate limits
`maker_pubkey` is free, so per-pubkey rate limits and reputation are evaded by generating fresh keys; IP limits fall to proxies. The "recent Bitcoin-anchored block height freshness token" is not a cost. No bond in v1. Reputation is per-pubkey and "client-side-overridable" → useless against fresh Sybils with no history. The liveness probe also lets an attacker force many read-only node RPC scans per offer → relay/node DoS.
- **Why it bites:** book-flooding and fake-depth Sybil are wide open from Phase 1; the stated mitigations don't bind.
- **Fix:** require a cost that binds to scarce resources — small PoW on `offer_id`, or a signature proving control of a live UTXO of the offered asset (note the privacy tension: this links offers to coins, so combine with B1's encryption and consider committing to a value range rather than an outpoint). Cap and cache liveness probes; never probe synchronously per submit.

### M5. Reverse-direction HTLC inherits the free-option, and the secret-holder's option is unpriced
§6's reverse path correctly flags the anchor-ordering proof as a gate (good). But beyond ordering: in asset→BTC the maker holds the secret and funds the long-timelock BTC leg, giving the maker a free option to complete or abort until near timeout while the taker's asset leg is locked. The design adds the role-swap glue but does not address the capital/timelock asymmetry economically.
- **Why it bites:** our MM (the maker funding BTC) carries unbounded option risk; counterparties' asset can be locked the full timelock for nothing.
- **Fix:** keep the flagged anchor-ordering test as a Phase 2 gate (agreed); additionally bound the option with shorter, tiered timelocks and/or a premium; document MM capital-at-risk and lock duration per reverse offer.

## MINOR

### m1. `price` as a `double` inside the signed payload is a canonicalization footgun
Fields 1..21 (including `price double` and `created_at`) are signed via "deterministic protobuf or sorted-key JSON." Floating-point (NaN, -0, platform rounding, JSON float formatting) makes deterministic serialization fragile across the Go relay and JS wallet, risking signature-verification mismatches. The authoritative ratio is already `want_amount/offer_amount` (integers).
- **Fix:** exclude `price` from the signed bytes (treat as display-only, derived), or serialize amounts only as integers; pin one canonical encoding and test cross-language round-trip.

### m2. `offer_id` uniqueness and cancel replay
`offer_id` is "maker-chosen, unique" but nothing namespaces it — two makers can collide; the relay must key offers by `(maker_pubkey, offer_id)`. `OfferCancel{offer_id, sig}` has no nonce/timestamp, so a relay (or MITM) could replay an old cancel against a re-posted same-id offer.
- **Fix:** namespace offers by pubkey; include `created_at`/nonce in the cancel's signed payload and require it to match the live offer.

### m3. Maker-can-double-spend-after-signing griefing window
After the maker returns `SwapAccept` (signed its inputs) but before the taker broadcasts, the maker can broadcast a conflicting tx spending the same inputs. Atomicity protects funds (the swap just won't confirm), but it wastes taker effort/locks and is a cheap grief.
- **Fix:** taker should broadcast immediately on receiving a complete signed tx (minimize the window); count maker-side conflicting-spend as an abort in reputation.

## PHASE-PLAN: under-scoped / out of order

- **E2E encryption (B1) is absent from Phase 1 and implicitly "polish."** It must be in Phase 1 — otherwise Phase 1 ships a CT break. This is the single biggest ordering error.
- **Anchor-depth finality + reorg-undo (B3) is missing from Phase 1.** Acceptance step 8 checks the anchor exists but not depth/reorg re-open. Phase 1 cannot claim `FILLED` correctly without it.
- **Privacy-harvest-via-abort (B2) and Sybil/oversell (M1, M4) exist from Phase 1, but anti-grief/reputation is deferred to Phase 5.** At minimum, accept-then-abort penalties and per-maker capital accounting need to land with the first lift path.
- **The liveness probe is load-bearing for "book cleanliness" in Phase 1 but cannot work for confidential balances** (the doc admits "imperfect"). Be honest that v1's book = unvalidated intents; don't let the probe imply validated depth.
- **The session router parsing PSETs (Phase 1) directly conflicts with B1's E2E encryption.** This is an architectural fork that must be decided *before* Phase 1, not discovered during it.
- **Relay SPOF/federation (M3) is in no phase.** At least mandatory offer self-expiry (so cancel-suppression self-heals) belongs in Phase 1; multi-relay can be later but should be on the roadmap.
- **Two_step/offline orders deferred to Phase 5** means Phases 1–4 have no genuine resting liquidity (M1). Consider promoting a minimal offline-order mode earlier, since "order book" is the headline claim.
- **Open item (1)** (relay in `seqdex` vs new repo): recommending `seqdex` to "share swap/xchain packages" is partly motivated by relay-side PSET validation, which B1 removes — re-evaluate the repo decision once the relay stops parsing inner payloads.

Settlement-reuse stance (don't rebuild `pkg/swap`/`pkg/xchain`) is sound; the risks above are in the *new* order/relay/blinding-transport layer and in the anchoring-finality assumptions, not in the proven primitives.