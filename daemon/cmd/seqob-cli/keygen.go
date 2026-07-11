package main

// keygen.go: emit a fresh secp256k1 keypair. A wallet/device uses this to mint its BTC
// claim key for a sub-asset SELL: the PUBKEY goes to the LSP in POST /swap (btc_claim_pub),
// the PRIVKEY never leaves the device and later claims the BTC HTLC (xsubas-claim-btc).

import (
	"encoding/hex"
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/xchain"
)

func cmdKeygen(args []string) {
	fs := newFlagSet("keygen")
	jsonOut := fs.Bool("json", false, "emit {priv,pub} as one JSON line")
	_ = fs.Parse(args)
	k, err := xchain.NewKey()
	if err != nil {
		fatal("mint key: %v", err)
	}
	priv := hex.EncodeToString(k.Bytes())
	pub := hex.EncodeToString(k.PubKey())
	if *jsonOut {
		fmt.Printf("{\"priv\":%q,\"pub\":%q}\n", priv, pub)
		return
	}
	fmt.Printf("priv %s\npub  %s\n", priv, pub)
}
