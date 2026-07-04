package client

// xcourier_pureln.go: the courier message atoms for a PURE-LN swap (both legs
// off-chain Lightning, stitched by one shared secret — no on-chain leg, no
// anchor gate). See pkg/xchain PureLNSwap for the settlement engine.
//
// BUY handshake (taker buys the asset with BTC; maker holds the incoming BTC
// leg and pays the asset leg — ln_direction LnBTCLNForAssetLN):
//
//	taker -> XcPlnTermsRequest{}
//	maker -> XcPlnTerms{maker_ln_node_id (BTC leg), btc_amount, asset_amount}
//	taker -> XcPlnAssetInvoice{hash_h, bolt11(asset invoice on taker's P)}
//	maker -> XcPlnHoldReady{hash_h}            (BTC hold registered on H)
//	taker  ... pays the maker's BTC hold by bare hash (off-courier LN)
//	maker  ... waits held -> pays the asset invoice (learns P) -> settles the hold
//	maker -> XcPlnSettled{}                     (informational; the taker already has its asset)
//
// The SELL handshake (taker sells the asset for BTC) is the mirror: the maker
// holds the incoming asset leg and pays BTC; maker_ln_node_id is then the asset
// leg's node id and the taker's invoice is a BTC invoice.
const (
	XcPlnTermsRequest XcMsgType = "pln_terms_request"
	XcPlnTerms        XcMsgType = "pln_terms"         // maker's per-lift terms + its incoming-leg node id
	XcPlnAssetInvoice XcMsgType = "pln_asset_invoice" // taker's invoice on H (the leg the maker pays)
	XcPlnHoldReady    XcMsgType = "pln_hold_ready"    // maker registered the incoming hold on H
	XcPlnSettled      XcMsgType = "pln_settled"       // maker settled its hold (informational)
)
