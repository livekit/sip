// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
)

func TestOutboundRouteHeaderWithRecordRoute(t *testing.T) {
	// Make sure the ACK doesn't carry over initial Route header.
	// Steps:
	// 1. Create a SIP participant with an initial Route header.
	// 2. Make sure the Route header is properly populates in INVITE.
	// 3. Fake a 200 response with Record Route headers.
	// 4. Make sure the ACK doesn't carry over initial Route header..

	// Plumbing
	initialRouteURI := sip.Uri{Host: "initial-header.com", UriParams: sip.HeaderParams{{"lr", ""}}}
	addedRouteURI := sip.Uri{Host: "added-header.com", UriParams: sip.HeaderParams{{"lr", ""}}}
	initialRouteHeader := sip.RouteHeader{Address: initialRouteURI}
	addedRouteHeader := sip.RouteHeader{Address: addedRouteURI}
	h := NewTestSIP(t, TestSIPConfig{})
	req := MinimalCreateSIPParticipantRequest()
	req.Headers = map[string]string{
		"Route": initialRouteHeader.Value(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // Allow test to continue
		_, err := h.Client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			// Only log error if context wasn't cancelled
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

	t.Log("Waiting for INVITE to be sent")
	tr := h.WaitTransaction(t, time.Second, req.SipCallId, "")

	t.Log("Received INVITE, validating")

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.NotNil(t, tr.transaction)
	require.Equal(t, sip.INVITE, tr.req.Method)
	routeHeaders := tr.req.GetHeaders("Route")
	require.Equal(t, 1, len(routeHeaders))
	require.Equal(t, initialRouteHeader.Value(), routeHeaders[0].Value())

	t.Log("INVITE okay, sending fake response")

	response := sip.NewSDPResponseFromRequest(tr.req, []byte(testMinimalSDP))
	require.NotNil(t, response, "NewSDPResponseFromRequest returned nil")
	response.RemoveHeader("Record-Route")
	rr1 := sip.RecordRouteHeader{Address: addedRouteURI}
	rr2 := sip.RecordRouteHeader{Address: initialRouteURI}
	response.AppendHeader(&rr1)
	response.AppendHeader(&rr2)
	tr.transaction.SendResponse(response)

	t.Log("Wait for ACK to be sent")
	ackReq := h.WaitRequest(t, time.Second, req.SipCallId, "")

	t.Log("Received ACK, validating")

	require.NotNil(t, ackReq)
	require.NotNil(t, ackReq.req)
	require.Equal(t, sip.ACK, ackReq.req.Method)
	require.Equal(t, tr.req.CSeq().SeqNo, ackReq.req.CSeq().SeqNo)
	require.Equal(t, tr.req.CallID(), ackReq.req.CallID())
	ackRouteHeaders := ackReq.req.GetHeaders("Route")
	require.Equal(t, 2, len(ackRouteHeaders)) // We expect this to fail prior to fixing our bug!
	require.Equal(t, initialRouteHeader.Value(), ackRouteHeaders[0].Value())
	require.Equal(t, addedRouteHeader.Value(), ackRouteHeaders[1].Value())

	cancel()
}

const (
	// Simulates sipgo caching a DNS-resolved transport target on the INVITE.
	testInviteCachedDestination = "10.0.0.1:5060"
	testInviteTargetHost        = "sip.example.com"
)

// waitOutboundINVITEAndACK drives CreateSIPParticipant until an INVITE is sent, fakes a 200 OK,
// and returns the captured ACK. mutate is called after the INVITE is received and before the
// 200 OK is delivered to the transaction.
func waitOutboundINVITEAndACK(
	t *testing.T,
	clientCfg TestSIPConfig,
	participantReq *rpc.InternalCreateSIPParticipantRequest,
	mutate func(tr *transactionRequest, resp *sip.Response),
) (*testSIPHarness, *transactionRequest, *sipRequest) {

	h := NewTestSIP(t, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_, err := h.Client.CreateSIPParticipant(ctx, participantReq)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
			t.Fail()
		}
	}()

	tr := h.WaitTransaction(t, time.Second, participantReq.SipCallId, "")
	require.Equal(t, sip.INVITE, tr.req.Method)

	resp := sip.NewSDPResponseFromRequest(tr.req, []byte(testMinimalSDP))
	require.NotNil(t, resp)
	mutate(tr, resp)
	require.NoError(t, tr.transaction.SendResponse(resp))

	ackReq := h.WaitRequest(t, time.Second, participantReq.SipCallId, "")
	require.Equal(t, sip.ACK, ackReq.req.Method)
	return h, tr, ackReq
}

// answerBYE waits for the call to send a BYE and responds 200 OK.
// If we don't explicitly answer the BYE, then a test might hang, as sipOutbound
// expects a response before closing the call.
func answerBYE(t *testing.T, h *testSIPHarness, timeout time.Duration, callID string) {
	t.Helper()

	byeTx := h.WaitTransaction(t, timeout, callID, "")
	require.Equal(t, sip.BYE, byeTx.req.Method)

	resp := sip.NewResponseFromRequest(byeTx.req, 200, "OK", nil)
	err := byeTx.transaction.SendResponse(resp)
	require.NoError(t, err)
}

func TestOutboundACKDestinationAfterInviteResponse(t *testing.T) {
	t.Run("changed contact flushes stale cached destination", func(t *testing.T) {
		// Without the fix, NewAckRequest copies the INVITE's cached DNS destination
		// (10.0.0.1:5060) even though the 200 OK Contact points elsewhere.
		const (
			contactHost = "10.0.0.99"
			contactPort = 5080
		)
		contactURI := sip.Uri{Host: contactHost, Port: contactPort}

		_, _, ackReq := waitOutboundINVITEAndACK(t, TestSIPConfig{}, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
			require.Equal(t, testInviteTargetHost, tr.req.Recipient.Host)
			tr.req.SetDestination(testInviteCachedDestination)
			resp.AppendHeader(&sip.ContactHeader{Address: contactURI})
		})
		require.NotNil(t, ackReq)

		require.Equal(t, contactHost, ackReq.req.Recipient.Host)
		require.Equal(t, contactPort, ackReq.req.Recipient.Port)
		require.Equal(t, fmt.Sprintf("%s:%d", contactHost, contactPort), ackReq.req.Destination())
		require.NotEqual(t, testInviteCachedDestination, ackReq.req.Destination())
	})

	t.Run("unchanged contact keeps cached destination", func(t *testing.T) {
		contactURI := sip.Uri{Host: testInviteTargetHost, Port: 5060}

		_, _, ackReq := waitOutboundINVITEAndACK(t, TestSIPConfig{}, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
			tr.req.SetDestination(testInviteCachedDestination)
			resp.AppendHeader(&sip.ContactHeader{Address: contactURI})
		})
		require.NotNil(t, ackReq)

		require.Equal(t, testInviteTargetHost, ackReq.req.Recipient.Host)
		require.Equal(t, testInviteCachedDestination, ackReq.req.Destination())
	})

	t.Run("record route rebuild flushes stale cached destination", func(t *testing.T) {
		// Route set changes must invalidate the cached destination even when Contact
		// matches the original INVITE target.
		proxyURI := sip.Uri{Host: "proxy.example.com", Port: 5060, UriParams: sip.HeaderParams{{"lr", ""}}}
		contactURI := sip.Uri{Host: testInviteTargetHost, Port: 5060}

		_, _, ackReq := waitOutboundINVITEAndACK(t, TestSIPConfig{}, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
			tr.req.SetDestination(testInviteCachedDestination)
			resp.AppendHeader(&sip.ContactHeader{Address: contactURI})
			resp.AppendHeader(&sip.RecordRouteHeader{Address: proxyURI})
		})
		require.NotNil(t, ackReq)

		require.Equal(t, testInviteTargetHost, ackReq.req.Recipient.Host)
		require.Equal(t, "proxy.example.com:5060", ackReq.req.Destination())
		require.NotEqual(t, testInviteCachedDestination, ackReq.req.Destination())
	})
}

func TestOutboundProviderLabel(t *testing.T) {
	joinMetric := "livekit_sip_dur_join_sec"
	labels := map[string]string{"dir": "out", "provider": "internal/xyz_carrier"}
	before := gatherHistogramCount(t, joinMetric, labels)

	req := MinimalCreateSIPParticipantRequest()
	req.ProviderInfo = &livekit.ProviderInfo{Name: "xyz_carrier", Type: livekit.ProviderType_PROVIDER_TYPE_INTERNAL}
	waitOutboundINVITEAndACK(t, TestSIPConfig{}, req, func(*transactionRequest, *sip.Response) {})

	// The join duration is recorded after the ACK is sent.
	require.Eventually(t, func() bool {
		return gatherHistogramCount(t, joinMetric, labels) == before+1
	}, time.Second, 10*time.Millisecond)
}

// sipResponse returns immediately on a cancelled context, sending a CANCEL.
func TestSIPResponseCancelReturnsImmediately(t *testing.T) {
	tx := &testSIPClientTransaction{
		log:       logger.NewTestLogger(t),
		responses: make(chan *sip.Response),
		cancels:   make(chan struct{}, 1),
		done:      make(chan struct{}),
		err:       make(chan error, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := sipResponse(ctx, tx, nil, nil)
	require.Error(t, err)
	require.Nil(t, res)
	require.Len(t, tx.cancels, 1, "CANCEL should be sent")
}

// watchCancelledInvite ACKs and BYEs a 2xx that races in after a CANCEL, and
// stays quiet otherwise.
func TestWatchCancelledInvite(t *testing.T) {
	newInvite := func() *sip.Request {
		req := sip.NewRequest(sip.INVITE, sip.Uri{User: "callee", Host: "sip.example.com"})
		from := &sip.FromHeader{Address: sip.Uri{User: "caller", Host: "lk"}, Params: sip.NewParams()}
		from.Params.Add("tag", "caller-tag")
		req.AppendHeader(from)
		req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "callee", Host: "sip.example.com"}, Params: sip.NewParams()})
		cid := sip.CallIDHeader("call-123")
		req.AppendHeader(&cid)
		req.AppendHeader(&sip.CSeqHeader{MethodName: sip.INVITE, SeqNo: 1})
		via := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "lk", Port: 5060, Params: sip.NewParams()}
		via.Params.Add("branch", "z9hG4bK.test")
		req.AppendHeader(via)
		return req
	}
	ok := sip.NewSDPResponseFromRequest(newInvite(), []byte(testMinimalSDP))
	ackBye := []sip.RequestMethod{sip.ACK, sip.BYE}

	for _, tt := range []struct {
		name  string
		resps []*sip.Response
		want  []sip.RequestMethod
	}{
		{"2xx answered", []*sip.Response{ok}, ackBye},
		{"provisional then 2xx", []*sip.Response{sip.NewResponse(sip.StatusRinging, "Ringing"), ok}, ackBye},
		{"non-2xx final", []*sip.Response{sip.NewResponse(sip.StatusRequestTerminated, "Terminated")}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx := &testSIPClientTransaction{log: logger.NewTestLogger(t), responses: make(chan *sip.Response, len(tt.resps)), done: make(chan struct{})}
			for _, r := range tt.resps {
				tx.responses <- r
			}
			cli := newTestSIPClient(logger.NewTestLogger(t), nil)
			watchCancelledInvite(logger.NewTestLogger(t), cli, nil, newInvite(), tx)
			require.Equal(t, tt.want, cli.requestMethods("call-123"))
		})
	}

	t.Run("no answer within grace", func(t *testing.T) {
		defer func(d time.Duration) { cancelResponseGrace = d }(cancelResponseGrace)
		cancelResponseGrace = 10 * time.Millisecond
		tx := &testSIPClientTransaction{log: logger.NewTestLogger(t), responses: make(chan *sip.Response), done: make(chan struct{})}
		cli := newTestSIPClient(logger.NewTestLogger(t), nil)
		watchCancelledInvite(logger.NewTestLogger(t), cli, nil, newInvite(), tx)
		require.Empty(t, cli.requestMethods("call-123"))
	})
}

func TestOutboundMaxCallDuration(t *testing.T) {
	const maxCallDuration = time.Second
	const waitSlack = time.Second

	type testCase struct {
		name              string
		waitUntilAnswered bool
	}
	type sessionEnd struct {
		reason string
		info   *livekit.SIPCallInfo
	}

	testCases := []testCase{
		{
			name:              "dial_async",
			waitUntilAnswered: false,
		},
		{
			name:              "dial_sync",
			waitUntilAnswered: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ended := make(chan sessionEnd, 1)

			clientCfg := TestSIPConfig{Handler: &TestHandler{
				OnSessionEndFunc: func(_ context.Context, _ *CallIdentifier, state *CallState, reason string) {
					ended <- sessionEnd{reason: reason, info: state.Info()}
				},
			}}

			req := MinimalCreateSIPParticipantRequest()
			req.WaitUntilAnswered = testCase.waitUntilAnswered
			req.MaxCallDuration = durationpb.New(maxCallDuration)
			h, _, _ := waitOutboundINVITEAndACK(t, clientCfg, req,
				func(tr *transactionRequest, resp *sip.Response) {})
			require.NotNil(t, h)

			answerBYE(t, h, maxCallDuration+waitSlack, req.SipCallId)

			select {
			case end := <-ended:
				require.Equal(t, "hangup", end.reason)
				require.Equal(t, livekit.DisconnectReason_CLIENT_INITIATED, end.info.DisconnectReason)
			case <-time.After(maxCallDuration + waitSlack):
				require.Fail(t, "expected call to have already ended")
			}
		})
	}

}

func TestOutboundHangupDrainsMediaBeforeBYE(t *testing.T) {
	// A locally-initiated hangup (agent end-call, EndCall RPC, participant
	// removed) must keep feeding media to the SIP peer for the configured
	// drain window before sending BYE, so the in-flight tail of the last
	// utterance reaches the callee (issue #4737). Remote BYE and error paths
	// must not be delayed.
	const drain = 500 * time.Millisecond

	conf := &config.Config{
		NodeID:            "test-node",
		SIPPort:           5060,
		SIPPortListen:     5060,
		RTPPort:           rtcconfig.PortRange{Start: 20000, End: 30000},
		MaxCpuUtilization: 0.99,
		WsUrl:             "ws://localhost:7880",
		ApiKey:            "test-api-key",
		ApiSecret:         "test-api-secret-extend-to-32-bytes-minimum",
		HangupDrainTime:   drain,
	}
	client := NewOutboundTestClient(t, TestClientConfig{Config: conf})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req := MinimalCreateSIPParticipantRequest()
	go func() {
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
			t.Fail()
		}
	}()

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "expected test SIP client to be created")
		return
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "expected INVITE transaction")
		return
	}
	require.Equal(t, sip.INVITE, tr.req.Method)

	// Fake 200 OK + ACK so the call is established.
	resp := sip.NewSDPResponseFromRequest(tr.req, []byte(testMinimalSDP))
	require.NoError(t, tr.transaction.SendResponse(resp))

	var ackReq *sipRequest
	select {
	case ackReq = <-sipClient.requests:
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "expected ACK request")
		return
	}
	require.Equal(t, sip.ACK, ackReq.req.Method)

	// Local hangup path: the BYE must arrive only after the drain window.
	call := client.getActiveCall(LocalTag(req.SipCallId))
	require.NotNil(t, call)

	start := time.Now()
	endCallDone := make(chan error, 1)
	go func() {
		endCallDone <- call.EndCall(ctx, nil)
	}()

	select {
	case byeTx := <-sipClient.transactions:
		require.Equal(t, sip.BYE, byeTx.req.Method)
		require.GreaterOrEqual(t, time.Since(start), drain, "BYE sent before the media drain window elapsed")
		resp := sip.NewResponseFromRequest(byeTx.req, 200, "OK", nil)
		require.NoError(t, byeTx.transaction.SendResponse(resp))
	case <-time.After(drain + 3*time.Second):
		require.Fail(t, "expected BYE after the drain window")
	}

	select {
	case err := <-endCallDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.Fail(t, "EndCall did not return after BYE was answered")
	}
}

func TestBuildOutboundHeaders(t *testing.T) {
	newReq := func() *rpc.InternalCreateSIPParticipantRequest {
		return &rpc.InternalCreateSIPParticipantRequest{}
	}
	check := func(t testing.TB, req *rpc.InternalCreateSIPParticipantRequest, defaultHost string, expURI, expFrom, expTo, expErr string) {
		if defaultHost == "" {
			defaultHost = "sip.default.test"
		}
		uri, from, to, err := buildOutboundHeaders(req, defaultHost)
		if expErr != "" {
			require.Error(t, err)
			require.Equal(t, expErr, err.Error())
			return
		}
		require.NoError(t, err)
		require.Equal(t, expURI, uri.String())
		require.Equal(t, expFrom, from.String())
		require.Equal(t, expTo, to.String())
	}
	expectErr := func(t testing.TB, req *rpc.InternalCreateSIPParticipantRequest, expErr string) {
		check(t, req, "", "", "", "", expErr)
	}
	expect := func(t testing.TB, req *rpc.InternalCreateSIPParticipantRequest, expURI, expFrom, expTo string) {
		check(t, req, "", expURI, expFrom, expTo, "")
	}
	uriVals := func(u *livekit.SIPUri) *livekit.SIPRequestDest {
		return &livekit.SIPRequestDest{
			Uri: &livekit.SIPRequestDest_Values{
				Values: u,
			},
		}
	}
	uriRaw := func(raw string) *livekit.SIPRequestDest {
		return &livekit.SIPRequestDest{
			Uri: &livekit.SIPRequestDest_Raw{
				Raw: raw,
			},
		}
	}
	namedVals := func(name string, u *livekit.SIPUri) *livekit.SIPNamedDest {
		return &livekit.SIPNamedDest{
			DisplayName: name,
			Uri: &livekit.SIPNamedDest_Values{
				Values: u,
			},
		}
	}
	namedRaw := func(name string, raw string) *livekit.SIPNamedDest {
		return &livekit.SIPNamedDest{
			DisplayName: name,
			Uri: &livekit.SIPNamedDest_Raw{
				Raw: raw,
			},
		}
	}
	t.Run("empty", func(t *testing.T) {
		req := newReq()
		expectErr(t, req, "invalid request URI: number must be set")
	})
	t.Run("legacy", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		expect(t, req,
			`sip:222@sip.test.com`,
			`From: "111" <sip:111@sip.default.test>`,
			`To: <sip:222@sip.test.com>`,
		)
	})
	t.Run("legacy name", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		req.DisplayName = new("LK")
		expect(t, req,
			`sip:222@sip.test.com`,
			`From: "LK" <sip:111@sip.default.test>`,
			`To: <sip:222@sip.test.com>`,
		)
	})
	t.Run("legacy and uri", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		req.SipRequestUri = uriVals(&livekit.SIPUri{
			User: "333",
			Host: "sip.another.com",
		})
		expect(t, req,
			`sip:333@sip.another.com`,
			`From: "111" <sip:111@sip.default.test>`,
			`To: <sip:222@sip.test.com>`,
		)
	})
	t.Run("legacy and uri raw", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		req.SipRequestUri = uriRaw(`sip:333@sip.another.com`)
		expect(t, req,
			`sip:333@sip.another.com`,
			`From: "111" <sip:111@sip.default.test>`,
			`To: <sip:222@sip.test.com>`,
		)
	})
	t.Run("legacy and From", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		req.SipFromHeader = namedVals("LK", &livekit.SIPUri{
			User: "333",
			Host: "sip.another.com",
		})
		expect(t, req,
			`sip:222@sip.test.com`,
			`From: "LK" <sip:333@sip.another.com>`,
			`To: <sip:222@sip.test.com>`,
		)
	})
	t.Run("legacy and To both", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		req.SipToHeader = namedVals("User", &livekit.SIPUri{
			User: "333",
			Host: "sip.another.com",
		})
		expectErr(t, req, "invalid To header: cannot use both CallTo and SipToHeader")
	})
	t.Run("legacy and To addr", func(t *testing.T) {
		// Allow both Address and To. Address could be used as a network-level destination.
		req := newReq()
		req.Address = "1.2.3.4"
		req.Number = "111"
		req.SipToHeader = namedVals("User", &livekit.SIPUri{
			User: "333",
			Host: "sip.another.com",
		})
		// However, CallTo is needed for request URI, but it cannot be set because it conflicts with To header.
		expectErr(t, req, "invalid request URI: number must be set")
	})
	t.Run("all new", func(t *testing.T) {
		req := newReq()
		req.SipRequestUri = uriVals(&livekit.SIPUri{
			User: "222",
			Host: "sip.test.com",
		})
		req.SipFromHeader = namedVals("LK", &livekit.SIPUri{
			User: "111",
			Host: "example.com", // OSS can override the hostname
		})
		req.SipToHeader = namedVals("User", &livekit.SIPUri{
			User: "333",
			Host: "sip.another.com",
		})
		expect(t, req,
			`sip:222@sip.test.com`,
			`From: "LK" <sip:111@example.com>`, // OSS can override the hostname
			`To: "User" <sip:333@sip.another.com>`,
		)
	})
	t.Run("all raw brackets", func(t *testing.T) {
		req := newReq()
		req.SipRequestUri = uriRaw(`sip:222@sip.test.com`)
		req.SipFromHeader = namedRaw("LK", `<sip:111@sip.livekit.test>`)
		req.SipToHeader = namedRaw("User", `<sip:333@sip.another.com>`)
		expect(t, req,
			`sip:222@sip.test.com`,
			`From: "LK" <sip:111@sip.livekit.test>`,
			`To: "User" <sip:333@sip.another.com>`,
		)
	})
	t.Run("all raw no brackets", func(t *testing.T) {
		req := newReq()
		req.SipRequestUri = uriRaw(`sip:222@sip.test.com`)
		req.SipFromHeader = namedRaw("LK", `sip:111@sip.livekit.test`)
		req.SipToHeader = namedRaw("User", `sip:333@sip.another.com`)
		expect(t, req,
			`sip:222@sip.test.com`,
			`From: "LK" <sip:111@sip.livekit.test>`,
			`To: "User" <sip:333@sip.another.com>`,
		)
	})
	t.Run("raw param override", func(t *testing.T) {
		req := newReq()
		req.SipRequestUri = uriRaw(`sip:222@sip.test.com`)
		req.SipFromHeader = namedRaw("LK", `<sip:111@sip.livekit.test>;tag=AAA`)
		req.SipToHeader = namedRaw("User", `<sip:333@sip.another.com>;tag=BBB`)
		expectErr(t, req, "invalid To header: invalid request URI")
	})
	t.Run("all raw transport", func(t *testing.T) {
		req := newReq()
		req.SipRequestUri = uriRaw(`sip:222@sip.test.com;transport=tcp`)
		req.SipFromHeader = namedRaw("LK", `sip:111@sip.livekit.test;transport=tcp`)
		req.SipToHeader = namedRaw("User", `sip:333@sip.another.com;transport=tcp`)
		expect(t, req,
			`sip:222@sip.test.com;transport=tcp`,
			`From: "LK" <sip:111@sip.livekit.test;transport=tcp>`,
			`To: "User" <sip:333@sip.another.com;transport=tcp>`,
		)
	})
	t.Run("all raw req transport", func(t *testing.T) {
		req := newReq()
		req.Transport = livekit.SIPTransport_SIP_TRANSPORT_TLS
		req.SipRequestUri = uriRaw(`sip:222@sip.test.com;transport=tcp`)
		req.SipFromHeader = namedRaw("LK", `sip:111@example.com;transport=tcp`)
		req.SipToHeader = namedRaw("User", `sip:333@sip.another.com;transport=tcp`)
		expect(t, req,
			`sip:222@sip.test.com;transport=tls`,
			`From: "LK" <sip:111@example.com;transport=tls>`,
			`To: "User" <sip:333@sip.another.com;transport=tls>`,
		)
	})
	t.Run("all new req transport", func(t *testing.T) {
		req := newReq()
		req.Transport = livekit.SIPTransport_SIP_TRANSPORT_TLS
		req.SipRequestUri = uriVals(&livekit.SIPUri{
			User: "222",
			Host: "sip.test.com",
		})
		req.SipFromHeader = namedVals("LK", &livekit.SIPUri{
			User: "111",
			Host: "example.com",
		})
		req.SipToHeader = namedVals("User", &livekit.SIPUri{
			User: "333",
			Host: "sip.another.com",
		})
		expect(t, req,
			`sip:222@sip.test.com;transport=tls`,
			`From: "LK" <sip:111@example.com;transport=tls>`,
			`To: "User" <sip:333@sip.another.com;transport=tls>`,
		)
	})
	t.Run("to user override", func(t *testing.T) {
		req := newReq()
		req.Address = "sip.test.com"
		req.Number = "111"
		req.CallTo = "222"
		req.Transport = livekit.SIPTransport_SIP_TRANSPORT_TLS
		req.ToUserOverride = "333"
		expect(t, req,
			`sip:222@sip.test.com;transport=tls`,
			`From: "111" <sip:111@sip.default.test;transport=tls>`,
			`To: <sip:333@sip.test.com;transport=tls>`,
		)
	})
	t.Run("to user override with request uri override", func(t *testing.T) {
		req := newReq()
		req.Number = "111"
		req.CallTo = "222"
		req.SipRequestUri = uriVals(&livekit.SIPUri{
			User: "999",
			Host: "test12.test34.com",
		})
		req.Address = "sip.trunk.com"
		req.ToUserOverride = "333"
		// The To is still trunk-derived; only its user is replaced.
		expect(t, req,
			`sip:999@test12.test34.com`,
			`From: "111" <sip:111@sip.default.test>`,
			`To: <sip:333@sip.trunk.com>`,
		)
	})
	t.Run("to user override rejects uri and injection", func(t *testing.T) {
		for _, bad := range []string{
			"333@sip.other.com",          // full user@host
			"333;tag=x",                  // param terminator
			"<333>",                      // angle brackets
			"333\r\nEvil-Hdr: y",         // header injection
			"3 33",                       // space
			"333?Route=sip:evil.example", // '?' opens URI headers
			"333:secret",                 // ':' makes the rest a password
			"333&x=1",
			"333/foo",
			"333,x",
			`333"x`,
		} {
			req := newReq()
			req.Address = "sip.test.com"
			req.Number = "111"
			req.CallTo = "222"
			req.ToUserOverride = bad
			expectErr(t, req, "invalid To header: to user override should be a phone number or SIP user, not a full SIP URI")
		}
	})
}

// startInvite places an outbound call and returns the harness plus the INVITE
// the client sent. Resolution itself is sipgo's contract and is tested there.
// These tests pin the wiring: when we ask sipgo to resolve, and what we do with
// the answer.
func startInvite(t *testing.T, cfg TestSIPConfig, req *rpc.InternalCreateSIPParticipantRequest) (*testSIPHarness, *transactionRequest) {
	t.Helper()
	h := NewTestSIP(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if _, err := h.Client.CreateSIPParticipant(ctx, req); err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant: %v", err)
		}
	}()

	tr := h.WaitTransaction(t, 5*time.Second, req.SipCallId, "")
	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.Equal(t, sip.INVITE, tr.req.Method)
	return h, tr
}

const testResolvedAddr = "198.51.100.11:5006"

func resolveToTestAddr(_, _ string, _ int, _ string) (netip.AddrPort, error) {
	return netip.MustParseAddrPort(testResolvedAddr), nil
}

func resolveFails(_, _ string, _ int, _ string) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("no such host")
}

var testProxyRoute = sip.RouteHeader{Address: sip.Uri{Host: "proxy.example.com", UriParams: sip.HeaderParams{{"lr", ""}}}}

func TestOutboundPreferSRV(t *testing.T) {
	cases := []struct {
		name      string
		address   string
		preferSRV bool
		route     bool
		resolve   func(network, host string, port int, sipScheme string) (netip.AddrPort, error)
		// wantResolve is nil when the call must not resolve at all.
		wantResolve *resolveCall
		wantDest    string
	}{
		{
			name:        "flag on uses the resolved address",
			address:     "trunk.example.com",
			preferSRV:   true,
			resolve:     resolveToTestAddr,
			wantResolve: &resolveCall{network: "udp", host: "trunk.example.com", port: 0, sipScheme: "sip"},
			wantDest:    testResolvedAddr,
		},
		{
			// sipgo suppresses SRV when a port is set, so the port must reach it.
			name:        "flag on passes the configured port",
			address:     "trunk.example.com:5007",
			preferSRV:   true,
			resolve:     resolveToTestAddr,
			wantResolve: &resolveCall{network: "udp", host: "trunk.example.com", port: 5007, sipScheme: "sip"},
			wantDest:    testResolvedAddr,
		},
		{
			name:        "resolution failure leaves the hostname",
			address:     "trunk.example.com",
			preferSRV:   true,
			resolve:     resolveFails,
			wantResolve: &resolveCall{network: "udp", host: "trunk.example.com", port: 0, sipScheme: "sip"},
			wantDest:    "trunk.example.com:5060",
		},
		{
			name:     "flag off does not resolve",
			address:  "trunk.example.com",
			resolve:  resolveToTestAddr,
			wantDest: "trunk.example.com:5060",
		},
		{
			// The request goes to the proxy, so resolving the request URI would
			// pin the wrong address.
			name:      "route header skips resolution",
			address:   "trunk.example.com",
			preferSRV: true,
			route:     true,
			resolve:   resolveToTestAddr,
			wantDest:  "proxy.example.com:5060",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := MinimalCreateSIPParticipantRequest()
			req.Address = tc.address
			if tc.preferSRV {
				req.FeatureFlags = map[string]string{outboundPreferSRVFeatureFlag: "true"}
			}
			if tc.route {
				req.Headers = map[string]string{"Route": testProxyRoute.Value()}
			}

			h, tr := startInvite(t, TestSIPConfig{ResolveAddr: tc.resolve}, req)

			calls := h.ResolveCalls()
			if tc.wantResolve == nil {
				require.Empty(t, calls)
			} else {
				require.Equal(t, []resolveCall{*tc.wantResolve}, calls)
			}
			require.Equal(t, tc.wantDest, tr.req.Destination())
			// The request URI keeps the configured hostname, only the address changes.
			require.Equal(t, "trunk.example.com", tr.req.Recipient.Host)
		})
	}
}

func uriWithParams(kv ...string) *sip.Uri {
	u := &sip.Uri{Scheme: "sip", Host: "h.example.com", UriParams: sip.NewParams()}
	for i := 0; i < len(kv); i += 2 {
		u.UriParams.Add(kv[i], kv[i+1])
	}
	return u
}

func TestURITransport(t *testing.T) {
	cases := []struct {
		name string
		uri  *sip.Uri
		want string
	}{
		{"defaults to udp", &sip.Uri{Scheme: "sip", Host: "h.example.com"}, "udp"},
		{"sips means tls", &sip.Uri{Scheme: "sips", Host: "h.example.com"}, "tls"},
		{"sips is matched case insensitively", &sip.Uri{Scheme: "SIPS", Host: "h.example.com"}, "tls"},
		{"transport param wins", uriWithParams("transport", "tcp"), "tcp"},
		{"transport param is lowercased", uriWithParams("transport", "TCP"), "tcp"},
		{"transport param beats sips", &sip.Uri{Scheme: "sips", Host: "h.example.com", UriParams: sip.HeaderParams{{"transport", "tcp"}}}, "tcp"},
		{"empty transport param falls through", uriWithParams("transport", ""), "udp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, uriTransport(tc.uri))
		})
	}
}
