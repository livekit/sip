// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build cgo

package sip

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/sdp"
)

// enableOpusForTest turns Opus on for a test and restores disabled state after.
func enableOpusForTest(t *testing.T) {
	t.Helper()
	SetOpusEnabled(true)
	t.Cleanup(func() { SetOpusEnabled(false) })
}

// TestOpusDisabledByDefault verifies that without calling SetOpusEnabled,
// Opus is absent from defaultCodecs — so existing deployments are unaffected.
func TestOpusDisabledByDefault(t *testing.T) {
	c := sdp.CodecByNameWith(defaultCodecs, OpusSDPName)
	require.Nil(t, c, "opus must not appear in defaultCodecs by default")
}

// TestOpusRegistered verifies the codec is present in msdk.Codecs(), uses a
// dynamic payload type, and runs at the correct 48 kHz clock rate.
func TestOpusRegistered(t *testing.T) {
	enableOpusForTest(t)

	c := sdp.CodecByNameWith(defaultCodecs, OpusSDPName)
	require.NotNil(t, c, "opus codec must be present in defaultCodecs when enabled")

	_, ok := c.(msdk.AudioCodec)
	require.True(t, ok, "opus codec must implement AudioCodec")

	info := c.Info()
	require.Equal(t, OpusSDPName, info.SDPName)
	require.Equal(t, 48000, info.SampleRate)
	require.Equal(t, 48000, info.RTPClockRate)
	require.False(t, info.RTPIsStatic, "opus must use a dynamic payload type")
}

// TestOpusInSDPOffer verifies that after enabling Opus, an SDP offer contains
// an rtpmap line advertising opus/48000/2.
func TestOpusInSDPOffer(t *testing.T) {
	enableOpusForTest(t)

	_, md, err := sdp.OfferMediaWith(defaultCodecs, 12345, sdp.EncryptionNone)
	require.NoError(t, err)

	var found bool
	for _, a := range md.Attributes {
		if a.Key == "rtpmap" && strings.Contains(strings.ToLower(a.Value), "opus/48000/2") {
			found = true
			break
		}
	}
	require.True(t, found, "SDP offer should contain an rtpmap line for opus/48000/2")
}

// TestOpusPreferredOverG722 verifies codec selection picks Opus (priority 10)
// over G722 (priority -5) and G711 (priority -10/-20) when all are offered.
func TestOpusPreferredOverG722(t *testing.T) {
	enableOpusForTest(t)

	opusC, ok := sdp.CodecByNameWith(defaultCodecs, OpusSDPName).(msdk.AudioCodec)
	require.True(t, ok, "opus must be an AudioCodec")

	ulawC, ok := sdp.CodecByNameWith(defaultCodecs, g711.ULawSDPName).(msdk.AudioCodec)
	require.True(t, ok, "PCMU must be an AudioCodec")

	g722C, ok := sdp.CodecByNameWith(defaultCodecs, g722.SDPName).(msdk.AudioCodec)
	require.True(t, ok, "G722 must be an AudioCodec")

	desc := sdp.MediaDesc{
		Codecs: []sdp.CodecInfo{
			{Type: 0, Codec: ulawC},
			{Type: 9, Codec: g722C},
			{Type: 111, Codec: opusC},
		},
	}
	got, err := sdp.SelectAudio(desc, false)
	require.NoError(t, err)
	require.Equal(t, OpusSDPName, got.Codec.Info().SDPName,
		"Opus should win priority-based codec selection")
	require.Equal(t, byte(111), got.Type,
		"peer-assigned payload type 111 must be honored")
}
