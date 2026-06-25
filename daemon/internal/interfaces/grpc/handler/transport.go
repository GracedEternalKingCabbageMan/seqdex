package grpchandler

import (
	"context"

	seqdexv1 "github.com/aejkcs50/seqdex/daemon/api-spec/protobuf/gen/seqdex/v1"
)

type transportHandler struct{}

func NewTransportHandler() seqdexv1.TransportServiceServer {
	return newTransportHandler()
}

func newTransportHandler() *transportHandler {
	return &transportHandler{}
}

func (t transportHandler) SupportedContentTypes(
	context.Context, *seqdexv1.SupportedContentTypesRequest,
) (*seqdexv1.SupportedContentTypesResponse, error) {
	return &seqdexv1.SupportedContentTypesResponse{
		AcceptedTypes: []seqdexv1.ContentType{
			seqdexv1.ContentType_CONTENT_TYPE_JSON,
			seqdexv1.ContentType_CONTENT_TYPE_GRPC,
			seqdexv1.ContentType_CONTENT_TYPE_GRPCWEB,
			seqdexv1.ContentType_CONTENT_TYPE_GRPCWEBTEXT,
		},
	}, nil
}
