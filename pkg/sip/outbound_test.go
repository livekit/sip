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

	msdp "github.com/livekit/media-sdk/sdp"
	pionsdp "github.com/pion/sdp/v3"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"

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

func TestCodecFiltering(t *testing.T) {
	// Test codec filtering in SDP offer
	client := NewOutboundTestClient(t, TestClientConfig{})

	// Set codec configuration that disables PCMA, G722, G729, enables PCMU and telephone-event
	client.conf.Codecs = map[string]bool{
		"PCMU":            true,
		"PCMA":            false,
		"G722":            false,
		"G729":            false,
		"telephone-event": true,
	}

	call := &outboundCall{
		c:   client,
		log: client.log,
	}

	// Create mock SDP offer with multiple codecs including G.711A and G.729
	sdpOffer := &msdp.Offer{
		SDP: pionsdp.SessionDescription{
			MediaDescriptions: []*pionsdp.MediaDescription{
				{
					MediaName: pionsdp.MediaName{
						Media:   "audio",
						Port:    pionsdp.RangedPort{Value: 5004},
						Protos:  []string{"RTP", "AVP"},
						Formats: []string{"0", "8", "9", "18", "101"}, // PCMU, PCMA, G722, G729, telephone-event
					},
					Attributes: []pionsdp.Attribute{
						{Key: "rtpmap", Value: "0 PCMU/8000"},
						{Key: "rtpmap", Value: "8 PCMA/8000"},
						{Key: "rtpmap", Value: "9 G722/8000"},
						{Key: "rtpmap", Value: "18 G729/8000"},
						{Key: "rtpmap", Value: "101 telephone-event/8000"},
						{Key: "fmtp", Value: "101 0-16"},
						{Key: "ptime", Value: "20"},
					},
				},
			},
		},
	}

	// Apply codec filtering
	err := call.filterCodecsFromSDP(sdpOffer)
	require.NoError(t, err)

	// Check that only enabled codecs remain
	media := sdpOffer.SDP.MediaDescriptions[0]

	// Should only have PCMU (0) and telephone-event (101)
	expectedFormats := []string{"0", "101"}
	require.Equal(t, expectedFormats, media.MediaName.Formats)

	// Check rtpmap attributes
	var rtpmapAttrs []string
	var fmtpAttrs []string
	for _, attr := range media.Attributes {
		if attr.Key == "rtpmap" {
			rtpmapAttrs = append(rtpmapAttrs, attr.Value)
		}
		if attr.Key == "fmtp" {
			fmtpAttrs = append(fmtpAttrs, attr.Value)
		}
	}

	// Should have rtpmap for PCMU and telephone-event
	expectedRtpmaps := []string{"0 PCMU/8000", "101 telephone-event/8000"}
	require.Equal(t, expectedRtpmaps, rtpmapAttrs)

	// Should have fmtp for telephone-event
	expectedFmtps := []string{"101 0-16"}
	require.Equal(t, expectedFmtps, fmtpAttrs)

	// Should keep other attributes like ptime
	foundPtime := false
	for _, attr := range media.Attributes {
		if attr.Key == "ptime" && attr.Value == "20" {
			foundPtime = true
			break
		}
	}
	require.True(t, foundPtime, "ptime attribute should be preserved")
}

func TestCodecFilteringStaticPayloadTypes(t *testing.T) {
	// Test codec filtering with static payload types that don't have rtpmap attributes
	client := NewOutboundTestClient(t, TestClientConfig{})

	// Set codec configuration that enables PCMU, PCMA, disables G722, G729
	client.conf.Codecs = map[string]bool{
		"PCMU": true,
		"PCMA": true,
		"G722": false,
		"G729": false,
	}

	call := &outboundCall{
		c:   client,
		log: client.log,
	}

	// Create mock SDP offer with static payload types (no rtpmap attributes for static types)
	sdpOffer := &msdp.Offer{
		SDP: pionsdp.SessionDescription{
			MediaDescriptions: []*pionsdp.MediaDescription{
				{
					MediaName: pionsdp.MediaName{
						Media:   "audio",
						Port:    pionsdp.RangedPort{Value: 5004},
						Protos:  []string{"RTP", "AVP"},
						Formats: []string{"0", "8", "9", "18", "101"}, // PCMU, PCMA, G722, G729, telephone-event
					},
					Attributes: []pionsdp.Attribute{
						// Only telephone-event has rtpmap (static types don't need it per RFC 3264)
						{Key: "rtpmap", Value: "101 telephone-event/8000"},
						{Key: "fmtp", Value: "101 0-16"},
						{Key: "ptime", Value: "20"},
					},
				},
			},
		},
	}

	// Apply codec filtering
	err := call.filterCodecsFromSDP(sdpOffer)
	require.NoError(t, err)

	// Check that only enabled codecs remain (PCMU, PCMA, telephone-event)
	media := sdpOffer.SDP.MediaDescriptions[0]
	expectedFormats := []string{"0", "8", "101"}
	require.Equal(t, expectedFormats, media.MediaName.Formats)

	// Check that rtpmap and fmtp attributes are preserved for telephone-event
	var rtpmapAttrs []string
	var fmtpAttrs []string
	for _, attr := range media.Attributes {
		if attr.Key == "rtpmap" {
			rtpmapAttrs = append(rtpmapAttrs, attr.Value)
		}
		if attr.Key == "fmtp" {
			fmtpAttrs = append(fmtpAttrs, attr.Value)
		}
	}

	expectedRtpmaps := []string{"101 telephone-event/8000"}
	require.Equal(t, expectedRtpmaps, rtpmapAttrs)

	expectedFmtps := []string{"101 0-16"}
	require.Equal(t, expectedFmtps, fmtpAttrs)
}

func TestCodecNameExtraction(t *testing.T) {
	// Test getCodecNameFromRTPMap function with various codecs
	testCases := []struct {
		rtpmap   string
		expected string
	}{
		{"0 PCMU/8000", "PCMU"},
		{"8 PCMA/8000", "PCMA"},
		{"9 G722/8000", "G722"},
		{"18 G729/8000", "G729"},
		{"101 telephone-event/8000", "telephone-event"},
		{"", ""},
		{"invalid", ""},
		{"123 codec/48000", "codec"},
	}

	for _, tc := range testCases {
		result := getCodecNameFromRTPMap(tc.rtpmap)
		require.Equal(t, tc.expected, result, "getCodecNameFromRTPMap(%q) = %q, expected %q", tc.rtpmap, result, tc.expected)
	}
}

func TestCodecNameByPayload(t *testing.T) {
	// Test getCodecNameByPayload function with static payload types
	testCases := []struct {
		payload  string
		expected string
	}{
		{"0", "PCMU"},
		{"8", "PCMA"},
		{"9", "G722"},
		{"18", "G729"},
		{"101", "telephone-event"},
		{"96", ""}, // Dynamic payload type
		{"", ""},
		{"invalid", ""},
	}

	for _, tc := range testCases {
		result := getCodecNameByPayload(tc.payload)
		require.Equal(t, tc.expected, result, "getCodecNameByPayload(%q) = %q, expected %q", tc.payload, result, tc.expected)
	}
}

func TestFromDomainPriority(t *testing.T) {
	// Test priority: from_domain > host > SIPFromDomain > fallback
	testCases := []struct {
		name          string
		hostname      string
		sipFromDomain string
		expected      string
		description   string
	}{
		{
			name:          "hostname used when set",
			hostname:      "hostname.com",
			sipFromDomain: "",
			expected:      "hostname.com",
			description:   "hostname should be used when set",
		},
		{
			name:          "SIPFromDomain used when hostname not set",
			hostname:      "",
			sipFromDomain: "global-from-domain.com",
			expected:      "global-from-domain.com",
			description:   "SIPFromDomain should be used when hostname is empty",
		},
		{
			name:          "hostname takes priority over SIPFromDomain",
			hostname:      "hostname.com",
			sipFromDomain: "global-from-domain.com",
			expected:      "hostname.com",
			description:   "hostname should take priority over SIPFromDomain",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create client with SIPFromDomain config
			client := NewOutboundTestClient(t, TestClientConfig{
				Config: &config.Config{
					SIPFromDomain: tc.sipFromDomain,
				},
			})

			req := MinimalCreateSIPParticipantRequest()
			req.Hostname = tc.hostname

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go func() {
				_, err := client.CreateSIPParticipant(ctx, req)
				if err != nil && ctx.Err() == nil {
					t.Logf("CreateSIPParticipant error: %v", err)
				}
			}()

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

			require.NotNil(t, tr)
			require.NotNil(t, tr.req)
			require.Equal(t, sip.INVITE, tr.req.Method)

			fromHeader := tr.req.From()
			require.NotNil(t, fromHeader)
			// Extract host from From header URI
			fromHost := fromHeader.Address.Host
			require.Equal(t, tc.expected, fromHost, tc.description)

			cancel()
		})
	}
}

func TestFromDomainInINVITE(t *testing.T) {
	// Test that From header in INVITE contains the correct domain when from_domain is set via config
	client := NewOutboundTestClient(t, TestClientConfig{
		Config: &config.Config{
			SIPFromDomain: "test-from-domain.com",
		},
	})

	req := MinimalCreateSIPParticipantRequest()
	req.Hostname = "" // Don't set hostname to test SIPFromDomain fallback

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

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

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.Equal(t, sip.INVITE, tr.req.Method)

	fromHeader := tr.req.From()
	require.NotNil(t, fromHeader)
	fromHost := fromHeader.Address.Host
	require.Equal(t, "test-from-domain.com", fromHost, "From header should use SIPFromDomain from config")

	cancel()
}

func TestCreateSIPCallInfoFromDomainPriority(t *testing.T) {
	// Test that createSIPCallInfo uses the same priority as newCall for determining fromHost
	testCases := []struct {
		name          string
		fromDomain    string
		hostname      string
		sipFromDomain string
		expected      string
		description   string
	}{
		{
			name:          "fromDomain has highest priority",
			fromDomain:    "from-domain.com",
			hostname:      "hostname.com",
			sipFromDomain: "global-from-domain.com",
			expected:      "from-domain.com",
			description:   "fromDomain should take highest priority",
		},
		{
			name:          "hostname used when fromDomain empty",
			fromDomain:    "",
			hostname:      "hostname.com",
			sipFromDomain: "global-from-domain.com",
			expected:      "hostname.com",
			description:   "hostname should be used when fromDomain is empty",
		},
		{
			name:          "SIPFromDomain used when hostname empty",
			fromDomain:    "",
			hostname:      "",
			sipFromDomain: "global-from-domain.com",
			expected:      "global-from-domain.com",
			description:   "SIPFromDomain should be used when hostname is empty",
		},
		{
			name:          "fallback to contact host when all empty",
			fromDomain:    "",
			hostname:      "",
			sipFromDomain: "",
			expected:      "test-client.example.com", // from test client
			description:   "should fallback to contact host when all domain fields are empty",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewOutboundTestClient(t, TestClientConfig{
				Config: &config.Config{
					SIPFromDomain: tc.sipFromDomain,
				},
			})

			req := MinimalCreateSIPParticipantRequest()
			req.Hostname = tc.hostname

			// Call createSIPCallInfo with the fromDomain parameter
			callInfo := client.createSIPCallInfo(req, tc.fromDomain)

			// Extract the host from the FromUri
			fromUriHost := callInfo.FromUri.Host
			require.Equal(t, tc.expected, fromUriHost, tc.description)
		})
	}
}

func TestCodecFilteringNilMediaDescription(t *testing.T) {
	// Test that codec filtering doesn't panic when MediaDescriptions contains nil pointers
	client := NewOutboundTestClient(t, TestClientConfig{})

	// Set codec configuration
	client.conf.Codecs = map[string]bool{
		"PCMU": true,
	}

	call := &outboundCall{
		c:   client,
		log: client.log,
	}

	// Create mock SDP offer with nil media description in the slice
	sdpOffer := &msdp.Offer{
		SDP: pionsdp.SessionDescription{
			MediaDescriptions: []*pionsdp.MediaDescription{
				nil, // This nil pointer should not cause a panic
				{
					MediaName: pionsdp.MediaName{
						Media:   "audio",
						Port:    pionsdp.RangedPort{Value: 5004},
						Protos:  []string{"RTP", "AVP"},
						Formats: []string{"0"}, // PCMU
					},
					Attributes: []pionsdp.Attribute{
						{Key: "rtpmap", Value: "0 PCMU/8000"},
					},
				},
			},
		},
	}

	// This should not panic and should process the non-nil media description
	err := call.filterCodecsFromSDP(sdpOffer)
	require.NoError(t, err)

	// Should have only one media description left (the nil one was skipped)
	require.Len(t, sdpOffer.SDP.MediaDescriptions, 2)

	// The second media description should still be processed correctly
	media := sdpOffer.SDP.MediaDescriptions[1]
	require.Equal(t, []string{"0"}, media.MediaName.Formats)
}

func TestTrunkFromDomainPriority(t *testing.T) {
	// Test that trunk FromDomain takes highest priority
	client := NewOutboundTestClient(t, TestClientConfig{
		Config: &config.Config{
			SIPFromDomain: "global-from-domain.com",
		},
	})

	// Add trunk configuration with FromDomain
	trunkID := "test-trunk-123"
	trunkConfig := &TrunkConfig{
		FromDomain: "trunk-from-domain.com",
		Name:       "Test Trunk",
		Numbers:    []string{"+1234567890"},
	}
	client.AddTrunkConfig(trunkID, trunkConfig)

	req := MinimalCreateSIPParticipantRequest()
	req.Hostname = "hostname.com"
	req.SipTrunkId = trunkID // Set trunk ID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

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

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.Equal(t, sip.INVITE, tr.req.Method)

	fromHeader := tr.req.From()
	require.NotNil(t, fromHeader)
	fromHost := fromHeader.Address.Host
	require.Equal(t, "trunk-from-domain.com", fromHost, "Trunk FromDomain should take highest priority")

	cancel()
}

func TestTrunkFromDomainFallback(t *testing.T) {
	// Test that when trunk has no FromDomain, it falls back to hostname
	client := NewOutboundTestClient(t, TestClientConfig{
		Config: &config.Config{
			SIPFromDomain: "global-from-domain.com",
		},
	})

	// Add trunk configuration without FromDomain
	trunkID := "test-trunk-456"
	trunkConfig := &TrunkConfig{
		Name:    "Test Trunk Without FromDomain",
		Numbers: []string{"+1234567890"},
	}
	client.AddTrunkConfig(trunkID, trunkConfig)

	req := MinimalCreateSIPParticipantRequest()
	req.Hostname = "hostname.com"
	req.SipTrunkId = trunkID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_, err := client.CreateSIPParticipant(ctx, req)
		if err != nil && ctx.Err() == nil {
			t.Logf("CreateSIPParticipant error: %v", err)
		}
	}()

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

	require.NotNil(t, tr)
	require.NotNil(t, tr.req)
	require.Equal(t, sip.INVITE, tr.req.Method)

	fromHeader := tr.req.From()
	require.NotNil(t, fromHeader)
	fromHost := fromHeader.Address.Host
	require.Equal(t, "hostname.com", fromHost, "Should fallback to hostname when trunk has no FromDomain")

	cancel()
}
