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

	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestOutboundRouteHeaderWithRecordRoute(t *testing.T) {
	// Make sure the ACK doesn't carry over initial Route header.
	// Steps:
	// 1. Create a SIP participant with an initial Route header.
	// 2. Make sure the Route header is properly populates in INVITE.
	// 3. Fake a 200 response with Record Route headers.
	// 4. Make sure the ACK doesn't carry over initial Route header..

	// Plumbing
	initialRouteURI := sip.Uri{Host: "initial-header.com", UriParams: sip.HeaderParams{"lr": ""}}
	addedRouteURI := sip.Uri{Host: "added-header.com", UriParams: sip.HeaderParams{"lr": ""}}
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
