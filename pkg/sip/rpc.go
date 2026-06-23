package sip

import (
	"context"
	"strings"

	"github.com/livekit/protocol/livekit/roomrpc/siprpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// getHeadersIgnore is a set of header that are filtered out of the participant RPC response.
var getHeadersIgnore = map[string]struct{}{
	"route":               {},
	"record-route":        {},
	"via":                 {},
	"supported":           {},
	"accept":              {},
	"allow":               {},
	"allow-events":        {},
	"content-type":        {},
	"content-length":      {},
	"content-encoding":    {},
	"content-disposition": {},
	"cseq":                {},
	"event":               {},
	"expires":             {},
	"session-expires":     {},
	"min-se":              {},
	"max-forwards":        {},
}

func rpcGetHeaders(ctx context.Context, cc Signaling, r *siprpc.GetRemoteHeadersV1Request) (*siprpc.GetRemoteHeadersV1Response, error) {
	var (
		only map[string]struct{}
		skip map[string]struct{}
	)
	if list := r.Include; len(list) != 0 {
		only = make(map[string]struct{}, len(list))
		for _, h := range list {
			only[strings.ToLower(h)] = struct{}{}
		}
	}
	if list := r.Exclude; len(list) != 0 {
		skip = make(map[string]struct{}, len(list))
		for _, h := range list {
			skip[strings.ToLower(h)] = struct{}{}
		}
	}
	out := make(map[string]string)
	for _, h := range cc.RemoteHeaders() {
		name := strings.ToLower(h.Name())
		if _, ok := getHeadersIgnore[name]; ok {
			continue
		}
		if _, ok := skip[name]; ok {
			continue
		}
		if _, ok := only[name]; !ok && only != nil {
			continue
		}
		out[h.Name()] = h.Value()
	}
	return &siprpc.GetRemoteHeadersV1Response{
		Headers: out,
	}, nil
}

func rpcEndCall(ctx context.Context, call CallInterface, r *siprpc.EndCallV1Request) (*siprpc.EndCallV1Response, error) {
	err := call.EndCall(ctx, r.Headers)
	if err != nil {
		return nil, err
	}
	return &siprpc.EndCallV1Response{}, nil
}

func registerSignalingRPC(r RoomInterface, cc Signaling) error {
	err := lksdk.RegisterRPCMethodJSON(r, "lk.sip.GetRemoteHeaders", func(ctx context.Context, r *siprpc.GetRemoteHeadersV1Request) (*siprpc.GetRemoteHeadersV1Response, error) {
		return rpcGetHeaders(ctx, cc, r)
	})
	if err != nil {
		return err
	}
	return nil
}

type CallInterface interface {
	EndCall(ctx context.Context, headers map[string]string) error
}

func registerCallRPC(r RoomInterface, call CallInterface) error {
	err := lksdk.RegisterRPCMethodJSON(r, "lk.sip.EndCall", func(ctx context.Context, r *siprpc.EndCallV1Request) (*siprpc.EndCallV1Response, error) {
		return rpcEndCall(ctx, call, r)
	})
	if err != nil {
		return err
	}
	return nil
}
