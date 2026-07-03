package client

// xcourier_submarine.go adds the SUBMARINE-SWAP (Sequentia asset on-chain <->
// BTC over Lightning) lift handshake to the cross-chain courier. It reuses the
// same opaque relay transport, session Crypter, and XcMsg envelope (Seal/OpenXcMsg)
// as the on-chain cross-chain path (xcourier.go); only the message TYPES and the
// BTC-leg representation differ — the BTC leg is a BOLT11 payment (XcMsg.Bolt11),
// not an on-chain HTLC (XcLeg). The Sequentia asset leg and the anchor-depth gate
// are the proven pkg/xchain primitives (submarine.go), unchanged.
//
// v1 = the NORMAL direction (offer sells a Sequentia asset for BTC-LN; secret
// holder = TAKER). It needs no hold-invoice plugin (maker pays with core `pay`).
//
//	NORMAL  (ln_direction = ASSET_ONCHAIN_FOR_BTC_LN; taker sells asset, receives BTC-LN)
//	  taker -> XcSubTermsRequest
//	  maker -> XcSubTerms{maker_seq_claim_pub, seq_locktime, seq_amount, min_anchor_depth}
//	  taker  : mint a BOLT11 on a chosen preimage P (payment_hash = H); fund the asset
//	           HTLC (claim=maker, refund=taker)
//	  taker -> XcSubAssetFunded{hash_h, taker_seq_refund_pub, leg(SEQ block_hash+anchor),
//	           bolt11}
//	  maker  : VerifySEQLeg -> wait the funding is anchor-buried >= min_anchor_depth ->
//	           Pay(bolt11) (learns P) -> ClaimSEQLeg(P)
//	  maker -> XcSubSettled{settle_txid}   (informational; the taker already has its BTC-LN)
//	  If the maker never pays, the taker refunds the asset HTLC after seq_locktime.
//
// The REVERSE maker-secret direction (ln_direction = BTC_LN_FOR_ASSET_ONCHAIN) is
// the mirror: taker -> XcSubTermsRequest{taker_seq_claim_pub}; maker ->
// XcSubAssetLocked{hash_h, maker_seq_refund_pub, seq_locktime, leg(SEQ), bolt11}; the
// taker gates the maker's asset HTLC (VerifySeqAnchorBuried) BEFORE paying the
// invoice, learns P, and claims the asset. Wired in a later slice.
const (
	XcSubTermsRequest XcMsgType = "sub_terms_request"
	XcSubTerms        XcMsgType = "sub_terms"        // normal: maker's per-lift terms
	XcSubAssetFunded  XcMsgType = "sub_asset_funded" // normal: taker funded the asset HTLC + bolt11
	XcSubAssetLocked  XcMsgType = "sub_asset_locked" // reverse: maker locked the asset HTLC + bolt11
	XcSubSettled      XcMsgType = "sub_settled"      // maker claimed the asset (informational)
)
