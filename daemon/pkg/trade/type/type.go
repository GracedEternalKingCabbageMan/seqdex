package tradetype

import (
	"errors"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
)

const (
	// Buy type
	Buy TradeType = iota
	// Sell type
	Sell
)

var (
	// ErrInvalidTradeType ...
	ErrInvalidTradeType = errors.New("trade type must be either BUY or SELL")
)

type TradeType int

// Validate makes sure that the current trade type is either BUY or SELL
func (t TradeType) Validate() error {
	if t != TradeType(seqdexv1.TradeType_TRADE_TYPE_BUY) && t != TradeType(seqdexv1.TradeType_TRADE_TYPE_SELL) {
		return ErrInvalidTradeType
	}
	return nil
}

// IsBuy returns whether the current trade type is BUY
func (t TradeType) IsBuy() bool {
	return t == TradeType(seqdexv1.TradeType_TRADE_TYPE_BUY)
}

// IsSell returns whether the current trade type is SELL
func (t TradeType) IsSell() bool {
	return t == TradeType(seqdexv1.TradeType_TRADE_TYPE_SELL)
}

// String formats the type to a human-readable form
func (t TradeType) String() string {
	return seqdexv1.TradeType(t).String()
}
