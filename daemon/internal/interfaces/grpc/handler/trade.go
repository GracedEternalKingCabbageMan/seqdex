package grpchandler

import (
	"context"
	"strings"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
	"github.com/aejkcs50/seqdex/daemon/internal/core/application"
	"github.com/aejkcs50/seqdex/daemon/internal/core/ports"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const ErrCannotServeRequest = "cannot serve request, please retry"

type tradeHandler struct {
	tradeSvc application.TradeService
}

func NewTradeHandler(
	tradeSvc application.TradeService,
) seqdexv1.TradeServiceServer {
	return newTradeHandler(tradeSvc)
}

func newTradeHandler(tradeSvc application.TradeService) *tradeHandler {
	return &tradeHandler{
		tradeSvc: tradeSvc,
	}
}

func (t tradeHandler) ListMarkets(
	ctx context.Context, _ *seqdexv1.ListMarketsRequest,
) (*seqdexv1.ListMarketsResponse, error) {
	return t.listMarkets(ctx)
}

func (t tradeHandler) GetMarketBalance(
	ctx context.Context, req *seqdexv1.GetMarketBalanceRequest,
) (*seqdexv1.GetMarketBalanceResponse, error) {
	return t.getMarketBalance(ctx, req)
}

func (t tradeHandler) GetMarketPrice(
	ctx context.Context, req *seqdexv1.GetMarketPriceRequest,
) (*seqdexv1.GetMarketPriceResponse, error) {
	marketPrice, err := t.getMarketPrice(ctx, req)
	if err != nil {
		return nil, err
	}

	marketBalance, err := t.getMarketBalance(ctx, &seqdexv1.GetMarketBalanceRequest{
		Market: req.GetMarket(),
	})
	if err != nil {
		return nil, err
	}

	return &seqdexv1.GetMarketPriceResponse{
		SpotPrice:         marketPrice.SpotPrice,
		MinTradableAmount: marketPrice.MinTradableAmount,
		Balance:           marketBalance.Balance,
	}, nil
}

func (t tradeHandler) PreviewTrade(
	ctx context.Context, req *seqdexv1.PreviewTradeRequest,
) (*seqdexv1.PreviewTradeResponse, error) {
	return t.previewTrade(ctx, req)
}

func (t tradeHandler) ProposeTrade(
	ctx context.Context, req *seqdexv1.ProposeTradeRequest,
) (*seqdexv1.ProposeTradeResponse, error) {
	return t.proposeTrade(ctx, req)
}

func (t tradeHandler) CompleteTrade(
	ctx context.Context, req *seqdexv1.CompleteTradeRequest,
) (*seqdexv1.CompleteTradeResponse, error) {
	return t.completeTrade(ctx, req)
}

func (t tradeHandler) listMarkets(
	ctx context.Context,
) (*seqdexv1.ListMarketsResponse, error) {
	markets, err := t.tradeSvc.GetTradableMarkets(ctx)
	if err != nil {
		return nil, err
	}

	marketsWithFee := make([]*seqdexv1.MarketWithFee, 0, len(markets))
	for _, v := range markets {
		m := &seqdexv1.MarketWithFee{
			Market: seqMarket{v.GetMarket()}.toProto(),
			Fee: seqMarketFeeInfo{
				v.GetPercentageFee(), v.GetFixedFee(),
			}.toProto(),
		}
		marketsWithFee = append(marketsWithFee, m)
	}

	return &seqdexv1.ListMarketsResponse{Markets: marketsWithFee}, nil
}

func (t tradeHandler) getMarketBalance(
	ctx context.Context, req *seqdexv1.GetMarketBalanceRequest,
) (*seqdexv1.GetMarketBalanceResponse, error) {
	market, err := parseMarketSeq(req.GetMarket())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	info, err := t.tradeSvc.GetMarketBalance(ctx, market)
	if err != nil {
		return nil, err
	}

	var baseBalance, quoteBalance uint64
	if balance := info.GetBalance(); len(balance) > 0 {
		baseBalance = balance[market.GetBaseAsset()].GetConfirmedBalance()
		quoteBalance = balance[market.GetQuoteAsset()].GetConfirmedBalance()
	}
	return &seqdexv1.GetMarketBalanceResponse{
		Balance: &seqdexv1.Balance{
			BaseAmount:  baseBalance,
			QuoteAmount: quoteBalance,
		},
		Fee: seqMarketFeeInfo{
			info.GetPercentageFee(), info.GetFixedFee(),
		}.toProto(),
	}, nil
}

func (t tradeHandler) getMarketPrice(
	ctx context.Context, req *seqdexv1.GetMarketPriceRequest,
) (*seqdexv1.GetMarketPriceResponse, error) {
	market, err := parseMarketSeq(req.GetMarket())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	price, minAmount, err := t.tradeSvc.GetMarketPrice(ctx, market)
	if err != nil {
		return nil, err
	}

	spotPrice, _ := price.Float64()
	return &seqdexv1.GetMarketPriceResponse{
		SpotPrice:         spotPrice,
		MinTradableAmount: minAmount,
	}, nil
}

func (t tradeHandler) previewTrade(
	ctx context.Context, req *seqdexv1.PreviewTradeRequest,
) (*seqdexv1.PreviewTradeResponse, error) {
	market, err := parseMarketSeq(req.GetMarket())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	tradeType, err := parseTradeTypeSeq(req.GetType())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	amount, err := parseAmount(req.GetAmount())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	asset, err := parseAsset(req.GetAsset())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	feeAsset, err := parseAsset(req.GetFeeAsset())
	if err != nil {
		// Change 'invalid asset' message to 'invalid fee asset'.
		errMsg := strings.Replace(err.Error(), " ", " fee ", -1)
		return nil, status.Error(codes.InvalidArgument, errMsg)
	}

	preview, err := t.tradeSvc.TradePreview(
		ctx, market, tradeType, amount, asset, feeAsset,
	)
	if err != nil {
		return nil, err
	}

	return &seqdexv1.PreviewTradeResponse{
		Previews: []*seqdexv1.Preview{
			{
				Price: seqMarketPriceInfo{preview.GetMarketPrice()}.toProto(),
				Fee: seqMarketFeeInfo{
					preview.GetMarketPercentageFee(), preview.GetMarketFixedFee(),
				}.toProto(),
				Amount:    preview.GetAmount(),
				Asset:     preview.GetAsset(),
				FeeAmount: preview.GetFeeAmount(),
				FeeAsset:  preview.GetFeeAsset(),
			},
		},
	}, nil
}

func (t tradeHandler) proposeTrade(
	ctx context.Context, req *seqdexv1.ProposeTradeRequest,
) (*seqdexv1.ProposeTradeResponse, error) {
	market, err := parseMarketSeq(req.GetMarket())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	tradeType, err := parseTradeTypeSeq(req.GetType())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	swapRequest, err := parseSwapRequestSeq(
		req.GetSwapRequest(), req.GetFeeAsset(), req.GetFeeAmount(),
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	accept, fail, swapExpiryTime, err := t.tradeSvc.TradePropose(
		ctx, market, tradeType, swapRequest,
	)
	if err != nil {
		return nil, err
	}

	return &seqdexv1.ProposeTradeResponse{
		SwapAccept:     seqSwapAcceptInfo{accept}.toProto(),
		SwapFail:       seqSwapFailInfo{fail}.toProto(),
		ExpiryTimeUnix: uint64(swapExpiryTime),
	}, nil
}

func (t tradeHandler) completeTrade(
	ctx context.Context, req *seqdexv1.CompleteTradeRequest,
) (*seqdexv1.CompleteTradeResponse, error) {
	var swapComplete ports.SwapComplete
	if s := req.SwapComplete; s != nil {
		swapComplete = s
	}
	var swapFail ports.SwapFail
	if s := req.SwapFail; s != nil {
		swapFail = s
	}
	txID, fail, err := t.tradeSvc.TradeComplete(
		ctx, swapComplete, swapFail,
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var swapFailStub *seqdexv1.SwapFail
	if fail != nil {
		swapFailStub = &seqdexv1.SwapFail{
			Id:             fail.GetId(),
			MessageId:      fail.GetMessageId(),
			FailureCode:    fail.GetFailureCode(),
			FailureMessage: fail.GetFailureMessage(),
		}
	}

	return &seqdexv1.CompleteTradeResponse{
		Txid:     txID,
		SwapFail: swapFailStub,
	}, nil
}
