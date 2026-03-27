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

package signaling

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSDP_VideoAndAudio(t *testing.T) {
	sdp := "v=0\r\n" +
		"o=- 123456 1 IN IP4 192.168.1.100\r\n" +
		"s=Test\r\n" +
		"c=IN IP4 192.168.1.100\r\n" +
		"t=0 0\r\n" +
		"m=video 49170 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 profile-level-id=42e01f;packetization-mode=1\r\n" +
		"a=sendrecv\r\n" +
		"m=audio 49172 RTP/AVP 0 101\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\n" +
		"a=fmtp:101 0-16\r\n" +
		"a=sendrecv\r\n"

	parsed, err := ParseSDP(sdp)
	require.NoError(t, err)

	assert.Equal(t, "192.168.1.100", parsed.ConnectionAddr)
	require.Len(t, parsed.Media, 2)

	// Video
	video := parsed.VideoMedia()
	require.NotNil(t, video)
	assert.Equal(t, "video", video.MediaType)
	assert.Equal(t, 49170, video.Port)
	assert.Equal(t, uint8(96), video.PayloadType)
	assert.Equal(t, "H264", video.CodecName)
	assert.Equal(t, 90000, video.ClockRate)
	assert.Contains(t, video.Fmtp, "42e01f")

	// Audio
	audio := parsed.AudioMedia()
	require.NotNil(t, audio)
	assert.Equal(t, "audio", audio.MediaType)
	assert.Equal(t, 49172, audio.Port)
	assert.Equal(t, uint8(0), audio.PayloadType)
	assert.Equal(t, "PCMU", audio.CodecName)
	assert.Equal(t, 8000, audio.ClockRate)
}

func TestParseSDP_Negotiate(t *testing.T) {
	sdp := "v=0\r\n" +
		"o=- 1 1 IN IP4 10.0.0.5\r\n" +
		"c=IN IP4 10.0.0.5\r\n" +
		"t=0 0\r\n" +
		"m=video 5004 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"m=audio 5006 RTP/AVP 8\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n"

	parsed, err := ParseSDP(sdp)
	require.NoError(t, err)

	nm, err := parsed.Negotiate()
	require.NoError(t, err)

	assert.Equal(t, uint8(96), nm.VideoPayloadType)
	assert.Equal(t, "H264", nm.VideoCodec)
	assert.Equal(t, 90000, nm.VideoClockRate)

	assert.Equal(t, uint8(8), nm.AudioPayloadType)
	assert.Equal(t, "PCMA", nm.AudioCodec)
	assert.Equal(t, 8000, nm.AudioClockRate)

	assert.Equal(t, netip.MustParseAddr("10.0.0.5"), nm.RemoteAddr.Addr())
	assert.Equal(t, uint16(5004), nm.RemoteAddr.Port())
}

func TestParseSDP_NoVideo(t *testing.T) {
	sdp := "v=0\r\n" +
		"c=IN IP4 10.0.0.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 5000 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n"

	parsed, err := ParseSDP(sdp)
	require.NoError(t, err)

	assert.Nil(t, parsed.VideoMedia())
	assert.NotNil(t, parsed.AudioMedia())

	_, err = parsed.Negotiate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no video")
}

func TestBuildVideoSDP(t *testing.T) {
	ip := netip.MustParseAddr("192.168.1.50")
	sdp := BuildVideoSDP(ip, 20000, 20002, "42e01f")

	assert.Contains(t, sdp, "v=0\r\n")
	assert.Contains(t, sdp, "192.168.1.50")
	assert.Contains(t, sdp, "m=video 20000 RTP/AVP 96")
	assert.Contains(t, sdp, "a=rtpmap:96 H264/90000")
	assert.Contains(t, sdp, "profile-level-id=42e01f")
	assert.Contains(t, sdp, "packetization-mode=1")
	assert.Contains(t, sdp, "m=audio 20002 RTP/AVP 0 8 101")
	assert.Contains(t, sdp, "a=rtpmap:0 PCMU/8000")
	assert.Contains(t, sdp, "a=rtpmap:101 telephone-event/8000")
}

func TestBuildVideoSDP_DefaultProfile(t *testing.T) {
	ip := netip.MustParseAddr("10.0.0.1")
	sdp := BuildVideoSDP(ip, 30000, 30002, "")

	// Should default to 42e01f
	assert.Contains(t, sdp, "profile-level-id=42e01f")
}
