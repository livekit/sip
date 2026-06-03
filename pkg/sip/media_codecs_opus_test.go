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
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/amrwb"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/audiotest"
	"github.com/livekit/sip/pkg/media/opus"
)

// enableOpusForTest turns Opus on for the duration of a test and restores the
// default (disabled) state afterwards, keeping global codec state isolated.
func enableOpusForTest(t *testing.T) {
	t.Helper()
	SetOpusEnabled(true)
	t.Cleanup(func() { SetOpusEnabled(false) })
}

// Opus must be registered as an AudioCodec, use a dynamic payload type, run at
// its native 48 kHz clock, and become available once enabled.
func TestOpusRegisteredAndEnabled(t *testing.T) {
	enableOpusForTest(t)

	c := sdp.CodecByNameWith(defaultCodecs, OpusSDPName)
	require.NotNil(t, c, "opus codec should be available in defaultCodecs once enabled")

	_, ok := c.(msdk.AudioCodec)
	require.True(t, ok, "opus codec should implement AudioCodec")

	info := c.Info()
	require.Equal(t, OpusSDPName, info.SDPName)
	require.Equal(t, OpusSampleRate, info.SampleRate)
	require.Equal(t, OpusSampleRate, info.RTPClockRate)
	require.False(t, info.RTPIsStatic, "opus must use a dynamic payload type")
}

// The generated SDP offer must advertise opus/48000/2 (RFC 7587) with a dynamic
// payload type in the dynamic range.
func TestOpusInSDPOffer(t *testing.T) {
	enableOpusForTest(t)

	_, md, err := sdp.OfferMediaWith(defaultCodecs, 12345, sdp.EncryptionNone)
	require.NoError(t, err)

	var rtpmap string
	for _, a := range md.Attributes {
		if a.Key == "rtpmap" && strings.Contains(strings.ToLower(a.Value), OpusSDPName) {
			rtpmap = a.Value
		}
	}
	require.NotEmpty(t, rtpmap, "SDP offer should advertise opus/48000/2")

	// rtpmap value looks like "<pt> opus/48000/2"; the payload type must be dynamic (96-127).
	pt := strings.SplitN(rtpmap, " ", 2)[0]
	require.Contains(t, md.MediaName.Formats, pt, "opus payload type must appear in m= formats")
}

// When a peer offers Opus alongside lower-priority codecs, we must select Opus
// and honor the payload type the peer assigned (e.g. 111).
func TestOpusPreferredInSelection(t *testing.T) {
	enableOpusForTest(t)

	opusC := sdp.CodecByNameWith(defaultCodecs, OpusSDPName).(msdk.AudioCodec)
	ulawC := sdp.CodecByNameWith(defaultCodecs, g711.ULawSDPName).(msdk.AudioCodec)
	g722C := sdp.CodecByNameWith(defaultCodecs, g722.SDPName).(msdk.AudioCodec)
	require.NotNil(t, opusC)
	require.NotNil(t, ulawC)
	require.NotNil(t, g722C)

	desc := sdp.MediaDesc{
		Codecs: []sdp.CodecInfo{
			{Type: 0, Codec: ulawC},
			{Type: 9, Codec: g722C},
			{Type: 111, Codec: opusC},
		},
	}
	got, err := sdp.SelectAudio(desc, false)
	require.NoError(t, err)
	require.Equal(t, OpusSDPName, got.Codec.Info().SDPName, "opus should win priority-based selection")
	require.Equal(t, byte(111), got.Type, "peer's opus payload type must be honored")
}

// A PCM signal encoded to Opus and decoded back should round-trip to a similar
// length of non-silent PCM, exercising encoder options and the dynamic-channel
// decoder. enc -> opus.Sample -> dec -> PCM16.
func TestOpusEncodeDecodeRoundTrip(t *testing.T) {
	log := logger.GetLogger()

	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, OpusSampleRate)

	dec, err := opus.Decode(sink, opusChannels, log)
	require.NoError(t, err)
	enc, err := opus.EncodeWith(dec, opus.EncodeOptions{
		Channels:          1,
		Bitrate:           24000,
		Complexity:        10,
		FEC:               true,
		PacketLossPercent: 5,
	}, log)
	require.NoError(t, err)

	// 200 ms of 48 kHz mono = 9600 samples = exactly 10 Opus frames of 20 ms.
	const n = OpusSampleRate / 5
	in := make(msdk.PCM16Sample, n)
	audiotest.GenSignal(in, []audiotest.Wave{{Ind: 5, Amp: 8000}})

	require.NoError(t, enc.WriteSample(in))
	require.NoError(t, enc.Close())

	require.NotEmpty(t, out, "decoded PCM should not be empty")
	// Each Opus frame decodes back to 960 samples; allow generous tolerance for
	// codec delay/framing.
	require.InDelta(t, n, len(out), float64(n)/2)

	// Output must carry actual signal energy, not silence.
	var energy int64
	for _, s := range out {
		energy += int64(s) * int64(s)
	}
	require.Greater(t, energy, int64(0), "decoded audio should contain signal energy")
}

// SetOpusOptions always forces mono, regardless of the channel count requested.
func TestSetOpusOptionsForcesMono(t *testing.T) {
	t.Cleanup(func() { opusOptions.Store(nil) })
	SetOpusOptions(opus.EncodeOptions{Channels: 2, Bitrate: 32000})
	got := currentOpusOptions()
	require.Equal(t, opusChannels, got.Channels)
	require.Equal(t, 32000, got.Bitrate)
}

// offerRtpmaps returns the rtpmap attribute values of an SDP offer built from
// the given codec set.
func offerRtpmaps(t *testing.T, codecs *msdk.CodecSet) []string {
	t.Helper()
	_, md, err := sdp.OfferMediaWith(codecs, 12345, sdp.EncryptionNone)
	require.NoError(t, err)
	var out []string
	for _, a := range md.Attributes {
		if a.Key == "rtpmap" {
			out = append(out, strings.ToLower(a.Value))
		}
	}
	return out
}

// With enable_opus unset (the default), Opus must not appear in SDP offers.
func TestOpusDisabledByDefault(t *testing.T) {
	// Be robust against test ordering: explicitly assert the default-off state.
	SetOpusEnabled(false)

	for _, m := range offerRtpmaps(t, defaultCodecs) {
		require.NotContains(t, m, "opus", "Opus must not be offered when disabled")
	}

	// Selecting from our offered set must yield a non-Opus codec.
	desc, _, err := sdp.OfferMediaWith(defaultCodecs, 12345, sdp.EncryptionNone)
	require.NoError(t, err)
	audio, err := sdp.SelectAudio(desc, false)
	require.NoError(t, err)
	require.NotEqual(t, OpusSDPName, audio.Codec.Info().SDPName)
}

// answerCodecFor simulates a peer that offers only the given codec set and
// returns the codec our side negotiates in the SDP answer.
func answerCodecFor(t *testing.T, callerCodecs, ourCodecs *msdk.CodecSet) (*sdp.AudioConfig, error) {
	t.Helper()
	ip := netip.MustParseAddr("1.2.3.4")
	offer, err := sdp.NewOfferWith(callerCodecs, ip, 10000, sdp.EncryptionNone)
	require.NoError(t, err)
	data, err := offer.SDP.Marshal()
	require.NoError(t, err)

	parsed, err := sdp.ParseOfferWith(ourCodecs, data)
	require.NoError(t, err)
	_, mc, err := parsed.Answer(ip, 20000, sdp.EncryptionNone)
	if err != nil {
		return nil, err
	}
	return &mc.Audio, nil
}

// When Opus is disabled, a caller that offers BOTH Opus and PCMU must be
// answered with PCMU — i.e. we actively exclude Opus from selection, not merely
// happen to pick PCMU because nothing else was offered.
func TestG711FallbackWhenOpusDisabled(t *testing.T) {
	SetOpusEnabled(false)

	caller := msdk.NewCodecSet()
	caller.SetEnabled(g711.ULawSDPName, true)
	caller.SetEnabled(OpusSDPName, true) // caller advertises Opus...

	audio, err := answerCodecFor(t, caller, defaultCodecs)
	require.NoError(t, err)
	// ...but we must not select it while disabled.
	require.Equal(t, g711.ULawSDPName, audio.Codec.Info().SDPName)
}

// With Opus enabled on our side, a caller offering only PCMU must fall back to
// G.711 cleanly — we never select a codec the peer did not offer. A positive
// control confirms the negotiation is real (Opus is chosen when offered).
func TestG711FallbackWhenCallerNoOpus(t *testing.T) {
	enableOpusForTest(t) // Opus enabled on our side

	// Positive control: when the caller offers Opus, we negotiate Opus.
	withOpus := msdk.NewCodecSet()
	withOpus.SetEnabled(g711.ULawSDPName, true)
	withOpus.SetEnabled(OpusSDPName, true)
	audio, err := answerCodecFor(t, withOpus, defaultCodecs)
	require.NoError(t, err)
	require.Equal(t, OpusSDPName, audio.Codec.Info().SDPName, "sanity: Opus is negotiated when the caller offers it")

	// A PCMU-only caller must fall back to PCMU, not a phantom Opus selection.
	ulawOnly := msdk.NewCodecSet()
	ulawOnly.SetEnabled(g711.ULawSDPName, true)
	audio, err = answerCodecFor(t, ulawOnly, defaultCodecs)
	require.NoError(t, err)
	require.Equal(t, g711.ULawSDPName, audio.Codec.Info().SDPName)
}

// callerOffering builds a single-codec caller set for scenario tests.
func callerOffering(names ...string) *msdk.CodecSet {
	s := msdk.NewCodecSet()
	for _, n := range names {
		s.SetEnabled(n, true)
	}
	return s
}

// Existing-call regression scenarios: with Opus DISABLED (the default), each
// legacy codec must negotiate exactly as before — correct codec + static
// payload type — and Opus must never be selected.
func TestLegacyCodecScenariosOpusDisabled(t *testing.T) {
	SetOpusEnabled(false)

	cases := []struct {
		name    string
		offer   *msdk.CodecSet
		wantSDP string
		wantPT  byte // RFC 3551 static payload type
	}{
		{"PCMU inbound", callerOffering(g711.ULawSDPName), g711.ULawSDPName, 0},
		{"PCMA inbound", callerOffering(g711.ALawSDPName), g711.ALawSDPName, 8},
		{"G722 inbound", callerOffering(g722.SDPName), g722.SDPName, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			audio, err := answerCodecFor(t, c.offer, defaultCodecs)
			require.NoError(t, err, "negotiation must succeed")
			require.Equal(t, c.wantSDP, audio.Codec.Info().SDPName, "correct codec selected")
			require.Equal(t, c.wantPT, audio.Type, "correct static payload type")
			require.NotEqual(t, OpusSDPName, audio.Codec.Info().SDPName, "Opus must not be touched")
		})
	}
}

// DTMF (telephone-event) must still be negotiated alongside a voice codec.
func TestDTMFNegotiatedOpusDisabled(t *testing.T) {
	SetOpusEnabled(false)

	caller := callerOffering(g711.ULawSDPName, dtmf.SDPName)
	audio, err := answerCodecFor(t, caller, defaultCodecs)
	require.NoError(t, err)
	require.Equal(t, g711.ULawSDPName, audio.Codec.Info().SDPName)
	require.NotZero(t, audio.DTMFType, "DTMF (telephone-event) must be negotiated")
}

// Outbound: with Opus disabled, our SDP offer must advertise the legacy codecs
// and must NOT include Opus.
func TestOutboundOfferExcludesOpusWhenDisabled(t *testing.T) {
	SetOpusEnabled(false)

	maps := offerRtpmaps(t, defaultCodecs)
	joined := strings.Join(maps, "\n")
	require.NotContains(t, joined, "opus", "offer must not include Opus when disabled")
	require.Contains(t, joined, strings.ToLower(g711.ULawSDPName), "offer must still include PCMU")
	require.Contains(t, joined, strings.ToLower(g711.ALawSDPName), "offer must still include PCMA")
	require.Contains(t, joined, strings.ToLower(g722.SDPName), "offer must still include G722")
}

// A call whose only offered codec we do not support must fail gracefully
// (an error, not a panic) — same behavior as before Opus existed.
func TestNoCommonCodecFailsGracefully(t *testing.T) {
	SetOpusEnabled(false)

	// AMR-WB is disabled by default; a caller offering only AMR-WB shares no
	// codec with us.
	caller := callerOffering(amrwb.SDPName)
	_, err := answerCodecFor(t, caller, defaultCodecs)
	require.Error(t, err, "no common codec must return an error, not panic")
}
