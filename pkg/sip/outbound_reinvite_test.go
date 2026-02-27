// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sipgo/sip"
)

// newTestOutboundCall creates a minimal outbound call for re-INVITE testing.
func newTestOutboundCall(callID string, localTag LocalTag, sdpOffer []byte) (*Client, *outboundCall) {
	log := logger.GetLogger()
	c := &Client{
		log:         log,
		conf:        &config.Config{},
		activeCalls: make(map[LocalTag]*outboundCall),
		byRemote:    make(map[RemoteTag]*outboundCall),
		byCallID:    make(map[string]*outboundCall),
	}

	// Build a minimal INVITE request with the SDP offer body
	invite := sip.NewRequest(sip.INVITE, sip.Uri{Host: "remote.com", User: "callee"})
	invite.SetBody(sdpOffer)

	contactHeader := &sip.ContactHeader{
		Address: sip.Uri{Host: "local.com", Port: 5060},
	}

	out := &sipOutbound{
		log:     log,
		c:       c,
		id:      localTag,
		callID:  callID,
		invite:  invite,
		contact: contactHeader,
	}

	call := &outboundCall{
		cc:  out,
		log: log,
	}

	// Register in all maps
	c.activeCalls[localTag] = call
	if callID != "" {
		c.byCallID[callID] = call
	}

	return c, call
}

// TestOutboundReInviteHandling tests that Client.onInvite properly routes
// re-INVITEs to the matching outbound call.
func TestOutboundReInviteHandling(t *testing.T) {
	sdpOffer := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\nc=IN IP4 1.2.3.4\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", sdpOffer)

	// Build a re-INVITE with the same Call-ID
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-call-123")
	req.AppendHeader(&callIDHeader)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.True(t, handled, "re-INVITE should be handled by client")
	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
	require.Equal(t, sdpOffer, tx.responses[0].Body(), "should respond with our SDP offer")

	// Verify Content-Type header
	ctHeader := tx.responses[0].GetHeader("Content-Type")
	require.NotNil(t, ctHeader, "should have Content-Type header")
	require.Equal(t, "application/sdp", ctHeader.Value(), "Content-Type should be application/sdp")
}

// TestOutboundReInviteUnknownCall tests that Client.onInvite returns false
// for re-INVITEs with unknown Call-IDs.
func TestOutboundReInviteUnknownCall(t *testing.T) {
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", []byte("v=0\r\n"))

	// Build a re-INVITE with a different Call-ID
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("unknown-call-456")
	req.AppendHeader(&callIDHeader)

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.False(t, handled, "re-INVITE with unknown Call-ID should not be handled")
	require.Len(t, tx.responses, 0, "should not send any response")
}

// TestOutboundReInviteNoCallID tests that Client.onInvite returns false
// when the INVITE has no Call-ID header.
func TestOutboundReInviteNoCallID(t *testing.T) {
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", []byte("v=0\r\n"))

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	// No Call-ID header

	tx := &testServerTransaction{}
	handled := c.onInvite(req, tx)

	require.False(t, handled, "INVITE without Call-ID should not be handled")
	require.Len(t, tx.responses, 0, "should not send any response")
}

// TestOutboundAcceptReInviteNoSDP tests that AcceptReInvite responds with 500
// when no SDP is available (invite is nil).
func TestOutboundAcceptReInviteNoSDP(t *testing.T) {
	log := logger.GetLogger()
	out := &sipOutbound{
		log: log,
		contact: &sip.ContactHeader{
			Address: sip.Uri{Host: "local.com", Port: 5060},
		},
		// invite is nil, so no SDP available
	}

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	tx := &testServerTransaction{}

	out.AcceptReInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusInternalServerError, tx.responses[0].StatusCode,
		"should respond with 500 when no SDP available")
}

// TestOutboundByCallIDCleanup tests that byCallID is cleaned up properly.
func TestOutboundByCallIDCleanup(t *testing.T) {
	c, _ := newTestOutboundCall("test-call-123", "SCL_test123", []byte("v=0\r\n"))

	// Verify the call is registered
	c.cmu.Lock()
	require.NotNil(t, c.byCallID["test-call-123"], "call should be in byCallID")
	require.NotNil(t, c.activeCalls["SCL_test123"], "call should be in activeCalls")
	c.cmu.Unlock()

	// Simulate cleanup (what happens in outboundCall.close)
	c.cmu.Lock()
	delete(c.activeCalls, "SCL_test123")
	delete(c.byCallID, "test-call-123")
	c.cmu.Unlock()

	// Verify cleanup
	c.cmu.Lock()
	require.Nil(t, c.byCallID["test-call-123"], "call should be removed from byCallID")
	require.Nil(t, c.activeCalls["SCL_test123"], "call should be removed from activeCalls")
	c.cmu.Unlock()
}

// TestOnRequestRoutesInviteToClient tests that Client.OnRequest properly
// routes INVITE method to the onInvite handler.
func TestOnRequestRoutesInviteToClient(t *testing.T) {
	sdpOffer := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\nc=IN IP4 1.2.3.4\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	c, _ := newTestOutboundCall("test-call-789", "SCL_test789", sdpOffer)

	// Build a re-INVITE with matching Call-ID
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "local.com"})
	callIDHeader := sip.CallIDHeader("test-call-789")
	req.AppendHeader(&callIDHeader)

	tx := &testServerTransaction{}
	handled := c.OnRequest(req, tx)

	require.True(t, handled, "OnRequest should handle INVITE for known outbound call")
	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
}
