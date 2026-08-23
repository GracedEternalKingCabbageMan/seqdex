package client

// xcourier_subasset_sell.go names the courier messages for the SUB-ASSET SELL swap:
// a taker pays an asset OVER LIGHTNING and receives BTC ON-CHAIN. It is the exact
// MIRROR of the sub-asset BUY (xcourier_subasset.go): there the taker locks BTC
// on-chain and the maker pays the asset over LN; here the MAKER locks BTC on-chain
// and the taker pays the asset over LN. The maker holds the preimage.
//
// Direction (maker perspective): the maker BUYS the asset. It LOCKS BTC in an
// on-chain HTLC (claim = taker with P, refund = maker after T_btc) and offers an
// asset HOLD invoice on the same H (maker owns P). The taker pays the asset invoice
// over LN; the maker settles it with P to TAKE the asset, which reveals P to the
// taker; the taker then claims the maker's on-chain BTC HTLC with P.
//
//	roles:
//	  maker : generates P, funds the BTC HTLC (claim=taker, refund=maker after T_btc),
//	          issues an asset hold invoice on H, waits for the taker's held asset
//	          payment, SETTLES it with P (taking the asset + revealing P), and reclaims
//	          the BTC HTLC at T_btc if the taker never pays.
//	  taker : verifies the maker's on-chain BTC HTLC, pays the asset hold invoice over
//	          LN (learning P when the maker settles), and claims the BTC HTLC with P.
//	          If it never pays, its LN payment simply never leaves — nothing lost.
//
// Reuses the shared XcMsg fields (TakerBtcClaimPub, MakerRefundPub, BtcLocktime,
// BtcAmount/SeqAmount, MakerLNNodeID, HashH, Bolt11, Leg, Preimage) — no new wire
// fields.
//
// PARTIAL FILLS: the terms request carries SeqAmount = the asset-side slice the
// taker will pay (atoms, JSON number; 0/absent = the whole offer, which is what an
// older taker sends). The maker's terms echo SeqAmount = the granted slice and
// BtcAmount = the slice priced at the SIGNED offer's ratio, rounded UP
// (ProportionalBtc, the taker-receives-the-on-chain-leg direction), and Leg.Amount
// carries exactly that priced slice — both sides derive the same ceil from the
// same signed offer, so neither can re-price the other.
const (
	XcSubAsSellTermsRequest XcMsgType = "subassell_terms_request" // taker -> maker: request + the taker's BTC claim pubkey + the asset slice (SeqAmount)
	XcSubAsSellTerms        XcMsgType = "subassell_terms"         // maker -> taker: asset hold invoice (bolt11 on H) + funded BTC HTLC leg (slice-sized) + H
	XcSubAsSellSettled      XcMsgType = "subassell_settled"       // maker -> taker: settled the asset hold with P (informational)
)
