// Package seqnet defines the Sequentia network parameters for the go-elements
// address / blinding / PSET machinery used by the daemon.
//
// Sequentia is an Elements 23.x fork, so the transaction, confidential-asset and
// PSETv2 formats are inherited unchanged; only the network parameters differ from
// Liquid. Addresses are deliberately Bitcoin-identical (same base58 version bytes
// and bech32 HRPs as Bitcoin main/test/regtest); the Sequentia-specific bit is the
// confidential (blech32) HRP — "sqb" on mainnet, "tsqb" on testnet.
//
// The native/policy asset is derived from each network's genesis and is therefore
// NOT a compile-time constant: it is supplied at runtime (read from the ocean
// wallet's GetInfo.nativeAsset, which the wallet itself reads from the node's
// getsidechaininfo.pegged_asset) and left empty here.
//
// This mirrors wallet/pkg/seqnet but additionally maps the daemon's ocean-wallet
// network signal: the ocean adapter stringifies the ocean.v1 GetInfoResponse
// network enum to "mainnet"/"testnet"/"regtest" (NOT the Sequentia names), so
// ByName accepts both spellings.
package seqnet

import "github.com/vulpemventures/go-elements/network"

// Network name keys.
const (
	Mainnet = "sequentia"
	Testnet = "sequentia-testnet"
	Regtest = "sequentia-regtest"
)

// SequentiaMainnet — Bitcoin-identical base58/bech32, confidential HRP "sqb".
var SequentiaMainnet = network.Network{
	Name:         Mainnet,
	Bech32:       "bc",
	Blech32:      "sqb",
	HDPublicKey:  [4]byte{0x04, 0x88, 0xb2, 0x1e}, // xpub
	HDPrivateKey: [4]byte{0x04, 0x88, 0xad, 0xe4}, // xprv
	PubKeyHash:   0,
	ScriptHash:   5,
	Wif:          0x80,
	Confidential: 12,
	AssetID:      "", // runtime: getsidechaininfo.pegged_asset
}

// SequentiaTestnet — Bitcoin-testnet-identical base58/bech32, confidential HRP "tsqb".
var SequentiaTestnet = network.Network{
	Name:         Testnet,
	Bech32:       "tb",
	Blech32:      "tsqb",
	HDPublicKey:  [4]byte{0x04, 0x35, 0x87, 0xcf}, // tpub
	HDPrivateKey: [4]byte{0x04, 0x35, 0x83, 0x94}, // tprv
	PubKeyHash:   111,
	ScriptHash:   196,
	Wif:          0xef,
	Confidential: 70,
	AssetID:      "", // runtime: getsidechaininfo.pegged_asset
}

// SequentiaRegtest — Bitcoin-regtest-identical; blech32 == bech32 ("bcrt").
var SequentiaRegtest = network.Network{
	Name:         Regtest,
	Bech32:       "bcrt",
	Blech32:      "bcrt",
	HDPublicKey:  [4]byte{0x04, 0x35, 0x87, 0xcf},
	HDPrivateKey: [4]byte{0x04, 0x35, 0x83, 0x94},
	PubKeyHash:   111,
	ScriptHash:   196,
	Wif:          0xef,
	Confidential: 4,
	AssetID:      "", // runtime: getsidechaininfo.pegged_asset
}

// byKey maps every recognised network signal — both the Sequentia names and the
// ocean adapter's enum stringification ("mainnet"/"testnet"/"regtest") — to its
// parameters. The daemon's ocean-wallet adapter reports "mainnet"/"testnet"/
// "regtest"; the wallet's own config uses the Sequentia names; we accept both.
var byKey = map[string]*network.Network{
	Mainnet:   &SequentiaMainnet,
	Testnet:   &SequentiaTestnet,
	Regtest:   &SequentiaRegtest,
	"mainnet": &SequentiaMainnet,
	"testnet": &SequentiaTestnet,
	"regtest": &SequentiaRegtest,
	// go-elements default Liquid name maps to Sequentia mainnet, in case some
	// upstream code path still reports "liquid".
	"liquid": &SequentiaMainnet,
}

// ByName returns a COPY of the network matching the given signal and whether it
// is known. Callers that set AssetID (from the wallet's native asset) get an
// isolated value, never mutating the shared definition.
func ByName(name string) (network.Network, bool) {
	n, ok := byKey[name]
	if !ok {
		return network.Network{}, false
	}
	return *n, true
}
