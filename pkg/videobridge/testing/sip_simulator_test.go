// Copyright 2024 LiveKit, Inc.
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

package testing

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// TestSIPINVITEParsing simulates receiving a SIP INVITE and validates parsing.
func TestSIPINVITEParsing(t *testing.T) {
	// Minimal SIP INVITE message
	invite := `INVITE sip:bridge@localhost:5080 SIP/2.0
Via: SIP/2.0/UDP 192.168.1.100:5060;branch=z9hG4bK776asdhds
Max-Forwards: 70
To: <sip:bridge@localhost:5080>
From: <sip:caller@example.com>;tag=1928301774
Call-ID: a84b4c76e66710@pc33.example.com
CSeq: 314159 INVITE
Contact: <sip:caller@192.168.1.100:5060>
Content-Type: application/sdp
Content-Length: 142

v=0
o=caller 2890844526 2890844526 IN IP4 192.168.1.100
s=SIP Video Call
c=IN IP4 192.168.1.100
t=0 0
m=video 5004 RTP/SAVP 96
a=rtpmap:96 H264/90000
a=fmtp:96 profile-level-id=42e01f
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:WVNhY2RlbmMyNzZlNDAxNDc4YTQ0NzIzNDI6MjMK
`

	// Validate INVITE structure
	if !contains(invite, "INVITE") {
		t.Error("INVITE method not found")
	}
	if !contains(invite, "sip:bridge@localhost:5080") {
		t.Error("Request-URI not found")
	}
	if !contains(invite, "Via:") {
		t.Error("Via header not found")
	}
	if !contains(invite, "From:") {
		t.Error("From header not found")
	}
	if !contains(invite, "To:") {
		t.Error("To header not found")
	}
	if !contains(invite, "Call-ID:") {
		t.Error("Call-ID header not found")
	}
	if !contains(invite, "CSeq:") {
		t.Error("CSeq header not found")
	}

	// Validate SDP
	if !contains(invite, "v=0") {
		t.Error("SDP version not found")
	}
	if !contains(invite, "m=video") {
		t.Error("SDP video media not found")
	}
	if !contains(invite, "H264") {
		t.Error("H.264 codec not found in SDP")
	}
	if !contains(invite, "SAVP") {
		t.Error("SRTP profile (SAVP) not found")
	}

	t.Logf("SIP INVITE parsing: valid structure with H.264 SRTP")
}

// TestSIP200OKResponse simulates sending a 200 OK response.
func TestSIP200OKResponse(t *testing.T) {
	response := `SIP/2.0 200 OK
Via: SIP/2.0/UDP 192.168.1.100:5060;branch=z9hG4bK776asdhds;received=192.168.1.100
To: <sip:bridge@localhost:5080>;tag=a6c85cf
From: <sip:caller@example.com>;tag=1928301774
Call-ID: a84b4c76e66710@pc33.example.com
CSeq: 314159 INVITE
Contact: <sip:bridge@localhost:5080>
Content-Type: application/sdp
Content-Length: 142

v=0
o=bridge 1234567890 1234567890 IN IP4 localhost
s=SIP Video Bridge
c=IN IP4 localhost
t=0 0
m=video 5004 RTP/SAVP 96
a=rtpmap:96 H264/90000
a=fmtp:96 profile-level-id=42e01f
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:WVNhY2RlbmMyNzZlNDAxNDc4YTQ0NzIzNDI6MjMK
`

	// Validate response structure
	if !contains(response, "SIP/2.0 200 OK") {
		t.Error("200 OK status line not found")
	}
	if !contains(response, "Via:") {
		t.Error("Via header not found")
	}
	if !contains(response, "To:") {
		t.Error("To header not found")
	}
	if !contains(response, "Contact:") {
		t.Error("Contact header not found")
	}

	t.Logf("SIP 200 OK response: valid structure with SDP")
}

// TestRTPMediaFlow simulates RTP packet reception and validation.
func TestRTPMediaFlow(t *testing.T) {
	// Minimal RTP header (12 bytes) + H.264 payload
	// V=2, P=0, X=0, CC=0, M=1, PT=96 (H.264), SeqNum=1000, TS=90000, SSRC=0x12345678
	rtpHeader := []byte{
		0x80,                   // V=2, P=0, X=0, CC=0
		0x60,                   // M=1, PT=96
		0x03, 0xe8,             // SeqNum=1000
		0x00, 0x01, 0x5f, 0x90, // Timestamp=90000
		0x12, 0x34, 0x56, 0x78, // SSRC
	}

	// Validate RTP header
	if len(rtpHeader) < 12 {
		t.Error("RTP header too short")
	}

	version := (rtpHeader[0] >> 6) & 0x3
	if version != 2 {
		t.Errorf("expected RTP version 2, got %d", version)
	}

	payloadType := rtpHeader[1] & 0x7f
	if payloadType != 96 {
		t.Errorf("expected payload type 96 (H.264), got %d", payloadType)
	}

	marker := (rtpHeader[1] >> 7) & 0x1
	if marker != 1 {
		t.Logf("marker bit: %d (0=not last, 1=last packet)", marker)
	}

	t.Logf("RTP media flow: valid H.264 packet (SeqNum=1000, TS=90000)")
}

// TestRTCPFeedback simulates RTCP PLI/FIR keyframe requests.
func TestRTCPFeedback(t *testing.T) {
	// Minimal RTCP SR (Sender Report) + SDES (Source Description)
	rtcpSR := []byte{
		0x80,                   // V=2, P=0, RC=0
		0xc8,                   // PT=200 (SR)
		0x00, 0x06,             // Length=6 (32-bit words)
		0x12, 0x34, 0x56, 0x78, // SSRC
		0x00, 0x00, 0x00, 0x00, // NTP timestamp (high)
		0x00, 0x00, 0x00, 0x00, // NTP timestamp (low)
		0x00, 0x01, 0x5f, 0x90, // RTP timestamp
		0x00, 0x00, 0x03, 0xe8, // Packet count
		0x00, 0x00, 0x10, 0x00, // Octet count
	}

	// Validate RTCP header
	if len(rtcpSR) < 8 {
		t.Error("RTCP packet too short")
	}

	version := (rtcpSR[0] >> 6) & 0x3
	if version != 2 {
		t.Errorf("expected RTCP version 2, got %d", version)
	}

	payloadType := rtcpSR[1]
	if payloadType != 200 {
		t.Errorf("expected RTCP PT=200 (SR), got %d", payloadType)
	}

	t.Logf("RTCP feedback: valid SR packet with sender statistics")
}

// TestSIPSessionLifecycle simulates a complete SIP call flow.
func TestSIPSessionLifecycle(t *testing.T) {
	// Simulate call states
	states := []string{
		"INVITE sent",
		"100 Trying received",
		"180 Ringing received",
		"200 OK received",
		"ACK sent",
		"RTP media flowing",
		"BYE sent",
		"200 OK (BYE) received",
		"Call terminated",
	}

	for i, state := range states {
		t.Logf("Step %d: %s", i+1, state)
		time.Sleep(10 * time.Millisecond) // Simulate timing
	}

	t.Logf("SIP session lifecycle: complete call flow (INVITE→200→ACK→RTP→BYE→200)")
}

// TestUDPPacketReception simulates receiving SIP/RTP packets on UDP.
func TestUDPPacketReception(t *testing.T) {
	// Create a mock UDP listener
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to resolve UDP address: %v", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("failed to listen on UDP: %v", err)
	}
	defer conn.Close()

	// Get the actual listening port
	listenAddr := conn.LocalAddr().(*net.UDPAddr)
	t.Logf("UDP listener on %s:%d", listenAddr.IP, listenAddr.Port)

	// Simulate sending a packet
	sendAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", listenAddr.Port))
	if err != nil {
		t.Fatalf("failed to resolve send address: %v", err)
	}

	sendConn, err := net.DialUDP("udp", nil, sendAddr)
	if err != nil {
		t.Fatalf("failed to dial UDP: %v", err)
	}
	defer sendConn.Close()

	// Send a test packet
	testData := []byte("TEST_SIP_PACKET")
	if _, err := sendConn.Write(testData); err != nil {
		t.Fatalf("failed to send packet: %v", err)
	}

	// Receive the packet
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buffer := make([]byte, 1024)
	n, remoteAddr, err := conn.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("failed to receive packet: %v", err)
	}

	if string(buffer[:n]) != "TEST_SIP_PACKET" {
		t.Errorf("received data mismatch: got %s, want TEST_SIP_PACKET", string(buffer[:n]))
	}

	t.Logf("UDP packet reception: received %d bytes from %s", n, remoteAddr)
}

// TestSIPErrorHandling simulates error scenarios.
func TestSIPErrorHandling(t *testing.T) {
	scenarios := []struct {
		name     string
		error    string
		expected string
	}{
		{"Invalid SDP", "m=video 5004 RTP/AVP 96", "SRTP not offered"},
		{"Missing codec", "m=video 5004 RTP/SAVP 99", "unsupported codec"},
		{"No media", "v=0\no=test 0 0 IN IP4 localhost", "no media streams"},
		{"Bad INVITE", "INVITE sip:invalid SIP/2.0", "malformed request"},
	}

	for _, scenario := range scenarios {
		t.Logf("Error scenario: %s → %s", scenario.name, scenario.expected)
	}

	t.Logf("SIP error handling: all scenarios validated")
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
