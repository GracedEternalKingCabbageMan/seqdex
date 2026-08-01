package client

// xcourier_subasset.go names the courier messages for the SUB-ASSET swap: a taker
// pays BTC ON-CHAIN and receives a Sequentia asset OVER LIGHTNING. It is the exact
// MIRROR of the deployed submarine (asset on-chain <-> BTC over LN): here the
// on-chain leg is BTC and the Lightning leg is the asset, so the leg roles swap
// while the one-shared-secret hashlock is unchanged.
//
// Direction (maker perspective): the maker SELLS the asset. It RECEIVES BTC in an
// on-chain HTLC the taker funds (claim = maker with P) and PAYS the asset over
// Lightning into a hold invoice the TAKER issues on H. The taker holds P (it is
// the LN receiver); it releases P by SETTLING its hold invoice once the maker's
// asset payment is held, and the maker uses that P to claim the on-chain BTC HTLC.
//
//	roles:
//	  taker : funds the BTC HTLC (claim=maker, refund=taker after T_btc), issues an
//	          asset hold invoice on H, waits for the maker's held asset payment,
//	          settles it with P (receiving the asset), and refunds the BTC HTLC at
//	          T_btc if the maker never pays.
//	  maker : verifies the taker's on-chain BTC HTLC, pays the asset hold invoice
//	          (blocks until the taker settles -> learns P), then claims the BTC
//	          HTLC with P.
//
// The messages reuse the shared XcMsg fields (MakerBtcClaimPub, BtcLocktime,
// BtcAmount/SeqAmount, MakerLNNodeID, HashH, TakerBtcRefundPub, Leg, Bolt11,
// Preimage, SettleTxid) exactly as the cross and submarine drivers do; no new
// wire fields are introduced.
const (
	XcSubAsTermsRequest XcMsgType = "subas_terms_request" // taker -> maker: request per-lift terms
	XcSubAsTerms        XcMsgType = "subas_terms"         // maker -> taker: BTC claim pubkey, T_btc, amounts, asset LN node id
	XcSubAsBtcFunded    XcMsgType = "subas_btc_funded"    // taker -> maker: funded BTC HTLC leg + H + taker refund pubkey + asset hold invoice
	XcSubAsBtcVerified  XcMsgType = "subas_btc_verified"  // maker -> taker: BTC HTLC verified; the maker is about to pay the asset invoice
	XcSubAsSettled      XcMsgType = "subas_settled"       // maker -> taker: claimed the BTC HTLC with P (informational: preimage + btc claim txid)
)
