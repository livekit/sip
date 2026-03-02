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

	psdp "github.com/pion/sdp/v3"
)

func TestGetMediaDirection(t *testing.T) {
	tests := []struct {
		name     string
		sdp      string
		expected string
	}{
		{
			name: "sendonly on audio media",
			sdp: "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
				"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
				"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendonly\r\n",
			expected: "sendonly",
		},
		{
			name: "recvonly on audio media",
			sdp: "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
				"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
				"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=recvonly\r\n",
			expected: "recvonly",
		},
		{
			name: "sendrecv on audio media",
			sdp: "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
				"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
				"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendrecv\r\n",
			expected: "sendrecv",
		},
		{
			name: "inactive on audio media",
			sdp: "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
				"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
				"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=inactive\r\n",
			expected: "inactive",
		},
		{
			name: "no direction attribute defaults to sendrecv",
			sdp: "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
				"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
				"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n",
			expected: "sendrecv",
		},
		{
			name: "session-level sendonly fallback",
			sdp: "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
				"c=IN IP4 1.2.3.4\r\nt=0 0\r\na=sendonly\r\n" +
				"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n",
			expected: "sendonly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sd := new(psdp.SessionDescription)
			err := sd.Unmarshal([]byte(tt.sdp))
			require.NoError(t, err)
			require.Equal(t, tt.expected, getMediaDirection(sd))
		})
	}
}

func TestComplementDirection(t *testing.T) {
	require.Equal(t, "recvonly", complementDirection("sendonly"))
	require.Equal(t, "sendonly", complementDirection("recvonly"))
	require.Equal(t, "sendrecv", complementDirection("sendrecv"))
	require.Equal(t, "inactive", complementDirection("inactive"))
	require.Equal(t, "sendrecv", complementDirection("unknown"))
	require.Equal(t, "sendrecv", complementDirection(""))
}

func TestSetMediaDirection(t *testing.T) {
	originalSDP := "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=LiveKit\r\n" +
		"c=IN IP4 1.2.3.4\r\nt=0 0\r\n" +
		"m=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendrecv\r\n"

	// Set to recvonly
	result, err := setMediaDirection([]byte(originalSDP), "recvonly")
	require.NoError(t, err)

	// Parse result and verify
	sd := new(psdp.SessionDescription)
	err = sd.Unmarshal(result)
	require.NoError(t, err)
	require.Equal(t, "recvonly", getMediaDirection(sd))

	// Verify sendrecv was removed
	require.NotContains(t, string(result), "a=sendrecv")
	require.Contains(t, string(result), "a=recvonly")

	// Set to sendonly
	result2, err := setMediaDirection([]byte(originalSDP), "sendonly")
	require.NoError(t, err)
	sd2 := new(psdp.SessionDescription)
	err = sd2.Unmarshal(result2)
	require.NoError(t, err)
	require.Equal(t, "sendonly", getMediaDirection(sd2))
}

func TestSetMediaDirectionNoAudio(t *testing.T) {
	// SDP with no audio media — should return unchanged
	noAudioSDP := "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
		"c=IN IP4 1.2.3.4\r\nt=0 0\r\n"
	result, err := setMediaDirection([]byte(noAudioSDP), "recvonly")
	require.NoError(t, err)
	require.Equal(t, []byte(noAudioSDP), result)
}

func TestGetRemoteMediaAddr(t *testing.T) {
	t.Run("valid address", func(t *testing.T) {
		sdpStr := "v=0\r\no=- 123 456 IN IP4 152.188.164.198\r\ns=-\r\n" +
			"c=IN IP4 152.188.164.198\r\nt=0 0\r\n" +
			"m=audio 27530 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
		sd := new(psdp.SessionDescription)
		err := sd.Unmarshal([]byte(sdpStr))
		require.NoError(t, err)

		addr, ok := getRemoteMediaAddr(sd)
		require.True(t, ok)
		require.Equal(t, "152.188.164.198", addr.Addr().String())
		require.Equal(t, uint16(27530), addr.Port())
	})

	t.Run("hold address 0.0.0.0", func(t *testing.T) {
		sdpStr := "v=0\r\no=- 123 456 IN IP4 152.188.164.198\r\ns=-\r\n" +
			"c=IN IP4 0.0.0.0\r\nt=0 0\r\n" +
			"m=audio 27530 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendonly\r\n"
		sd := new(psdp.SessionDescription)
		err := sd.Unmarshal([]byte(sdpStr))
		require.NoError(t, err)

		addr, ok := getRemoteMediaAddr(sd)
		require.True(t, ok)
		// 0.0.0.0 is a valid parse result — caller should check IsUnspecified()
		require.True(t, addr.Addr().IsUnspecified())
	})

	t.Run("no audio media", func(t *testing.T) {
		sdpStr := "v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\ns=-\r\n" +
			"c=IN IP4 1.2.3.4\r\nt=0 0\r\n"
		sd := new(psdp.SessionDescription)
		err := sd.Unmarshal([]byte(sdpStr))
		require.NoError(t, err)

		_, ok := getRemoteMediaAddr(sd)
		require.False(t, ok)
	})
}
