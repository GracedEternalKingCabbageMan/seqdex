package tradeclient

import (
	"context"
	"errors"

	trademarket "github.com/aejkcs50/seqdex/daemon/pkg/trade/market"
	tradetype "github.com/aejkcs50/seqdex/daemon/pkg/trade/type"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"

	"google.golang.org/protobuf/proto"
)

var (
	// ErrMalformedSwapRequestMessage ...
	ErrMalformedSwapRequestMessage = errors.New("swap request must be a valid serialized message")
)

// TradeProposeOpts is the struct given to TradePropose method
type TradeProposeOpts struct {
	Market      trademarket.Market
	SwapRequest []byte
	TradeType   tradetype.TradeType
	// FeeAsset / FeeAmount are carried on the ProposeTradeRequest itself (the
	// tdex.v2 SwapRequest message does not serialize them). The daemon's
	// proposeTrade handler validates these top-level fields, so the taker SDK
	// must populate them from the trade preview, otherwise the daemon rejects
	// the proposal with "invalid fee asset".
	FeeAsset  string
	FeeAmount uint64
}

func (o TradeProposeOpts) validate() error {
	if err := o.Market.Validate(); err != nil {
		return err
	}
	if err := proto.Unmarshal(o.SwapRequest, &seqdexv1.SwapRequest{}); err != nil {
		return ErrMalformedSwapRequestMessage
	}
	if err := o.TradeType.Validate(); err != nil {
		return err
	}

	return nil
}

// TradePropose crafts the request and calls the TradePropose rpc
func (c *Client) TradePropose(opts TradeProposeOpts) (*seqdexv1.ProposeTradeResponse, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	market := &seqdexv1.Market{
		BaseAsset:  opts.Market.BaseAsset,
		QuoteAsset: opts.Market.QuoteAsset,
	}
	swapRequest := &seqdexv1.SwapRequest{}
	//nolint
	proto.Unmarshal(opts.SwapRequest, swapRequest)

	request := &seqdexv1.ProposeTradeRequest{
		Market:      market,
		SwapRequest: swapRequest,
		Type:        seqdexv1.TradeType(opts.TradeType),
		FeeAsset:    opts.FeeAsset,
		FeeAmount:   opts.FeeAmount,
	}
	return c.client.ProposeTrade(context.Background(), request)
}
