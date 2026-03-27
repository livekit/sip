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

package security

import (
	"testing"
)

func TestSRTPEnforcer_NotEnforcing(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: false})
	if e.IsEnforcing() {
		t.Error("should not be enforcing")
	}

	// Should accept anything when not enforcing
	sdp := "v=0\r\nm=video 49170 RTP/AVP 96\r\n"
	if err := e.ValidateSDP(sdp); err != nil {
		t.Errorf("should accept RTP/AVP when not enforcing: %v", err)
	}
}

func TestSRTPEnforcer_AcceptSAVP(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	sdp := "v=0\r\nm=video 49170 RTP/SAVP 96\r\n"
	if err := e.ValidateSDP(sdp); err != nil {
		t.Errorf("should accept RTP/SAVP: %v", err)
	}
}

func TestSRTPEnforcer_AcceptSAVPF(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	sdp := "v=0\r\nm=audio 5004 RTP/SAVPF 111\r\nm=video 49170 RTP/SAVPF 96\r\n"
	if err := e.ValidateSDP(sdp); err != nil {
		t.Errorf("should accept RTP/SAVPF: %v", err)
	}
}

func TestSRTPEnforcer_RejectAVP(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	sdp := "v=0\r\nm=video 49170 RTP/AVP 96\r\n"
	if err := e.ValidateSDP(sdp); err == nil {
		t.Error("should reject RTP/AVP when SRTP enforced")
	}
}

func TestSRTPEnforcer_RejectAVPF(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	sdp := "v=0\r\nm=video 49170 RTP/AVPF 96\r\n"
	if err := e.ValidateSDP(sdp); err == nil {
		t.Error("should reject RTP/AVPF when SRTP enforced")
	}
}

func TestSRTPEnforcer_MixedMediaLines(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	// One line SAVP, one line AVP → should reject
	sdp := "v=0\r\nm=audio 5004 RTP/SAVP 111\r\nm=video 49170 RTP/AVP 96\r\n"
	if err := e.ValidateSDP(sdp); err == nil {
		t.Error("should reject when any media line uses non-SRTP profile")
	}
}

func TestSRTPEnforcer_CustomProfiles(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{
		Enforce:         true,
		AllowedProfiles: []string{"RTP/SAVP"},
	})
	// SAVPF should be rejected since only SAVP is allowed
	sdp := "v=0\r\nm=video 49170 RTP/SAVPF 96\r\n"
	if err := e.ValidateSDP(sdp); err == nil {
		t.Error("should reject RTP/SAVPF when only RTP/SAVP is allowed")
	}
}

func TestSRTPEnforcer_NoMediaLines(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	sdp := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=-\r\n"
	if err := e.ValidateSDP(sdp); err != nil {
		t.Errorf("SDP without media lines should pass: %v", err)
	}
}

func TestSRTPEnforcer_CaseInsensitive(t *testing.T) {
	e := NewSRTPEnforcer(SRTPConfig{Enforce: true})
	sdp := "v=0\r\nm=video 49170 rtp/savp 96\r\n"
	// extractMediaProfile returns lowercase, but allowedProfiles are uppercased
	// and we compare with ToUpper, so this should work
	if err := e.ValidateSDP(sdp); err != nil {
		t.Errorf("should accept case-insensitive profile: %v", err)
	}
}

func TestExtractMediaProfile(t *testing.T) {
	cases := []struct {
		line     string
		expected string
	}{
		{"m=video 49170 RTP/SAVP 96", "RTP/SAVP"},
		{"m=audio 5004 RTP/AVP 0 8", "RTP/AVP"},
		{"m=video 0 RTP/SAVPF 96 97", "RTP/SAVPF"},
		{"m=application 9 UDP/DTLS/SCTP webrtc-datachannel", "UDP/DTLS/SCTP"},
		{"m=invalid", ""},
		{"m=video 1234", ""},
	}
	for _, tc := range cases {
		got := extractMediaProfile(tc.line)
		if got != tc.expected {
			t.Errorf("extractMediaProfile(%q) = %q, want %q", tc.line, got, tc.expected)
		}
	}
}
