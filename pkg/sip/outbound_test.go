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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo/sip"
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
	participantReq *rpc.InternalCreateSIPParticipantRequest,
	mutate func(tr *transactionRequest, resp *sip.Response),
) (*transactionRequest, *sipRequest) {
	t.Helper()

	client := NewOutboundTestClient(t, TestClientConfig{})
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
		return nil, nil
	}

	var tr *transactionRequest
	select {
	case tr = <-sipClient.transactions:
		t.Cleanup(func() { tr.transaction.Terminate() })
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "expected INVITE transaction")
		return nil, nil
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
		return tr, nil
	}

	require.Equal(t, sip.ACK, ackReq.req.Method)
	return tr, ackReq
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

		_, ackReq := waitOutboundINVITEAndACK(t, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
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

		_, ackReq := waitOutboundINVITEAndACK(t, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
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

		_, ackReq := waitOutboundINVITEAndACK(t, MinimalCreateSIPParticipantRequest(), func(tr *transactionRequest, resp *sip.Response) {
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
}
