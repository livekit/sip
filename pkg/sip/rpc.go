package sip

import (
	"encoding/json"
	"strings"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

func registerRPCMethodJSON[Req any, Resp any](r RoomInterface, name string, fnc func(r Req) (Resp, error)) error {
	return r.RegisterRPC(name, func(data lksdk.RpcInvocationData) (string, error) {
		var req Req
		if len(data.Payload) != 0 {
			if err := json.Unmarshal([]byte(data.Payload), &req); err != nil {
				return "", err
			}
		}
		resp, err := fnc(req)
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(&resp)
		return string(out), err
	})
}

type getHeadersReq struct {
	Include []string `json:"include,omitempty"` // list of headers to include
	Exclude []string `json:"exclude,omitempty"` // list of headers to exclude
}

type getHeadersResp struct {
	Headers map[string]string `json:"headers"`
}

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

func rpcGetHeaders(cc Signaling, r getHeadersReq) (getHeadersResp, error) {
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
	return getHeadersResp{
		Headers: out,
	}, nil
}

func registerSignalingRPC(r RoomInterface, cc Signaling) error {
	err := registerRPCMethodJSON(r, "lk.sip.GetRemoteHeaders", func(r getHeadersReq) (getHeadersResp, error) {
		return rpcGetHeaders(cc, r)
	})
	if err != nil {
		return err
	}
	return nil
}
