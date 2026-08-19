# Consolidating cross-chain onto the order book (retiring the RFQ special-maker)

## The question

"The cross-chain market shouldn't drive BTC in the swap tab, since we've moved on
from a pure RFQ model to an order book. Did we mistakenly create a hybrid? If so we
need to consolidate to the order book, where no maker is special."

## Diagnosis: yes, it is a hybrid

The wallet's Swap tab draws liquidity from two different systems:

- **Same-chain swaps** (asset for asset) go through **seqob**, the order-book relay
  (`:9955`). Offers rest, signed, in a public book; anyone can post one; our market
  maker is just one participant. This is the order-book model.
- **Cross-chain swaps** (BTC for asset) go through **seqdexd**, the TDEX-fork daemon
  (`:9945`), over `/v1/xchain/{markets,quote,propose,swap}` and the reverse
  `/v1/xchain/reverse/{quote,open,submit}`. This is the RFQ model with a **single
  privileged maker** (the `xchainmaker`, funded from `seqdex-mm-btc` on dexnode):
  only that maker quotes, and only it can make a BTC market.

So the composer gets same-chain liquidity from a permissionless book and BTC
liquidity from one special maker. That is the hybrid.

## Why it should change

1. **It contradicts the "no special maker" principle.** Nobody but our maker can make
   a BTC market. In the order book, anyone can.
2. **It is fragile.** The Swap tab's BTC availability is coupled to seqdexd being up
   and the maker's wallets being loaded. When dexnode dropped its wallets on a
   restart, `/v1/xchain/markets` errored and BTC vanished from the tab. An order-book
   offer is durable state that does not depend on a daemon's liveness the same way.
3. **It is two protocols to maintain** (seqob plus the seqdexd RFQ surface).
4. **seqdexd holds swap state in memory.** A restart strands in-flight cross-chain
   swaps (the stranded-swap we hit). The order book has no central per-swap state to
   lose: offers are signed and resting, settlement is per-party.

## The groundwork already exists

This is wiring, not a redesign:

- **The offer schema already models cross-chain.** `seqob/v1/offer.proto` Settlement
  oneof has `CrossChainTerms cross_chain = 21` beside `SameChainTerms same_chain = 20`,
  carrying `btc_sentinel`, `maker_claim_pub`, `maker_refund_pub`, `maker_leg_locktime`
  (CLTV), `maker_recv_address`, and `direction` (BTC_TO_ASSET / ASSET_TO_BTC). The
  shared `min_anchor_depth` (field 16) is documented as covering cross-chain settlement.
- **The relay is settlement-agnostic.** seqobd couriers opaque end-to-end messages and
  rests signed offers; it does not interpret the settlement variant, so it already
  carries cross-chain offers without change.
- **The HTLC settlement is already built and proven.** `pkg/xchain` (orchestrator,
  `leg_bitcoin`, `leg_elements`, anchor gate, claim/refund) is what seqdexd uses today,
  and the wallet already has the BTC-leg and SEQ-leg bridges plus the forward
  (`xswap.js`) and reverse (`xrswap.js`) HTLC wizards. Live swaps have settled with it.

## Target architecture

BTC is just another asset in the one book.

- A maker posts a `CrossChainTerms` offer to seqob (BTC->asset: offer asset, want BTC;
  asset->BTC: offer BTC, want asset), including its claim/refund pubkeys and CLTV. Our
  maker does this like anyone else; it is no longer privileged.
- A taker discovers BTC<->asset offers in the same book (`/v1/markets`,
  `/v1/market/{base}/{quote}/orderbook`) and lifts one.
- The lift runs the existing HTLC settlement (the end-to-end-couriered atomic swap,
  reusing `pkg/xchain` and the wallet's HTLC bridges), gated on the offer's
  `min_anchor_depth`, with claim/refund as today. No RFQ quote/propose round-trip.
- The Swap tab discovers BTC<->asset markets from seqob, not seqdexd. Settlement state
  lives where it belongs: the taker keeps its swap in `localStorage` (already does), the
  maker re-derives from the offer and the on-chain legs. There is no central swap map to
  lose, which also makes the separate seqdexd-persistence task unnecessary.

## Implementation plan (phased, non-breaking until the last step)

1. **Maker posts cross-chain offers (additive).** Teach `seqob-maker` to build and post
   `CrossChainTerms` offers (start with BTC<->GOLD) alongside its same-chain offers. The
   relay already carries them. The seqdexd RFQ stays up untouched, so nothing breaks.
2. **Taker lift for cross-chain.** Implement the cross-chain lift in `seqob-cli` and the
   wallet, reusing `pkg/xchain` settlement and the wallet's existing HTLC wizards but
   driven by a seqob offer instead of an RFQ quote. Test both directions on testnet.
3. **Rewire the Swap tab.** Point cross-chain discovery at seqob; drop the
   `/v1/xchain/markets` dependency so BTC availability no longer hinges on seqdexd.
4. **Retire the RFQ cross-chain.** Once the order-book path is proven both directions,
   remove the seqdexd `xchainmaker` and `/v1/xchain/*`, or leave it dormant.

## Risks and rollout

- Keep the RFQ path live as a fallback through phases 1 to 3; only remove it in phase 4
  after the order-book path is proven both directions.
- The settlement primitives are already proven by live swaps, so the new risk is in the
  offer plumbing and the wallet rewire, not in the atomic-swap cryptography.
- Anchoring still governs finality identically (Principle 1): the offer's
  `min_anchor_depth` is the per-maker dial, default 0, surfaced honestly.

## Recommendation

Do this with interactive verification, not as an unattended deploy, because phases 2 to
4 change the live cross-chain path the wallet relies on. The schema and the settlement
code are ready, so the effort is offer plumbing plus the wallet UX, not a new protocol.
This supersedes the separate "seqdexd cross-chain swap-state persistence" task: the
order book removes the central in-memory swap state that task was meant to protect.
