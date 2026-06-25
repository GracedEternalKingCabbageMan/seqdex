package grpchandler

import (
	"errors"
	"fmt"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
)

// This file holds the seqdex.v1-typed helpers used exclusively by the public
// TradeService/TransportService handlers (trade.go, transport.go). They mirror
// the tdex.v2-typed helpers in types.go/utils.go (which remain in use by the
// untouched Operator API, whose proto still references tdex.v2 messages).
// seqdex.v1 and tdex.v2 are field-identical, so these are straight retypings.

// --- seqdex.v1 market/fee/price conversions (Trade path) ---

type seqMarket struct {
	ports.Market
}

func (m seqMarket) toProto() *seqdexv1.Market {
	return &seqdexv1.Market{
		BaseAsset:  m.GetBaseAsset(),
		QuoteAsset: m.GetQuoteAsset(),
	}
}

type seqMarketFeeInfo struct {
	percentageFee ports.MarketFee
	fixedFee      ports.MarketFee
}

func (i seqMarketFeeInfo) toProto() *seqdexv1.Fee {
	return &seqdexv1.Fee{
		PercentageFee: &seqdexv1.MarketFee{
			BaseAsset:  int64(i.percentageFee.GetBaseAsset()),
			QuoteAsset: int64(i.percentageFee.GetQuoteAsset()),
		},
		FixedFee: &seqdexv1.MarketFee{
			BaseAsset:  int64(i.fixedFee.GetBaseAsset()),
			QuoteAsset: int64(i.fixedFee.GetQuoteAsset()),
		},
	}
}

type seqMarketPriceInfo struct {
	ports.MarketPrice
}

func (i seqMarketPriceInfo) toProto() *seqdexv1.Price {
	info := i.MarketPrice
	basePrice, _ := info.GetBasePrice().Float64()
	quotePrice, _ := info.GetQuotePrice().Float64()
	return &seqdexv1.Price{
		BasePrice:  basePrice,
		QuotePrice: quotePrice,
	}
}

// --- seqdex.v1 trade type ---

type seqTradeTypeInfo seqdexv1.TradeType

func (i seqTradeTypeInfo) IsBuy() bool {
	return seqdexv1.TradeType(i) == seqdexv1.TradeType_TRADE_TYPE_BUY
}
func (i seqTradeTypeInfo) IsSell() bool {
	return seqdexv1.TradeType(i) == seqdexv1.TradeType_TRADE_TYPE_SELL
}

// --- seqdex.v1 swap request/accept/fail ---

type seqSwapRequestInfo struct {
	*seqdexv1.SwapRequest
	feeAsset  string
	feeAmount uint64
}

func (i seqSwapRequestInfo) GetUnblindedInputs() []ports.UnblindedInput {
	info := i.SwapRequest
	list := make([]ports.UnblindedInput, 0, len(info.GetUnblindedInputs()))
	for _, in := range info.GetUnblindedInputs() {
		list = append(list, in)
	}
	return list
}

func (i seqSwapRequestInfo) GetFeeAsset() string {
	return i.feeAsset
}

func (i seqSwapRequestInfo) GetFeeAmount() uint64 {
	return i.feeAmount
}

type seqSwapAcceptInfo struct {
	ports.SwapAccept
}

func (i seqSwapAcceptInfo) toProto() *seqdexv1.SwapAccept {
	info := i.SwapAccept
	if info == nil {
		return nil
	}
	return &seqdexv1.SwapAccept{
		Id:          info.GetId(),
		RequestId:   info.GetRequestId(),
		Transaction: info.GetTransaction(),
	}
}

type seqSwapFailInfo struct {
	ports.SwapFail
}

func (i seqSwapFailInfo) toProto() *seqdexv1.SwapFail {
	info := i.SwapFail
	if info == nil {
		return nil
	}
	return &seqdexv1.SwapFail{
		Id:             info.GetId(),
		MessageId:      info.GetMessageId(),
		FailureCode:    info.GetFailureCode(),
		FailureMessage: info.GetFailureMessage(),
	}
}

// --- seqdex.v1 parse/validate helpers (Trade path) ---

func parseMarketSeq(market *seqdexv1.Market) (ports.Market, error) {
	if market == nil {
		return nil, fmt.Errorf("missing market")
	}
	if !isValidAsset(market.GetBaseAsset()) {
		return nil, errors.New("invalid base asset")
	}
	if !isValidAsset(market.GetQuoteAsset()) {
		return nil, errors.New("invalid quote asset")
	}
	return market, nil
}

func parseTradeTypeSeq(tradeType seqdexv1.TradeType) (ports.TradeType, error) {
	if tradeType != seqdexv1.TradeType_TRADE_TYPE_BUY &&
		tradeType != seqdexv1.TradeType_TRADE_TYPE_SELL {
		return nil, fmt.Errorf("unknown trade type")
	}
	return seqTradeTypeInfo(tradeType), nil
}

func parseSwapRequestSeq(
	sr *seqdexv1.SwapRequest, feeAsset string, feeAmount uint64,
) (ports.SwapRequest, error) {
	if sr == nil {
		return nil, fmt.Errorf("missing swap request")
	}
	if !isValidAmount(sr.GetAmountP()) {
		return nil, fmt.Errorf("invalid swap request amount proposed")
	}
	if !isValidAsset(sr.GetAssetP()) {
		return nil, fmt.Errorf("invalid swap request asset proposed")
	}
	if !isValidAmount(sr.GetAmountR()) {
		return nil, fmt.Errorf("invalid swap request amount received")
	}
	if !isValidAsset(sr.GetAssetR()) {
		return nil, fmt.Errorf("invalid swap request asset received")
	}
	if !isValidTransaction(sr.GetTransaction()) {
		return nil, fmt.Errorf("invalid swap request transaction")
	}
	if !isValidUnblindedInputListSeq(sr.GetUnblindedInputs()) {
		return nil, fmt.Errorf("invalid unblinded input(s)")
	}
	if !isValidAsset(feeAsset) {
		return nil, fmt.Errorf("invalid fee asset")
	}
	if !isValidAmount(feeAmount) {
		return nil, fmt.Errorf("invalid fee amount")
	}
	return seqSwapRequestInfo{sr, feeAsset, feeAmount}, nil
}

func isValidUnblindedInputListSeq(list []*seqdexv1.UnblindedInput) bool {
	for _, in := range list {
		if !isValidIndex(in.GetIndex()) || !isValidAsset(in.GetAsset()) ||
			!isValidAmount(in.GetAmount()) || !isValidAsset(in.GetAssetBlinder()) ||
			!isValidAsset(in.GetAmountBlinder()) {
			return false
		}
	}
	return true
}
