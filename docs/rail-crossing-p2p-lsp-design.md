# Rail-crossing settlement: P2P-first, LSP-fallback, both directions

Design spec for completing the SeqDEX rail-crossing matrix. Principle: matching is
rail-blind (price/asset/size); settlement picks a mutually-supported rail per the
counterparties' capabilities — **direct peer-to-peer when they line up, the LSP
leg-bridge ONLY on a genuine mismatch** (offline / on-chain-only / passive covenant
maker). This applies symmetrically to both bridge directions.

## The two BTC-leg representations

A BTC<->asset swap has two legs bound by one preimage H. The asset leg is Sequentia
on-chain; the mismatch is always on the BTC leg.

| | BTC leg | on-chain HTLCs | coupled locktimes |
|---|---|---|---|
| **P2P submarine** | a bolt11 on H (pure Lightning) | 1 (asset only) | none — single T_seq gate |
| **LSP leg-bridge** | LSP terminates LN, originates an on-chain BTC HTLC | 2 (BTC + asset) | full W1/W2 coupling |

The LSP's second on-chain HTLC is exactly what forces the intricate coupled locktimes.
When the maker can accept BTC-LN, that HTLC never exists and the coupling disappears —
this is why **P2P is the safer default**, not a compromise.

## PAYER shape — Path A (FIRST-CLASS): direct P2P reverse submarine

Trade: taker BUYS asset, PAYS BTC over Lightning, RECEIVES asset on-chain. Maker rests
`LightningTerms{ln_direction=LnBTCForAsset=1}` advertising `maker_ln_node_pubkey`.
Drivers ALREADY EXIST: `RunMakerReverseSubmarine`/`RunTakerReverseSubmarine`
(seqdex `xdriver_submarine_reverse.go`); courier `XcSubTermsRequest`/`XcSubAssetLocked`/
`XcSubSettled` (`xcourier_submarine.go`).

Secret holder = MAKER (generates P, H=sha256(P)). Safe BECAUSE the BTC leg is a plain
invoice — Lightning forces the maker to reveal P to capture the payment.

1. Maker locks ONE on-chain HTLC (the asset): claim=taker-with-P, refund=maker after
   T_seq; mints a PLAIN bolt11 on H.
2. Maker -> `XcSubAssetLocked{hash_h, maker_refund_pub, seq_locktime, leg, bolt11}`.
3. Taker binds the leg to the signed offer: `VerifySEQLeg` proves the output is
   claim=taker's own SeqClaimKey on H, correct asset/amount/locktime.
4. Taker runs the anchor-depth gate `VerifySeqAnchorBuried >= min_anchor_depth` — never
   pays against a reorg-able asset HTLC.
5. Taker `PayInvoice(bolt11)` -> learns P. `OnPaid` persists P+leg before the claim.
6. Taker `ClaimSEQLeg(P)` before T_seq -> has the asset. Maker already has the BTC-LN.

Fund-safety: paying the maker's invoice is the ONLY way to obtain P, and paying it IS the
maker capturing BTC-LN. So the maker cannot capture BTC-LN without handing the taker the
key to a pre-locked, pre-verified, anchor-buried asset HTLC. Taker can't lose BTC-LN
without the asset (it verifies 3+4 before its single irreversible act); maker can't get
BTC-LN without delivering (getting paid = revealing P = taker claims before T_seq).

Timelocks (single chain, the ENTIRE coupling):
- (P1) anchorDepth(assetHTLC) >= min_anchor_depth   [before pay]
- (P2) seqTip + claimMargin < T_seq                  [before pay, re-checked]
- (P3) pay -> learn P -> claim, all strictly before T_seq
- (P4) HOLD-INVOICE min-final-CLTV vs T_seq gate     [before pay] — see below.

CORRECTION (P4, the taker's hold gate). A bolt11 HOLD invoice is BYTE-IDENTICAL to a plain one, so the taker
CANNOT assume the maker minted a cooperative plain invoice. A malicious interactive maker can mint a HOLD
invoice whose `min_final_cltv` lets it keep the taker's payment HELD (settleable) PAST T_seq — then refund the
asset at T_seq and settle the hold, capturing BTC-LN with NO asset for the taker. So the taker MUST gate on the
invoice's min-final-CLTV: decode the `c` tagged field (default 18 if absent), take the LATEST the maker could
still settle (reveal P) = a BTC height ~ `btcTip + min_final_cltv`, convert that BTC-block window to the
Sequentia timeframe with the CONSERVATIVE **INVERSE** ratio (`SUB_REVERSE_CLTV_RATIO` = 60), and REQUIRE it
leaves a claim margin before T_seq. The gate is evaluated at the POST-anchor-bury tip (`seqTip2`, see the
coupling below): `settleDeadlineSeq = seqTip2 + ceil(fc * 60)`, gate passes iff
`settleDeadlineSeq + claimMargin < T_seq`. NOTE the conservative direction FLIPS versus the forward leg-bridge
sizing: the bridge's `HOLD_LIFE_DEFAULTS` (fastBtc 150 / slowSeq 90 ≈ 1.67) is right for SIZING a hold to
COVER T_seq (assume BTC fast + SEQ slow); here we must UPPER-BOUND how many SEQ slots elapse before a
masqueraded hold's `fc`-BTC-block HTLC finally expires. The SEQ slot is DETERMINISTIC (30 s, g_pos_slot_interval),
so ALL the margin sits on the VARIABLE Bitcoin side: BTC nominal ~600 s, but a sustained hashrate-drop lull can
average ~1500-1800 s/block over a short window, so we assume BTC as SLOW as ~1800 s and SEQ at its exact 30 s
slot => 1 BTC block maps to 1800/30 = 60 SEQ slots. (This GENEROUS ratio covers a SUSTAINED lull, not just a
modest ~900 s excursion which would need only ~30x.) Using the forward 1.67 here made the gate ~36x too
permissive (a hold could stay settleable past T_seq undetected). The taker ALSO caps its OWN outgoing payment's
max-cltv-delay to `fc` so a HELD payment fails back (refunds the taker) as early as possible. Fail closed (XcFail
`BAD_HOLD_CLTV`, never pay) otherwise. (subswap.js `bolt11MinFinalCltv` + `holdCltvSafeVsTseq`, in
`runTakerReverseSubmarine` before the pay.)

HONEST-MAKER COUPLING (seqdex, so this corrected gate PASSES a legitimate offer). The reverse-submarine maker
sizes its asset-leg T_seq (`SeqLocktimeDelta`) and the minted bolt11's `min_final_cltv` from ONE invariant, so
they can never drift apart — and because the taker's gate runs only AFTER it has buried the fresh asset HTLC to
`min_anchor_depth` BTC confs (during which the SEQ tip advances ~`min_anchor_depth*ratio` blocks), T_seq must
ALSO absorb that bury advance: `SeqLocktimeDelta >= (fc + minAnchorDepth)*ratio + claimMargin + buffer`. The
maker mints a BOUNDED-SMALL `fc` (viable over the short LSP-hub routes these swaps take) and derives T_seq from
it. Chosen pair: **fc = 8, ratio = 60, minAnchorDepth = 3, claimMargin = 120, buffer = 40 => T_seq = (8+3)*60 +
120 + 40 = 820 SEQ blocks (~6.8 h)** (`SubReverseInvoiceCLTV` / `SubReverseConservativeRatio` /
`SubReverseMinAnchorDepth` / `SubReverseClaimMargin` / `SubReverseTseqBuffer` / `SubReverseSeqLocktimeDelta` in
seqdex `xdriver_submarine_reverse.go`; `coupleSubReverse` raises a short delta — e.g. the cross-sized 240 default
— to the coupled minimum). Arithmetic: at `seqTip2 = M + 3*60 = M+180`, `settleDeadlineSeq = M+180 + ceil(8*60)
= M+660`, and `M+660 + 120 = M+780 < M+820 = T_seq` — clears with 40 blocks (= buffer) of slack. This is the
single-chain submarine gate, UNRELATED to the cross W1/W2 `-seq-locktime-delta`. (Keeping `buffer` < `ratio`
makes the honest `fc` exactly the largest CLTV the T_seq admits; the buffer is then pure tip-advance slack.)

RESIDUAL RISK (P4, known + mitigated — not a logic bug, mirror of Path B). This is a FIXED SEQ window versus a
VARIABLE, unbounded Bitcoin block time. The ratio is sized for a SUSTAINED ~1800 s/block lull; if the REAL
Bitcoin average over the `fc`-block window EXCEEDS `ratio*30 s`, a hold-masquerade maker could reveal P PAST the
claim window and capture the taker's BTC-LN with no asset. It is BOUNDED, never a permanent freeze: the taker
caps its OWN outgoing max-cltv-delay at `fc`, so a HELD payment REFUNDS if the maker never settles — the taker's
BTC is never left merely HELD unrecoverable, and the loss materialises ONLY if a maker actively settles late
(borne then by the taker/LSP, never the maker). We MITIGATE, not eliminate: (a) the generous ratio (60, a 3x
sustained slowdown); (b) the bounded maker `fc`; (c) the taker's own max-cltv cap failing a held payment back as
early as `fc` allows. Chasing it to ZERO would need an UNBOUNDED window (itself a liveness / capital-lock
failure), so it is sized generously and documented — the SAME known limitation every Lightning CLTV delta lives
with (an LN forwarding node can likewise lose an HTLC it cannot timeout-claim before the incoming CLTV under
sustained congestion). This gate is NOT an unconditional guarantee.

No T_btc and no second on-chain HTLC — the BTC leg never touches chain. But there IS a min-final-CLTV-vs-T_seq
inequality on the TAKER side (P4): the earlier "no min-final-CLTV inequality" claim held ONLY for a cooperative
plain invoice; because a hold invoice is indistinguishable from a plain one, the taker must gate on it.

## PAYER shape — Path B (FALLBACK): LSP payer-direction leg-bridge

Used when the matched maker is on-chain-only (FORWARD `CrossChainTerms`) or a passive
`CovenantTerms` fill — cannot accept BTC-LN. LSP terminates the taker's BTC-LN and
originates an on-chain BTC HTLC to the maker. Mirror of the built receiver bridge; reuses
`stepPayerLn` (leg-bridge.mjs:595-620, already unit-tested but never admitted).

Secret holder = TAKER (mints H, holds P). If the LSP minted H it could settle the taker's
hold without giving P to the taker -> taker loss. With the taker holding P, P becomes
public only when the taker claims the asset.

Structure (two on-chain HTLCs + one LN hold, all on the taker's H):
1. Taker mints H (holds P), hands H + its asset-claim pubkey to the LSP.
2. LSP ISSUES a BTC-LN HOLD invoice on H (inverse of /bridge/front). Taker pays -> HELD,
   not captured. Primitive: `lnrpc('holdinvoice', [H, amtMsat, ...])`; state via
   `holdinvoicelookup`; capture via `holdinvoicesettle`.
3. Only after the hold is HELD (stepPayerLn gate), LSP funds the on-chain BTC HTLC to the
   maker via the FORWARD handshake (claim=maker_btc_claim_pub-with-P, refund=LSP after
   T_btc) — `fundOnchain` (lsp-server.mjs:1916).
4. Maker funds the asset HTLC to the taker's claim pubkey on H, refund=maker after T_seq;
   relays `XcSeqLegLocked` which the LSP passes to the taker.
5. Taker verifies + claims the asset with P self-custody -> has the asset.
6. Maker reads P from the asset claim, claims the LSP's BTC HTLC -> maker paid.
7. LSP reads P (from the Sequentia asset-claim witness, primary; or its own BTC HTLC
   spend, backstop) and settles the held LN (`recoupSettle`) -> recoups exactly its front.

Timelocks (coupled, mirror of the receiver W1/W2):
- (B1) now < T_seq < T_btc  (maker needs runway to claim BTC after P reveals at ~T_seq;
  already enforced by the FORWARD driver, xdriver.go:613-618)
- (B2) hold settleable >= T_seq_wall + reorgMargin + settleMargin  (== `requiredTakerHold`,
  leg-bridge.mjs:471-490; sizes hold_expiry AND the incoming-HTLC min-final-CLTV from T_seq)
- (B3) T_btc matures inside the hold's remaining life (stepPayerLn holdBuffer) so a
  no-reveal ends double-no-loss (BTC refunds to LSP, hold expires to taker)

The (B3) hold buffer is SIZED, not guessed: `holdBuffer = refundFinalityConfs + refundConfirmBudget`
(6 + 12 = 18 BTC blocks), DERIVED in leg-bridge.mjs so the confirm budget and the finality depth
can never silently drift out of the window. It is the worst-case wall-clock window a no-reveal
refund gets: an adversarial maker pins T_btc to exactly `holdCLTV - holdBuffer` (the B3 ceiling), so
the slack must span the ENTIRE refund lifecycle — broadcast at T_btc, CONFIRM under RBF
(refundConfirmBudget = 12 blocks), then BURY to finality (refundFinalityConfs = 6). The honest fleet
rests T_btc well below that ceiling (seqdex `BtcLocktimeDelta` 180 vs a ~210-block hold, ~30 blocks
below), clearing both B1 (floor ~150, from the T_seq wall-clock + maker-claim runway) and B3
(ceiling ~192) with headroom. Coupled bump: `BtcLocktimeDelta` was raised 100 -> 180 so the honest
maker's T_btc clears the conservative B1 floor (100 was a wall-clock inversion under fast BTC) and
stays under the B3 ceiling at holdBuffer = 18.

RESIDUAL RISK (known, mitigated — not a logic bug). A FIXED hold window versus real on-chain finality
carries an irreducible residual: under SUSTAINED, multi-hour congestion where even a top-of-mempool,
RBF-bumped refund cannot CONFIRM + BURY inside holdBuffer, the LSP's refund stalls past the hold, the
hold fails back to the taker, and a maker claim can then take the LSP's BTC HTLC — a front loss borne
by the LSP, NEVER by the taker (whose BTC was only ever HELD) or the maker. We MITIGATE, not eliminate:
(a) a generous, reasoned window (refundConfirmBudget is ~4x the ~3-block RBF target, so only 12+ blocks
of top-fee starvation bites); (b) RBF escalation from the first post-refund tick (`refundBumpWithin`,
`sizeRefundFee`, the `refund-bump` action); (c) the LSP's concurrent-exposure caps bound the worst
case. This is the SAME known limitation every HTLC / Lightning system lives with — a fixed CLTV delta
vs. confirmation latency (an LN forwarding node can likewise lose an HTLC if it cannot confirm a
timeout-claim before the incoming CLTV under sustained congestion). Chasing it to ZERO would require an
UNBOUNDED window, itself a liveness / capital-lock failure. So it is sized generously and documented —
not treated as a bug to be closed.

P-source direction differs from the receiver bridge: there the LSP learns P from its own
LN settle (waitsendpay); here the LSP settles TOWARD the taker, so it must read P from a
chain (Sequentia asset claim primary, its BTC HTLC spend backstop). Front-time re-verify
(mirror of verifyFrontRouteExpiry): verify the ACTUAL committed incoming-HTLC CLTV covers
T_seq, never the invoice's requested min_final_cltv. Fail closed if unverifiable.

## Maker handshakes
- P2P payer (A): the bridge-maker.mjs:12-19 "can't fix both pubkey and H" constraint is a
  property of the maker-funded on-chain BTC HTLC (`CrossChainTerms`) ONLY. On the submarine
  path the maker funds NO BTC HTLC (bolt11), so it dissolves. P2P is fund-safe with any
  online maker; drivers already exist. Re-scope the code note to "why the LSP is the
  fallback for an on-chain-only maker," not "why the LSP is required."
- LSP payer (B): needs a NEW `runForwardBridgeTerms`/`openForwardBridgeSession` (mirror of
  `runReverseBridgeTerms`) — LSP funds the BTC HTLC + relays the maker's asset leg. Message
  types exist (`XcTerms`/`XcBtcLegFunded`/`XcSeqLegLocked`); maker side is `RunMakerForward`
  (xdriver.go:665), which mints from the taker's hash and never learns P. Verify-not-trust:
  the LSP verifies the maker's asset leg binds claim=the real taker's pubkey on the taker's
  H before relaying (analog of verifyMakerBtcHtlc).
- LSP vs covenant: LSP fills the covenant on-chain (seqdex `bridge` package) and runs the
  submarine hop with the taker.

## RECEIVER-direction P2P path (symmetry)
The receiver bridge is built LSP-only. Its P2P analog is the NORMAL submarine
(`xdriver_submarine.go`, LnAssetForBTC=0): taker sells asset on-chain, receives BTC-LN,
taker as secret holder. Needs: classify ln_direction=0 submarine asks as P2P-capable;
route to it when the bid-maker advertises btc_ln+interactive; wire the taker driver (mint
bolt11 on P, fund asset HTLC claim=maker, await the maker's pay -> maker claims). Single
on-chain HTLC -> same single T_seq gate. LSP receiver bridge stays as the mismatch fallback.

## Offer format — SettlementCapabilities (additive, signed)
The signed Offer uses `oneof settlement` (one variant per intent). Add a signed capability
descriptor so ONE resting intent advertises its full settlement surface:
```
message SettlementCapabilities {
  bool btc_onchain=1; bool btc_ln=2; bool asset_onchain=3; bool asset_ln=4;
  bool interactive=5;            // online + runs the handshake live (false => passive covenant)
  bool maker_can_hold_invoice=6; // node runs the holdinvoice plugin
}
```
Part of the maker-signed bytes (a relay can't forge it). A CovenantTerms offer IMPLIES
{interactive:false, asset_onchain:true, btc_*:false} regardless of stated caps (chain is
authoritative). Matching stays rail-blind; caps are read only at settlement.
Relay: extend `classifyRelayOffer` (unified-book.mjs:39-50) to recognize ln_direction 0/1
and surface `meta.caps` (generalize the existing `interactive` flag).

## Routing — chooseSettlementPath(match, offerCaps)
New pure function in settlement-router.mjs, consumed by swap.js review + LSP /swap dispatch:
```
plan = planSettlement(match)                       // rail-blind cross detect (unchanged)
if plan.happyCoincidence: return {path:'native'}   // rails coincide, no bridge
for the crossed (BTC) leg, takerBtcRail='ln':
  if offerCaps.interactive && offerCaps.btc_ln:
     return {path:'p2p-submarine', ln_direction: side==='buy' ? 1 : 0}
  else:
     return {path:'lsp-bridge', lnSide: side==='buy' ? 'payer' : 'receiver'}
```
`crossingShapeSupported` (bridge-driver.mjs:276-280) becomes "which shapes the LSP FALLBACK
settles"; extend to admit `btcLeg.bridge && lnSide==='payer' && native asset leg`.

## File-by-file
**seqdex:** offer.proto add SettlementCapabilities + Offer field (regen offer.pb.go);
validator.go validate caps vs ln_direction/trade_dir + covenant=>non-interactive; confirm
xdriver_submarine{,_reverse}.go reachable from the wallet courier; confirm RunMakerForward
accepts an LSP-funded BTC leg. xdriver_submarine_reverse.go: `coupleSubReverse` sizes T_seq +
the invoice min_final_cltv from ONE invariant (fc 8 / ratio 60 / minAnchorDepth 3 / T_seq 820, evaluated at the
post-anchor-bury tip) so an honest reverse offer clears the taker's corrected hold-CLTV gate;
cmd/seqob-maker/submarine.go sets both.
**LSP (sequentia-web-wallet/tooling/lsp):** unified-book.mjs classify ln_direction 0/1 +
surface caps; settlement-router.mjs add chooseSettlementPath; bridge-driver.mjs
crossingShapeSupported+describeCrossingSupport admit payer; bridge-maker.mjs add
runForwardBridgeTerms + verifyMakerAssetLeg; leg-bridge.mjs add a payer-side front-time gate
(analog of checkBridgeLocktimeOrdering/verifyFrontRouteExpiry verifying ACTUAL incoming-HTLC
CLTV covers T_seq); lsp-server.mjs new POST /bridge/hold (LSP ISSUES a BTC-LN hold on the
taker's H), observe() payer branch points s.lnRpc at the LSP's own node, prepareBridgeLegs
payer branch drives runForwardBridgeTerms, complete refundBridgeHtlcBtc (1981-1987 stub),
/swap route bridge:true BUY into runBridgedSwapJob + narrow the xsubbuy 422 to non-bridge.
**web swap.js:** call chooseSettlementPath; dispatch P2P submarine (both directions) vs LSP
bridge; bridgedTakePlan/startBridged BUY variant + payer bridgedSteps driver; STOP the inline
channel + startMixed for buy-btcln-assetchain; honest-disable Review when a shape REQUIRES
the bridge and the book is empty (never fall through to startMixed/422); disable Review up
front for a sub-asset BUY when subassetCapable is false.
**Ambra:** swap_route.dart stop degrading btcRail=='ln'&&assetRail=='chain' (return the real
mixed/bridge kind for BOTH directions); wire XchainReverseSwapScreen for the cross SELL; add
bridged-buy + bridged-sell screens posting bridge:true; COUPLE the same-chain rail toggles;
BUILD same-chain pure-LN (enable + wire LightningSwapScreen); lsp_client.swap send
node_key/counter_node_key/offer_id/maker_pubkey (self-custody + pin); pre-check /lnbook
liquidity before navigating.
