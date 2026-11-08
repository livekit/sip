package sip

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/livekit/protocol/livekit/roomrpc/siprpc"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"
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
	SendInfo(ctx context.Context, contentType string, body []byte, headers map[string]string) (*sip.Response, error)
}

// MaxInfoBodySize limits the body of a SIP INFO sent through lk.sip.SendInfo.
const MaxInfoBodySize = 64 << 10

// sendInfoIgnore lists headers a caller may not set on an INFO request: they
// identify the dialog or transaction, or are derived from the body.
var sendInfoIgnore = map[string]struct{}{
	"via":            {},
	"from":           {},
	"to":             {},
	"call-id":        {},
	"cseq":           {},
	"contact":        {},
	"max-forwards":   {},
	"route":          {},
	"record-route":   {},
	"content-type":   {},
	"content-length": {},
}

// sendInfoRequest is the lk.sip.SendInfo request. Field names follow protojson
// (lowerCamelCase); the snake_case spellings are accepted as well, so the
// payload matches a future SendInfoV1Request in roomrpc_sip_v1.proto.
type sendInfoRequest struct {
	ContentType string            `json:"contentType,omitempty"`
	Body        string            `json:"body,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

func (r *sendInfoRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		ContentType  *string           `json:"contentType"`
		ContentType2 *string           `json:"content_type"`
		Body         string            `json:"body"`
		Headers      map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = sendInfoRequest{Body: raw.Body, Headers: raw.Headers}
	if raw.ContentType != nil {
		r.ContentType = *raw.ContentType
	} else if raw.ContentType2 != nil {
		r.ContentType = *raw.ContentType2
	}
	return nil
}

// sendInfoResponse is the lk.sip.SendInfo response: the final SIP response the
// remote party sent to the INFO.
type sendInfoResponse struct {
	StatusCode int    `json:"statusCode"`
	Reason     string `json:"reason,omitempty"`
}

func rpcSendInfo(ctx context.Context, call CallInterface, r *sendInfoRequest) (*sendInfoResponse, error) {
	body := []byte(r.Body)
	if len(body) > MaxInfoBodySize {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "INFO body is %d bytes, limit is %d", len(body), MaxInfoBodySize)
	}
	contentType := strings.TrimSpace(r.ContentType)
	if len(body) != 0 && contentType == "" {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "contentType is required when body is set")
	}
	for name := range r.Headers {
		if _, ok := sendInfoIgnore[strings.ToLower(strings.TrimSpace(name))]; ok {
			return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "header %q cannot be set on INFO", name)
		}
	}
	res, err := call.SendInfo(ctx, contentType, body, r.Headers)
	if err != nil {
		return nil, err
	}
	return &sendInfoResponse{StatusCode: int(res.StatusCode), Reason: res.Reason}, nil
}

func registerCallRPC(r RoomInterface, call CallInterface) error {
	err := lksdk.RegisterRPCMethodJSON(r, "lk.sip.EndCall", func(ctx context.Context, r *siprpc.EndCallV1Request) (*siprpc.EndCallV1Response, error) {
		return rpcEndCall(ctx, call, r)
	})
	if err != nil {
		return err
	}
	err = r.RegisterRpcCtxMethod("lk.sip.SendInfo", func(ctx context.Context, data []byte) ([]byte, error) {
		var req sendInfoRequest
		if len(data) != 0 {
			if err := json.Unmarshal(data, &req); err != nil {
				return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "invalid lk.sip.SendInfo request: %v", err)
			}
		}
		resp, err := rpcSendInfo(ctx, call, &req)
		if err != nil {
			return nil, err
		}
		return json.Marshal(resp)
	})
	if err != nil {
		return err
	}
	return nil
}
