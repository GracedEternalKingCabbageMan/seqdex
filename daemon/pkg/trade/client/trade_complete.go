package tradeclient

import (
	"context"
	"errors"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"

	"google.golang.org/protobuf/proto"
)

// TradeCompleteOpts is the struct given to TradeComplete method
type TradeCompleteOpts struct {
	SwapComplete []byte
	SwapFail     []byte
}

var (
	// ErrNullTradeCompleteOpts ...
	ErrNullTradeCompleteOpts = errors.New("swap complete and swap fail messages must not be both null")
	// ErrInvalidTradeCompleteOpts ...
	ErrInvalidTradeCompleteOpts = errors.New("swap complete and swap fail messages must not be both defined")
	// ErrInvalidSwapCompleteMessage ...
	ErrInvalidSwapCompleteMessage = errors.New("swap complete must be a valid serialized message")
	// ErrInvalidSwapFailMessage ...
	ErrInvalidSwapFailMessage = errors.New("swap fail must be a valid serialized message")
)

func (o TradeCompleteOpts) validate() error {
	if len(o.SwapComplete) <= 0 && len(o.SwapFail) <= 0 {
		return ErrNullTradeCompleteOpts
	}
	if len(o.SwapComplete) > 0 && len(o.SwapFail) > 0 {
		return ErrInvalidTradeCompleteOpts
	}
	if len(o.SwapComplete) > 0 {
		if err := proto.Unmarshal(o.SwapComplete, &seqdexv1.SwapComplete{}); err != nil {
			return ErrInvalidSwapCompleteMessage
		}
	}
	if len(o.SwapFail) > 0 {
		if err := proto.Unmarshal(o.SwapFail, &seqdexv1.SwapFail{}); err != nil {
			return ErrInvalidSwapFailMessage
		}
	}

	return nil
}

// TradeComplete crafts the request and calls the TradeComplete rpc
func (c *Client) TradeComplete(opts TradeCompleteOpts) (*seqdexv1.CompleteTradeResponse, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	var swapComplete *seqdexv1.SwapComplete
	var swapFail *seqdexv1.SwapFail
	if len(opts.SwapComplete) > 0 {
		swapComplete = &seqdexv1.SwapComplete{}
		//nolint
		proto.Unmarshal(opts.SwapComplete, swapComplete)
	}
	if len(opts.SwapFail) > 0 {
		swapFail = &seqdexv1.SwapFail{}
		//nolint
		proto.Unmarshal(opts.SwapFail, swapFail)
	}

	request := &seqdexv1.CompleteTradeRequest{
		SwapComplete: swapComplete,
		SwapFail:     swapFail,
	}
	return c.client.CompleteTrade(context.Background(), request)
}
