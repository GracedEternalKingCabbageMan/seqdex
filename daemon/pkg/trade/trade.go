package trade

import (
	"fmt"

	"github.com/aejkcs50/seqdex/daemon/pkg/explorer"
	"github.com/aejkcs50/seqdex/daemon/pkg/seqnet"
	tradeclient "github.com/aejkcs50/seqdex/daemon/pkg/trade/client"
	"github.com/vulpemventures/go-elements/network"
)

var (
	// ErrInvalidChain ...
	ErrInvalidChain = fmt.Errorf(
		"chain must be a known Sequentia network (e.g. '%s', '%s' or '%s')",
		seqnet.Mainnet, seqnet.Testnet, seqnet.Regtest,
	)
	// ErrInvalidProviderURL ...
	ErrInvalidProviderURL = fmt.Errorf(
		"provider url must be a valid url in the form 'host:port'",
	)
	// ErrNullExplorer ...
	ErrNullExplorer = fmt.Errorf("explorer must not be null")
	// ErrNullClient ...
	ErrNullClient = fmt.Errorf("client must not be null")
)

type Trade struct {
	network  *network.Network
	explorer explorer.Service
	client   *tradeclient.Client
}

// NewTradeOpts is the struct given to NewTrade method
type NewTradeOpts struct {
	// Chain is a Sequentia network signal accepted by seqnet.ByName, i.e. one
	// of the Sequentia names ("sequentia", "sequentia-testnet",
	// "sequentia-regtest") or the ocean-adapter spellings
	// ("mainnet"/"testnet"/"regtest").
	Chain string
	// NativeAsset is the network's policy/native asset (the node's
	// getsidechaininfo.pegged_asset). It is genesis-derived and therefore must
	// be supplied at runtime; it is stamped onto the network's AssetID so the
	// go-elements PSET/blinding machinery treats it as the fee asset.
	NativeAsset     string
	ExplorerService explorer.Service
	Client          *tradeclient.Client
}

func (o NewTradeOpts) validate() error {
	if !isValidChain(o.Chain) {
		return ErrInvalidChain
	}
	if o.ExplorerService == nil {
		return ErrNullExplorer
	}
	if o.Client == nil {
		return ErrNullClient
	}
	return nil
}

// NewTrade returns a new trade initialized with the given arguments
func NewTrade(opts NewTradeOpts) (*Trade, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	net := networkFromString(opts.Chain)
	net.AssetID = opts.NativeAsset

	return &Trade{
		network:  net,
		explorer: opts.ExplorerService,
		client:   opts.Client,
	}, nil
}

func isValidChain(chain string) bool {
	_, ok := seqnet.ByName(chain)
	return ok
}

// networkFromString resolves a Sequentia network from its signal. It returns a
// COPY (seqnet.ByName already copies) so callers may safely stamp AssetID.
func networkFromString(chain string) *network.Network {
	net, ok := seqnet.ByName(chain)
	if !ok {
		// Should never happen: callers go through validate() first.
		net = seqnet.SequentiaMainnet
	}
	return &net
}
