package sip

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"
)

func TestSendInfo(t *testing.T) {
	st := NewServiceTest(t, nil)

	type infoResult struct {
		res *sip.Response
		err error
	}

	directions := map[string]func(t *testing.T) (*sipUADialogTest, CallInterface){
		"inbound": func(t *testing.T) (*sipUADialogTest, CallInterface) {
			call, ic := st.CreateInboundCall(t)
			return call, ic
		},
		"outbound": func(t *testing.T) (*sipUADialogTest, CallInterface) {
			call, oc, _ := st.CreateOutboundCall(t)
			return call, oc
		},
	}

	// receiveInfo waits for the INFO on the remote side, checks it belongs to the
	// dialog and answers it with status.
	receiveInfo := func(t *testing.T, ctx context.Context, reqs <-chan *sipUARequest, call *sipUADialogTest, status int) *sip.Request {
		t.Helper()
		select {
		case <-ctx.Done():
			require.FailNow(t, "no INFO received")
			return nil
		case msg := <-reqs:
			require.NotNil(t, msg)
			require.Equal(t, sip.INFO, msg.req.Method)
			require.Equal(t, call.callID, msg.req.CallID().Value(), "INFO must stay in the call's dialog")
			require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""), "INFO must target the remote party's tag")
			require.Equal(t, sip.INFO, msg.req.CSeq().MethodName)
			resp := sip.NewResponseFromRequest(msg.req, status, sipStatus(sip.StatusCode(status)), nil)
			require.NoError(t, msg.tx.Respond(resp))
			return msg.req
		}
	}

	start := func(ctx context.Context, call CallInterface, contentType string, body []byte, headers map[string]string) <-chan infoResult {
		done := make(chan infoResult, 1)
		go func() {
			res, err := call.SendInfo(ctx, contentType, body, headers)
			done <- infoResult{res, err}
		}()
		return done
	}

	for direction, setup := range directions {
		t.Run(direction, func(t *testing.T) {
			t.Parallel()

			t.Run("json body and headers", func(t *testing.T) {
				t.Parallel()
				call, lkCall := setup(t)
				reqs := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()

				body := []byte(`{"final":true,"intent":"billing"}`)
				done := start(ctx, lkCall, "application/json", body, map[string]string{"Info-Package": "bot-update"})
				req := receiveInfo(t, ctx, reqs, call, 200)

				require.Equal(t, body, req.Body())
				require.Equal(t, "application/json", req.ContentType().Value())
				require.Equal(t, "bot-update", req.GetHeader("Info-Package").Value())

				res := <-done
				require.NoError(t, res.err)
				require.Equal(t, sip.StatusCode(200), res.res.StatusCode)
			})

			t.Run("cseq advances per request", func(t *testing.T) {
				t.Parallel()
				call, lkCall := setup(t)
				reqs := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()

				var seqs []uint32
				for i := 0; i < 3; i++ {
					done := start(ctx, lkCall, "application/dtmf-relay", []byte("Signal=5\r\nDuration=160\r\n"), nil)
					req := receiveInfo(t, ctx, reqs, call, 200)
					seqs = append(seqs, req.CSeq().SeqNo)
					require.NoError(t, (<-done).err)
				}
				require.Less(t, seqs[0], seqs[1])
				require.Less(t, seqs[1], seqs[2])
			})

			t.Run("non-2xx is reported, not an error", func(t *testing.T) {
				t.Parallel()
				call, lkCall := setup(t)
				reqs := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()

				done := start(ctx, lkCall, "application/json", []byte(`{}`), nil)
				receiveInfo(t, ctx, reqs, call, 469) // Bad Info Package, RFC 6086
				res := <-done
				require.NoError(t, res.err)
				require.Equal(t, sip.StatusCode(469), res.res.StatusCode)
			})

			t.Run("empty body has no content type", func(t *testing.T) {
				t.Parallel()
				call, lkCall := setup(t)
				reqs := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()

				done := start(ctx, lkCall, "", nil, nil)
				req := receiveInfo(t, ctx, reqs, call, 200)
				require.Empty(t, req.Body())
				require.Nil(t, req.ContentType())
				require.NoError(t, (<-done).err)
			})

			t.Run("rpc round trip", func(t *testing.T) {
				t.Parallel()
				call, lkCall := setup(t)
				reqs := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()

				var r sendInfoRequest
				require.NoError(t, json.Unmarshal([]byte(`{"content_type":"application/json","body":"{\"a\":1}"}`), &r))
				done := make(chan infoResult, 1)
				var out *sendInfoResponse
				go func() {
					var err error
					out, err = rpcSendInfo(ctx, lkCall, &r)
					done <- infoResult{err: err}
				}()
				req := receiveInfo(t, ctx, reqs, call, 200)
				require.Equal(t, `{"a":1}`, string(req.Body()))
				require.NoError(t, (<-done).err)
				data, err := json.Marshal(out)
				require.NoError(t, err)
				require.JSONEq(t, `{"statusCode":200,"reason":"OK"}`, string(data))
			})
		})
	}
}

type fakeInfoCall struct {
	called bool
}

func (f *fakeInfoCall) EndCall(ctx context.Context, headers map[string]string) error { return nil }

func (f *fakeInfoCall) SendInfo(ctx context.Context, contentType string, body []byte, headers map[string]string) (*sip.Response, error) {
	f.called = true
	return sip.NewResponse(200, "OK"), nil
}

func TestSendInfoRequestValidation(t *testing.T) {
	cases := []struct {
		name string
		req  string
		err  string
	}{
		{"body without content type", `{"body":"x"}`, "contentType is required"},
		{"body too large", `{"contentType":"text/plain","body":"` + strings.Repeat("a", MaxInfoBodySize+1) + `"}`, "limit is"},
		{"dialog header", `{"headers":{"Call-ID":"x"}}`, `"Call-ID" cannot be set`},
		{"cseq header any case", `{"headers":{"cseq":"1 INFO"}}`, `"cseq" cannot be set`},
		{"content type via headers", `{"body":"x","contentType":"text/plain","headers":{"Content-Type":"x"}}`, "cannot be set"},
		{"compact call-id", `{"headers":{"i":"other-call"}}`, `"i" cannot be set`},
		{"compact via", `{"headers":{"v":"SIP/2.0/UDP 10.0.0.9"}}`, `"v" cannot be set`},
		{"crlf in header value", `{"headers":{"X-A":"b\r\nCall-ID: evil"}}`, "invalid header"},
		{"crlf in header name", `{"headers":{"X-A\r\nCall-ID":"evil"}}`, "invalid header"},
		{"forbidden header", `{"headers":{"Refer-To":"sip:x@y"}}`, "invalid header"},
		{"crlf in content type", `{"body":"x","contentType":"text/plain\r\nX-Evil: 1"}`, "invalid contentType"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r sendInfoRequest
			require.NoError(t, json.Unmarshal([]byte(c.req), &r))
			f := &fakeInfoCall{}
			_, err := rpcSendInfo(t.Context(), f, &r)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.err)
			var perr psrpc.Error
			require.ErrorAs(t, err, &perr)
			require.Equal(t, psrpc.InvalidArgument, perr.Code())
			require.False(t, f.called, "invalid requests must not reach the call")
		})
	}

	t.Run("accepts both field spellings", func(t *testing.T) {
		for _, raw := range []string{
			`{"contentType":"application/json","body":"{}"}`,
			`{"content_type":"application/json","body":"{}"}`,
		} {
			var r sendInfoRequest
			require.NoError(t, json.Unmarshal([]byte(raw), &r))
			require.Equal(t, "application/json", r.ContentType)
			f := &fakeInfoCall{}
			out, err := rpcSendInfo(t.Context(), f, &r)
			require.NoError(t, err)
			require.True(t, f.called)
			require.Equal(t, 200, out.StatusCode)
		}
	})
}

func TestSendInfoNotEstablished(t *testing.T) {
	var c sipInbound
	_, err := c.SendInfo(t.Context(), "text/plain", []byte("x"), nil)
	var perr psrpc.Error
	require.ErrorAs(t, err, &perr)
	require.Equal(t, psrpc.FailedPrecondition, perr.Code())
}

func TestNewInfoRequestMatchesReferDialog(t *testing.T) {
	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "example.com"})
	invite.AppendHeader(&sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "10.0.0.1", Port: 5060, Params: sip.NewParams()})
	from := &sip.FromHeader{Address: sip.Uri{User: "alice", Host: "example.org"}, Params: sip.NewParams()}
	from.Params.Add("tag", "from-tag")
	invite.AppendHeader(from)
	to := &sip.ToHeader{Address: sip.Uri{User: "bob", Host: "example.com"}, Params: sip.NewParams()}
	invite.AppendHeader(to)
	callID := sip.CallIDHeader("call-1")
	invite.AppendHeader(&callID)
	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 7, MethodName: sip.INVITE})

	ok := sip.NewResponseFromRequest(invite, 200, "OK", nil)
	ok.To().Params.Add("tag", "to-tag")
	contact := &sip.ContactHeader{Address: sip.Uri{User: "lk", Host: "10.0.0.2"}}

	refer := NewReferRequest(invite, ok, contact, "tel:+15550000000", nil)
	info := NewInfoRequest(invite, ok, contact, "application/json", []byte(`{}`), map[string]string{"X-A": "b"})

	require.Equal(t, sip.INFO, info.Method)
	require.Equal(t, sip.INFO, info.CSeq().MethodName)
	require.Equal(t, refer.CSeq().SeqNo, info.CSeq().SeqNo)
	require.Equal(t, refer.CallID().Value(), info.CallID().Value())
	require.Equal(t, refer.From().Value(), info.From().Value())
	require.Equal(t, refer.To().Value(), info.To().Value())
	require.Equal(t, "application/json", info.ContentType().Value())
	require.Equal(t, "b", info.GetHeader("X-A").Value())
	require.Nil(t, info.GetHeader("Refer-To"))
	require.Equal(t, "tel:+15550000000", refer.GetHeader("Refer-To").Value())
}

type rpcCaptureRoom struct {
	RoomInterface
	handlers map[string]lksdk.RpcHandlerCtxFunc
}

func (r *rpcCaptureRoom) RegisterRpcCtxMethod(method string, handler lksdk.RpcHandlerCtxFunc) error {
	r.handlers[method] = handler
	return nil
}

func TestSendInfoRPCRegistered(t *testing.T) {
	room := &rpcCaptureRoom{handlers: make(map[string]lksdk.RpcHandlerCtxFunc)}
	call := &fakeInfoCall{}
	require.NoError(t, registerCallRPC(room, call))
	h, ok := room.handlers["lk.sip.SendInfo"]
	require.True(t, ok, "lk.sip.SendInfo must be registered with the call RPCs")
	require.Contains(t, room.handlers, "lk.sip.EndCall")

	out, err := h(t.Context(), []byte(`{"contentType":"application/json","body":"{\"final\":true}"}`))
	require.NoError(t, err)
	require.True(t, call.called)
	require.JSONEq(t, `{"statusCode":200,"reason":"OK"}`, string(out))

	_, err = h(t.Context(), []byte(`not json`))
	var perr psrpc.Error
	require.ErrorAs(t, err, &perr)
	require.Equal(t, psrpc.InvalidArgument, perr.Code())
}
