# SeqLN Phase 2 → SeqDEX/wallet integration plan

Phase 2 delivered the submarine-swap PRIMITIVES (both directions, proven live; see
`seqln-phase2-submarine-swaps.md`). This note maps them onto the EXISTING SeqDEX order-book
architecture so a wallet user can trade a Sequentia asset ↔ BTC-over-Lightning. It is the §5b-style
seam map for the integration layer, grounded in the live code (`~/seqdex/daemon`, branch `main`).

## 1. What already exists (reuse, do not rebuild)

- **Order-book offer schema** (`api-spec/protobuf/seqob/v1/offer.proto`): the settlement `oneof` already
  reserves `LightningTerms lightning = 22`, and `LightningTerms` is drafted with the fields the primitives
  need: `ln_direction` (0 = ASSET_ONCHAIN_FOR_BTC_LN, 1 = BTC_LN_FOR_ASSET_ONCHAIN), `maker_claim_pub`,
  `maker_refund_pub`, `onchain_cltv`, `maker_issues_hold_invoice` (selects the two REVERSE modes), and
  `max_0conf_amount`. The Offer also carries `min_anchor_depth` (=16), `maker_ln_node_pubkey` (=17), and
  `ln_connect_hints` (=18). So the schema is additive-ready — likely NO proto change needed for v1.
- **Cross-chain driver + courier pattern** (`internal/seqob/client/xdriver.go`, `xdriver_reverse.go`,
  `xcourier.go`): the maker runs an opaque relay-couriered lift handshake and settles via the `XcOps` seam
  bound to `*xchain.Swap` (`LiveXcOps`). Everything from the peer is untrusted (re-derived scripts,
  byte-compared, checked against the SIGNED offer). This is the exact shape the submarine flow mirrors,
  minus the on-chain BTC leg.
- **Maker application service** (`internal/core/application/xchainmaker/{maker.go,handler.go,
  handler_reverse.go}`) + gRPC (`api-spec/.../seqdex/v1/xchain.proto`): constructs the swap
  (`NewSwap`/`NewSwapBitcoin`) and drives Verify/Lock/Claim. The Lightning path adds a sibling that
  constructs `NewSubmarineSwap` instead.
- **The proven submarine primitives** (`pkg/xchain/{leg_lightning.go,submarine.go}`): `SubmarineSwap`
  (`RunNormal`, `OfferReverseMakerSecret`/`AwaitReversePayment`, `RunReverse`), `clnLNLeg`, and the
  anchor-depth gate `VerifySeqAnchorBuried`.

## 2. Net-new for the integration (build order)

1. **Config: the maker's LN node.** Add the maker's `lightning-rpc` socket path (and derived
   `maker_ln_node_pubkey` via `clnLNLeg.NodeID()`) to the daemon config, alongside the existing SEQ/BTC
   chain config. This is the only new external dependency (a SeqLN-on-Bitcoin node; already running for the
   tests). The BTC *chain* backend is NOT needed for the LN leg.
2. **Submarine seam.** A thin `SubmarineOps`-style binding (mirroring `LiveXcOps`) over `*SubmarineSwap`, OR
   call `SubmarineSwap` directly from the handler — it is already a high-level orchestrator (unlike
   `*xchain.Swap`, which needed the `XcOps` wrapper). Decide by whether the courier handshake wants a
   fake-able seam for tests (the on-chain driver did).
3. **Submarine courier handshake** in a new `xdriver_submarine.go`: negotiate H, the maker's LN endpoint,
   the asset-HTLC terms, and the direction over the SAME opaque relay courier (`xcourier.go`). NORMAL: taker
   funds the asset HTLC + hands the maker a BOLT11; maker gates + pays + claims. REVERSE (maker-secret):
   maker locks the asset + returns the invoice; taker gates + pays + claims. This is the bulk of the work;
   it reuses the courier/crypter, not settlement.
4. **Handler dispatch.** In the maker service, branch on `offer.GetLightning()` to the submarine driver,
   as the existing code branches on `GetSameChain()`/`GetCrossChain()`.
5. **Taker/wallet flow.** The taker side (in the wallet) must, for the maker-secret REVERSE, run the
   anchor-depth gate (`VerifySeqAnchorBuried >= min_anchor_depth`) BEFORE paying, and surface it honestly
   (never "final" at 0-conf; §DEX-0conf policy). For NORMAL the wallet funds the asset HTLC and mints the
   BOLT11 with a chosen preimage.
6. **UX (later):** the wallet Swap screen shows Lightning offers and lets a user take one; per the SeqOB UX
   directive, never error "no price" — show the book and let the user post their own LN offer as maker.

## 3. Open design forks (need a steer before step 3)

- **Which REVERSE mode does the DEX default to?** The shipped, plugin-free **maker-secret / taker-gated**
  mode works against a stock node TODAY and fits a wallet/DEX that controls its own taker client (it runs
  the gate). The **hold-invoice / maker-gated** mode (`RunReverse`) is strictly more maker-protective for a
  public maker serving arbitrary takers, but needs the hold-invoice plugin on the node (task #20). The
  offer schema already carries `maker_issues_hold_invoice` to advertise which a given maker supports, so
  both can coexist — but v1 should implement one end-to-end first.
- **Relay-courier session shape for submarine swaps.** Reuse the existing `SwapMsg` opaque frames
  (`xcourier.go`) verbatim, or add a submarine-specific session type? The legs differ (one side is an LN
  invoice, not an on-chain HTLC), so the courier PAYLOADS differ even if the transport is identical.
- **0-conf caps + fee policy** for the LN leg (`max_0conf_amount`, `min_anchor_depth` defaults per pair).

## 4. Smallest shippable first slice

Implement the **NORMAL** direction end-to-end through the daemon (taker sells an asset for BTC-LN): it needs
no plugin, no new proto, and reuses the courier. That makes "sell a Sequentia asset, receive BTC on
Lightning" work from the wallet against the live maker, and is the lowest-risk way to prove the integration
before adding the REVERSE take-flow and the hold-invoice option.

## 5. Progress

- **DONE — the NORMAL submarine lift DRIVER** (`internal/seqob/client/xdriver_submarine.go`,
  `xcourier_submarine.go`; merged to `main`). `RunMakerSubmarineNormal` / `RunTakerSubmarineNormal` over the
  opaque relay courier, the `SubMakerOps`/`SubTakerOps` seams (+ `LiveSub*Ops` over `*xchain.SubmarineSwap`),
  the submarine courier messages (BTC leg as a BOLT11 in `XcMsg.Bolt11`), and `pkg/xchain` taker helpers
  (`MintInvoice`/`AwaitInvoicePaid`). Node-free handshake test (`xdriver_submarine_test.go`): the maker
  recovers `P`, the taker gets paid, and a maker quoting the wrong `seq_amount` is refused. This is the
  protocol logic — the hard part — and it is at parity with the on-chain cross-chain driver (both are
  driver+test; the cmd wiring below is the last mile for BOTH).
- **DONE — cmd wiring, PROVEN LIVE END TO END through the binaries** (merged to `main`):
  1. `cmd/seqob-maker`: `-mode lightning` (+ `-ln-socket`, `-sub-anchor-depth`, `-onchain-cltv`);
     `runSubmarineMaker` posts a NORMAL Lightning offer and `serveSubmarine` dispatches
     `RunMakerSubmarineNormal` (no bitcoind — only the Sequentia node + the maker's `lightning-rpc`). The
     maker BUYS the asset for BTC-LN (`trade_dir=BUY`, `ln_direction=LnAssetForBTC`).
  2. Offer posting: `buildSubmarineOffer` (base=asset, quote=BTC sentinel, `maker_ln_node_pubkey` from
     `NodeID()`).
  3. `internal/seqob/validator`: `checkLightning` accepts `GetLightning()` offers; `offer.LnDirectionConsistent`
     (the maker's trade side is the INVERSE of the flow name, unlike cross-chain).
  4. `cmd/seqob-cli xsublift`: the taker sells the asset for BTC-LN via `RunTakerSubmarineNormal`, persisting
     the asset refund material before funding.
  5. **Live e2e PASSED** (2026-07-03, isolated regtest + relay + the two SeqLN-on-Bitcoin nodes): maker posts
     the offer → relay validates + rests it → taker funds the asset HTLC + mints a BOLT11 on `P` → maker gates
     on anchor depth (buried by a parent-chain block generator), pays the invoice (taker receives 1,000,000
     msat), learns `P`, and claims the asset on-chain. Both sides logged SETTLED. The run caught two real bugs
     (now fixed): the maker's `AnchorTimeout` defaulted to 0 (gate failed instead of waiting), and the flat
     asset-claim fee could exceed a small leg (`bad-txns-vout-negative`) — clamped with `xcSafeFee`. The
     Sequentia node also needs a fee exchange rate for the asset (`setfeeexchangerates`) for the taker to fund.
- **DONE — REVERSE (maker-secret), PROVEN LIVE END TO END** (merged to `main`): the mirror of NORMAL.
  `RunMakerReverseSubmarine` / `RunTakerReverseSubmarine` + `SubReverse{Maker,Taker}Ops`; `cmd/seqob-maker
  -mode lightning -side sell` posts a SELL/`LnBTCForAsset` offer and the serve loop branches on `ln_direction`;
  `cmd/seqob-cli xsubbuy` is the taker (buys the asset with BTC-LN). Node-free handshake test green.
  **Live e2e PASSED** (2026-07-03, same rig): maker posts the SELL offer → relay rests it → maker locks the
  asset HTLC (claim=taker) + issues a plain invoice on H → taker VERIFIES the HTLC and runs the anchor-depth
  gate BEFORE paying (logged `asset HTLC anchor-buried (depth=5); paying`), pays the invoice (maker receives
  1,000,000 msat), learns P, and claims the asset on-chain. Both sides logged SETTLED. Here the anchor gate is
  TAKER-side (pre-pay) — a plain invoice is irreversible once paid — vs maker-side in NORMAL. So BOTH
  directions of the plugin-free submarine swap now run through the binaries.
- **THEN — the wallet** take-flow (Phoenix-like UX) consumes the taker drivers (`RunTakerSubmarineNormal` /
  `RunTakerReverseSubmarine`); the hold-invoice REVERSE (fully-maker-non-custodial) and asset-aware channels
  (pure-LN) remain.
