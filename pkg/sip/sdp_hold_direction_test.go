// Copyright 2026 LiveKit, Inc.
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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const carrierHoldOffer = "v=0\r\n" +
	"o=genband 1642851412 1894065156 IN IP4 203.0.113.20\r\n" +
	"s=-\r\n" +
	"c=IN IP4 203.0.113.20\r\n" +
	"t=0 0\r\n" +
	"m=audio 29076 RTP/AVP 0 8 18 9 13 101\r\n" +
	"c=IN IP4 203.0.113.20\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n" +
	"a=rtpmap:101 telephone-event/8000\r\n" +
	"a=fmtp:101 0-15\r\n" +
	"a=sendonly\r\n" +
	"a=ptime:20\r\n"

const cachedLocalSDP = "v=0\r\n" +
	"o=- 9157931411869268290 9157931411869268294 IN IP4 203.0.113.10\r\n" +
	"s=LiveKit\r\n" +
	"c=IN IP4 203.0.113.10\r\n" +
	"t=0 0\r\n" +
	"m=audio 17830 RTP/AVP 0 101\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:101 telephone-event/8000\r\n" +
	"a=fmtp:101 0-16\r\n" +
	"a=ptime:20\r\n" +
	"a=sendrecv\r\n"

func TestAnswerDirectionFor(t *testing.T) {
	cases := []struct {
		offer string
		want  string
	}{
		{"a=sendonly\r\n", "recvonly"},
		{"a=recvonly\r\n", ""},
		{"a=inactive\r\n", ""},
		{"a=sendrecv\r\n", "sendrecv"},
		{"m=audio 1 RTP/AVP 0\r\n", ""},
		{carrierHoldOffer, "recvonly"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, answerDirectionFor([]byte(tc.offer)))
	}
}

func TestUnsupportedHoldOffer(t *testing.T) {
	require.True(t, unsupportedHoldOffer([]byte("a=recvonly\r\n")))
	require.True(t, unsupportedHoldOffer([]byte("a=inactive\r\n")))
	require.False(t, unsupportedHoldOffer([]byte("a=sendonly\r\n")))
	require.False(t, unsupportedHoldOffer([]byte("a=sendrecv\r\n")))
	require.False(t, unsupportedHoldOffer([]byte("m=audio 1 RTP/AVP 0\r\n")))
}

func TestWithSDPDirectionCarrierHold(t *testing.T) {
	got := withSDPDirection([]byte(cachedLocalSDP), answerDirectionFor([]byte(carrierHoldOffer)))
	require.Contains(t, string(got), "a=recvonly\r\n")
	require.NotContains(t, string(got), "a=sendrecv")
	require.Contains(t, string(got), "a=rtpmap:0 PCMU/8000\r\n")
	require.Contains(t, string(got), "a=ptime:20\r\n")
}

func TestWithSDPDirectionNoop(t *testing.T) {
	got := withSDPDirection([]byte(cachedLocalSDP), "")
	require.Equal(t, cachedLocalSDP, string(got))
}

func TestWithSDPDirectionUnhold(t *testing.T) {
	held := withSDPDirection([]byte(cachedLocalSDP), answerDirectionFor([]byte("a=sendonly\r\n")))
	resumed := withSDPDirection(held, answerDirectionFor([]byte("a=sendrecv\r\n")))
	require.Contains(t, string(resumed), "a=sendrecv\r\n")
	require.Equal(t, 1, strings.Count(string(resumed), "a=sendrecv"))
	require.NotContains(t, string(resumed), "a=recvonly")
}

func TestWithSDPDirectionAppendMissing(t *testing.T) {
	base := "v=0\r\nm=audio 1 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	got := withSDPDirection([]byte(base), "recvonly")
	require.True(t, strings.HasSuffix(string(got), "a=recvonly\r\n"))
}

func TestWithSDPDirectionCollapseDuplicates(t *testing.T) {
	dup := cachedLocalSDP + "a=sendonly\r\n"
	got := withSDPDirection([]byte(dup), "recvonly")
	require.Equal(t, 1, strings.Count(string(got), "a=recvonly"))
	require.NotContains(t, string(got), "a=sendrecv")
	require.NotContains(t, string(got), "a=sendonly")
}
