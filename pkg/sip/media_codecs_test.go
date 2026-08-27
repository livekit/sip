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

package sip

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// sdpWithMedia builds a minimal SDP body with the given m= line and attributes.
func sdpWithMedia(media string, attrs ...string) []byte {
	body := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" + media + "\r\n"
	for _, a := range attrs {
		body += a + "\r\n"
	}
	return []byte(body)
}

// Testing edge cases that ParseOfferWith sometimes returns
func TestPeerCodecNames(t *testing.T) {
	cases := []struct {
		name string
		sdp  []byte
		exp  []string
	}{
		{
			// ParseMediaWith diverts telephone-event from a=rtpmap into DTMFType,
			// but its payload type stays in m=audio and resolves to no codec.
			// Without the skip it would be reported as an unsupported codec
			name: "telephone-event is not an unsupported codec",
			sdp: sdpWithMedia("m=audio 5004 RTP/AVP 0 101",
				"a=rtpmap:0 PCMU/8000", "a=rtpmap:101 telephone-event/8000"),
			exp: []string{"PCMU/8000", "telephone-event/8000"},
		},
		{
			// No a=rtpmap at all, codecs resolved from the static payload types
			name: "static payload types only",
			sdp:  sdpWithMedia("m=audio 5004 RTP/AVP 0 8"),
			exp:  []string{"PCMU/8000", "PCMA/8000"},
		},
		{
			// A codec listed in both a=rtpmap and m=audio is parsed twice
			name: "deduplicated",
			sdp:  sdpWithMedia("m=audio 5004 RTP/AVP 0", "a=rtpmap:0 PCMU/8000"),
			exp:  []string{"PCMU/8000"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			offer, err := parseOfferWith(nil, defaultCodecs, c.sdp)
			require.NoError(t, err)
			require.ElementsMatch(t, c.exp, peerCodecNames(offer.MediaDesc))
		})
	}
}
