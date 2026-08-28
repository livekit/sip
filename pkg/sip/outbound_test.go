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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
)

// recordingSIPClient is a SIPClient that records the requests written to it.
type recordingSIPClient struct {
	reqs []*sip.Request
}

func (c *recordingSIPClient) TransactionRequest(req *sip.Request, _ ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	return nil, nil
}

func (c *recordingSIPClient) WriteRequest(req *sip.Request, _ ...sipgo.ClientRequestOption) error {
	c.reqs = append(c.reqs, req)
	return nil
}

func (c *recordingSIPClient) ResolveTargets(_ context.Context, _, _ string, _ int, _ string) ([]netip.AddrPort, error) {
	return nil, errors.New("not resolved in this test")
}

func (c *recordingSIPClient) Close() error { return nil }

func (c *recordingSIPClient) methods() []sip.RequestMethod {
	var m []sip.RequestMethod
	for _, r := range c.reqs {
		m = append(m, r.Method)
	}
	return m
}

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
	client := NewOutboundTestClient(t, TestClientConfig{})
	req := MinimalCreateSIPParticipantRequest()
	req.Headers = map[string]string{
		"Route": initialRouteHeader.Value(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // Allow test to continue
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			// Only log error if context wasn't cancelled
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

	t.Log("Waiting for INVITE to be sent")

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(100 * time.Millisecond):
		cancel()
		require.Fail(t, "expected client to be created")
		return
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		cancel()
		require.Fail(t, "expected transaction request to be created")
		return
	}

	fmt.Println("Received INVITE, validating")

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.NotNil(t, tr.transaction)
	require.Equal(t, sip.INVITE, tr.req.Method)
	routeHeaders := tr.req.GetHeaders("Route")
	require.Equal(t, 1, len(routeHeaders))
	require.Equal(t, initialRouteHeader.Value(), routeHeaders[0].Value())

	t.Log("INVITE okay, sending fake response")

	minimalSDP := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	response := sip.NewSDPResponseFromRequest(tr.req, minimalSDP)
	require.NotNil(t, response, "NewSDPResponseFromRequest returned nil")
	response.RemoveHeader("Record-Route")
	rr1 := sip.RecordRouteHeader{Address: addedRouteURI}
	rr2 := sip.RecordRouteHeader{Address: initialRouteURI}
	response.AppendHeader(&rr1)
	response.AppendHeader(&rr2)
	tr.transaction.SendResponse(response)

	t.Log("Wait for ACK to be sent")

	// Make sure ACK is okay
	var ackReq *sipRequest
	select {
	case ackReq = <-sipClient.requests:
		// All good
	case <-time.After(100 * time.Millisecond):
		cancel()
		require.Fail(t, "expected ACK request to be created")
		return
	}

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
	testMinimalSDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	// Simulates sipgo caching a DNS-resolved transport target on the INVITE.
	testInviteCachedDestination = "10.0.0.1:5060"
	testInviteTargetHost        = "sip.example.com"
)

// waitOutboundINVITEAndACK drives CreateSIPParticipant until an INVITE is sent, fakes a 200 OK,
// and returns the captured ACK. mutate is called after the INVITE is received and before the
// 200 OK is delivered to the transaction.
func waitOutboundINVITEAndACK(
	t *testing.T,
	clientCfg TestClientConfig,
	participantReq *rpc.InternalCreateSIPParticipantRequest,
	mutate func(tr *transactionRequest, resp *sip.Response),
) (*testSIPClient, *transactionRequest, *sipRequest) {

	client := NewOutboundTestClient(t, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_, err := client.CreateSIPParticipant(ctx, participantReq)
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
		return nil, nil, nil
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "expected INVITE transaction")
		return sipClient, nil, nil
	}

	require.Equal(t, sip.INVITE, tr.req.Method)

	resp := sip.NewSDPResponseFromRequest(tr.req, []byte(testMinimalSDP))
	require.NotNil(t, resp)
	mutate(tr, resp)
	require.NoError(t, tr.transaction.SendResponse(resp))

	var ackReq *sipRequest
	select {
	case ackReq = <-sipClient.requests:
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "expected ACK request")
		return sipClient, tr, nil
	}

	require.Equal(t, sip.ACK, ackReq.req.Method)
	return sipClient, tr, ackReq
}

// answerBYE waits for the call to send a BYE and responds 200 OK.
// If we don't explicitly answer the BYE, then a test might hang, as sipOutbound
// expects a response before closing the call.
func answerBYE(t *testing.T, sipClient *testSIPClient, timeout time.Duration) {
	t.Helper()

	var byeTx *transactionRequest
	select {
	case byeTx = <-sipClient.transactions:
	case <-time.After(timeout):
		require.Fail(t, "expected BYE transaction")
		return
	}
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

		_, _, ackReq := waitOutboundINVITEAndACK(t, TestClientConfig{}, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
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

		_, _, ackReq := waitOutboundINVITEAndACK(t, TestClientConfig{}, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
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

		_, _, ackReq := waitOutboundINVITEAndACK(t, TestClientConfig{}, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
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

// sipResponse returns immediately on a cancelled context, sending a CANCEL.
func TestSIPResponseCancelReturnsImmediately(t *testing.T) {
	tx := &testSIPClientTransaction{
		log:       logger.GetLogger(),
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
			tx := &testSIPClientTransaction{log: logger.GetLogger(), responses: make(chan *sip.Response, len(tt.resps)), done: make(chan struct{})}
			for _, r := range tt.resps {
				tx.responses <- r
			}
			cli := &recordingSIPClient{}
			watchCancelledInvite(logger.GetLogger(), cli, nil, newInvite(), tx)
			require.Equal(t, tt.want, cli.methods())
		})
	}

	t.Run("no answer within grace", func(t *testing.T) {
		defer func(d time.Duration) { cancelResponseGrace = d }(cancelResponseGrace)
		cancelResponseGrace = 10 * time.Millisecond
		tx := &testSIPClientTransaction{log: logger.GetLogger(), responses: make(chan *sip.Response), done: make(chan struct{})}
		cli := &recordingSIPClient{}
		watchCancelledInvite(logger.GetLogger(), cli, nil, newInvite(), tx)
		require.Empty(t, cli.methods())
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

			clientCfg := TestClientConfig{Handler: &TestHandler{
				OnSessionEndFunc: func(_ context.Context, _ *CallIdentifier, state *CallState, reason string) {
					ended <- sessionEnd{reason: reason, info: state.Info()}
				},
			}}

			req := MinimalCreateSIPParticipantRequest()
			req.WaitUntilAnswered = testCase.waitUntilAnswered
			req.MaxCallDuration = durationpb.New(maxCallDuration)
			sipClient, _, _ := waitOutboundINVITEAndACK(t, clientCfg, req,
				func(tr *transactionRequest, resp *sip.Response) {})
			require.NotNil(t, sipClient)

			answerBYE(t, sipClient, maxCallDuration+waitSlack)

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

// fakeDNS is a minimal UDP DNS server answering A and SRV queries from a table,
// so outbound resolution runs through sipgo for real in tests.
type fakeDNS struct {
	conn *net.UDPConn
	a    map[string][]string
	srv  map[string][]fakeSRV

	mu   sync.Mutex
	seen map[string]int

	done sync.WaitGroup
}

type fakeSRV struct {
	target string
	port   uint16
}

const (
	dnsTypeA   = 1
	dnsTypeSRV = 33
)

func newFakeDNS(t *testing.T, a map[string][]string, srv map[string][]fakeSRV) *fakeDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	d := &fakeDNS{conn: conn, a: a, srv: srv, seen: map[string]int{}}
	d.done.Add(1)
	go d.serve()
	// Closing the socket makes the read in serve fail, which ends the goroutine.
	// Wait for it so it cannot outlive the test.
	t.Cleanup(func() {
		_ = conn.Close()
		d.done.Wait()
	})
	return d
}

func (d *fakeDNS) resolver() *net.Resolver {
	addr := d.conn.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", addr)
		},
	}
}

func (d *fakeDNS) queries(name string, qtype uint16) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return d.seen[strings.ToLower(name)+"/"+string(rune(qtype))]
}

func (d *fakeDNS) serve() {
	defer d.done.Done()
	buf := make([]byte, 512)
	for {
		n, from, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if resp := d.respond(buf[:n]); resp != nil {
			_, _ = d.conn.WriteToUDP(resp, from)
		}
	}
}

func (d *fakeDNS) respond(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	name, off, ok := dnsReadName(q, 12)
	if !ok || off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	d.mu.Lock()
	d.seen[strings.ToLower(name)+"/"+string(rune(qtype))]++
	d.mu.Unlock()

	var answers []byte
	var count int
	switch qtype {
	case dnsTypeA:
		for _, ip := range d.a[strings.TrimSuffix(name, ".")] {
			answers = append(answers, dnsEncodeName(name)...)
			answers = append(answers, dnsRRHeader(dnsTypeA, 4)...)
			answers = append(answers, net.ParseIP(ip).To4()...)
			count++
		}
	case dnsTypeSRV:
		for _, rec := range d.srv[strings.TrimSuffix(name, ".")] {
			target := dnsEncodeName(rec.target)
			rdata := make([]byte, 6, 6+len(target))
			binary.BigEndian.PutUint16(rdata[0:], 5)
			binary.BigEndian.PutUint16(rdata[2:], 50)
			binary.BigEndian.PutUint16(rdata[4:], rec.port)
			rdata = append(rdata, target...)
			answers = append(answers, dnsEncodeName(name)...)
			answers = append(answers, dnsRRHeader(dnsTypeSRV, uint16(len(rdata)))...)
			answers = append(answers, rdata...)
			count++
		}
	}

	resp := make([]byte, 12)
	copy(resp, q[:2])
	binary.BigEndian.PutUint16(resp[2:], 0x8180)
	binary.BigEndian.PutUint16(resp[4:], 1)
	binary.BigEndian.PutUint16(resp[6:], uint16(count))
	if count == 0 {
		binary.BigEndian.PutUint16(resp[2:], 0x8183)
	}
	resp = append(resp, q[12:off+4]...)
	return append(resp, answers...)
}

func dnsRRHeader(qtype, rdlen uint16) []byte {
	b := make([]byte, 10)
	binary.BigEndian.PutUint16(b[0:], qtype)
	binary.BigEndian.PutUint16(b[2:], 1)
	binary.BigEndian.PutUint32(b[4:], 60)
	binary.BigEndian.PutUint16(b[8:], rdlen)
	return b
}

func dnsEncodeName(name string) []byte {
	var b []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

func dnsReadName(msg []byte, off int) (string, int, bool) {
	var sb strings.Builder
	for off < len(msg) {
		n := int(msg[off])
		off++
		if n == 0 {
			return sb.String(), off, true
		}
		if n > 63 || off+n > len(msg) {
			return "", 0, false
		}
		sb.Write(msg[off : off+n])
		sb.WriteByte('.')
		off += n
	}
	return "", 0, false
}

const (
	testSBC1IP = "198.51.100.11"
	testSBC2IP = "198.51.100.12"
	testSBC1   = "sbc1.example.net"
	testSBC2   = "sbc2.example.net"
)

// twoSBCDNS reproduces a carrier layout where two servers sit on different ports
// behind one hostname whose A record lists both of their addresses.
func twoSBCDNS(t *testing.T) *fakeDNS {
	return newFakeDNS(t,
		map[string][]string{
			testSBC1:             {testSBC1IP},
			testSBC2:             {testSBC2IP},
			testInviteTargetHost: {testSBC1IP, testSBC2IP},
		},
		map[string][]fakeSRV{
			"_sip._udp." + testInviteTargetHost: {
				{target: testSBC1, port: 5006},
				{target: testSBC2, port: 5008},
			},
		},
	)
}

func startOutboundCall(t *testing.T, cfg TestClientConfig, req *rpc.InternalCreateSIPParticipantRequest) *testSIPClient {
	t.Helper()
	client := NewOutboundTestClient(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_, _ = client.CreateSIPParticipant(ctx, req)
	}()

	select {
	case sipClient := <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
		return sipClient
	case <-time.After(2 * time.Second):
		require.Fail(t, "expected test SIP client to be created")
		return nil
	}
}

func nextTransaction(t *testing.T, sipClient *testSIPClient) *transactionRequest {
	t.Helper()
	select {
	case tr := <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
		return tr
	case <-time.After(2 * time.Second):
		require.Fail(t, "expected INVITE transaction")
		return nil
	}
}

// End to end: sipgo pairs each SRV target with its own port, and a 500 from one
// server moves the call to the next instead of failing it.
func TestOutboundSRVDestinationsAndFailover(t *testing.T) {
	d := twoSBCDNS(t)
	sipClient := startOutboundCall(t, TestClientConfig{DNSResolver: d.resolver()}, MinimalCreateSIPParticipantRequest())

	valid := map[string]string{
		testSBC1IP + ":5006": testSBC2IP + ":5008",
		testSBC2IP + ":5008": testSBC1IP + ":5006",
	}

	first := nextTransaction(t, sipClient)
	require.Equal(t, sip.INVITE, first.req.Method)
	// The request URI stays the configured hostname; only the transport target is pinned.
	require.Equal(t, testInviteTargetHost, first.req.Recipient.Host)
	expectSecond, ok := valid[first.req.Destination()]
	require.True(t, ok, "first INVITE went to %s, which pairs a host with another record's port", first.req.Destination())

	require.NoError(t, first.transaction.SendResponse(
		sip.NewResponseFromRequest(first.req, sip.StatusInternalServerError, "Server Internal Error", nil)))

	second := nextTransaction(t, sipClient)
	require.Equal(t, sip.INVITE, second.req.Method)
	require.Equal(t, expectSecond, second.req.Destination())

	require.NoError(t, second.transaction.SendResponse(
		sip.NewSDPResponseFromRequest(second.req, []byte(testMinimalSDP))))

	select {
	case ack := <-sipClient.requests:
		require.Equal(t, sip.ACK, ack.req.Method)
	case <-time.After(2 * time.Second):
		require.Fail(t, "expected ACK after the second INVITE succeeded")
	}
}

// A status that answers the request must not burn through the other addresses.
func TestOutboundNoFailoverOnTerminalStatus(t *testing.T) {
	d := twoSBCDNS(t)
	sipClient := startOutboundCall(t, TestClientConfig{DNSResolver: d.resolver()}, MinimalCreateSIPParticipantRequest())

	first := nextTransaction(t, sipClient)
	require.NoError(t, first.transaction.SendResponse(
		sip.NewResponseFromRequest(first.req, sip.StatusBusyHere, "Busy Here", nil)))

	select {
	case tr := <-sipClient.transactions:
		require.Fail(t, "unexpected retry after a terminal status", "method=%v", tr.req.Method)
	case <-time.After(500 * time.Millisecond):
	}
}

// With an explicit port SRV is skipped, so every candidate keeps that port.
func TestOutboundExplicitPortSkipsSRV(t *testing.T) {
	d := twoSBCDNS(t)
	req := MinimalCreateSIPParticipantRequest()
	req.Address = testInviteTargetHost + ":5006"
	sipClient := startOutboundCall(t, TestClientConfig{DNSResolver: d.resolver()}, req)

	first := nextTransaction(t, sipClient)
	require.Contains(t, []string{testSBC1IP + ":5006", testSBC2IP + ":5006"}, first.req.Destination())
	require.Zero(t, d.queries("_sip._udp."+testInviteTargetHost, dnsTypeSRV),
		"SRV must not be consulted when the trunk address carries a port")
	// The pinned destination is an IP literal, so sipgo does not resolve again.
	require.Equal(t, 1, d.queries(testInviteTargetHost, dnsTypeA),
		"the host should be resolved exactly once, by ResolveTargets")

	require.NoError(t, first.transaction.SendResponse(
		sip.NewResponseFromRequest(first.req, sip.StatusInternalServerError, "Server Internal Error", nil)))

	second := nextTransaction(t, sipClient)
	require.NotEqual(t, first.req.Destination(), second.req.Destination())
	require.Contains(t, []string{testSBC1IP + ":5006", testSBC2IP + ":5006"}, second.req.Destination())
}
