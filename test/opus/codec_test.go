package opus

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	_ "github.com/livekit/media-sdk/all"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/sdp"

	"github.com/livekit/sip/pkg/sip"
)

const (
	// opusSDPName is the canonical Opus SDP name per RFC 7587 §6.1.
	opusSDPName = "opus/48000/2"
	// Bare names as they appear in SIPCodec config.
	opusBareName  = "opus"
	g722BareName  = "g722"
	pcmuFullName  = "PCMU/8000"
	pcmaFullName  = "PCMA/8000"
	dtmfFullName  = "telephone-event/8000"
	amrwbFullName = "AMR-WB/16000"
)

// TestDefaultCodecsEnabled verifies the complete enabled codec list from sip.DefaultCodecs().
func TestDefaultCodecsEnabled(t *testing.T) {
	cs := sip.DefaultCodecs()

	enabled := cs.ListEnabled()
	names := make(map[string]bool)
	for _, c := range enabled {
		names[c.Info().SDPName()] = true
	}

	require.True(t, names[opusBareName], "opus must be enabled")
	require.True(t, names["PCMU"], "PCMU must be enabled")
	require.True(t, names["PCMA"], "PCMA must be enabled")
	require.True(t, names["G722"], "G722 must be enabled")
	require.True(t, names["telephone-event"], "telephone-event must be enabled")
	require.False(t, names["AMR-WB"], "AMR-WB must be disabled by default")
}

// TestOpusCodecPriority verifies Opus is offered first among enabled codecs.
func TestOpusCodecPriority(t *testing.T) {
	offered := sdp.OfferCodecsWith(sip.DefaultCodecs())

	require.NotEmpty(t, offered)
	highest := offered[0].Info
	require.Equal(t, opusSDPName, highest.SDPFullName(),
		"opus must be highest-priority offered codec")
	require.Equal(t, 10, highest.Priority)
}

// TestCodecByNameResolution verifies the codec lookup chain works for opus.
//
// The media-sdk resolves lookups by the bare codec name (the part before the
// first '/'), lower-cased: every rate/channel spelling of opus resolves to
// the registered "opus" codec.
func TestCodecByNameResolution(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"opus/48000/2", true},
		{"OPUS/48000/2", true},
		{"opus/48000", true},
		{opusBareName, true},
		{pcmuFullName, true},
		{g722BareName + "/8000", true},
		{"unknown-codec", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			c := sdp.CodecByNameWith(sip.DefaultCodecs(), tt.input)
			if tt.ok {
				require.NotNil(t, c, "codec should be found: %s", tt.input)
			} else {
				require.Nil(t, c, "codec should not be found: %s", tt.input)
			}
		})
	}
}

// TestNewSetInheritsOpus verifies that NewSet() inherits opus from parent.
func TestNewSetInheritsOpus(t *testing.T) {
	parent := sip.DefaultCodecs()
	child := parent.NewSet()

	// Child must inherit opus from parent.
	require.True(t, child.IsEnabledByName(opusSDPName),
		"opus should be inherited in child set")

	// Disabling opus in child must not affect parent.
	child.SetEnabled(opusSDPName, false)
	require.False(t, child.IsEnabledByName(opusSDPName))
	require.True(t, parent.IsEnabledByName(opusSDPName),
		"disabling in child must not affect parent")
}

// TestOnlyListedCodecs verifies that OnlyListedCodecs=true starts empty.
func TestOnlyListedCodecs(t *testing.T) {
	// Simulating what codecSet does with OnlyListedCodecs=true:
	// it creates an empty set, then adds only the specified codecs.
	cs := msdk.NewCodecSet()
	cs.SetEnabled(g711.ULawSDPNameAndRate, true)
	cs.SetEnabled(opusSDPName, true)

	enabled := cs.ListEnabled()
	names := make(map[string]bool)
	for _, c := range enabled {
		names[c.Info().SDPName()] = true
	}

	require.True(t, names[opusBareName])
	require.True(t, names["PCMU"])
	require.False(t, names["PCMA"], "PCMA should not be in explicit-only set")
	require.False(t, names["G722"], "G722 should not be in explicit-only set")
}

// TestOpusCodecInfo verifies the registered Opus codec matches RFC 7587 §6.1:
// RTP clock rate is always 48000 Hz, stereo is expressed via channels=2.
func TestOpusCodecInfo(t *testing.T) {
	codec := sdp.CodecByNameWith(sip.DefaultCodecs(), opusSDPName)
	require.NotNil(t, codec)

	require.Equal(t, "opus", codec.Info().SDPName())
	require.Equal(t, 10, codec.Info().Priority)
	require.False(t, codec.Info().RTPIsStatic, "opus has no static RTP payload type")

	// Full configuration (sample rate / RTP clock rate) lives on the offered
	// CodecInfo, not on the basic CodecTypeInfo.
	offer := codec.Offer(nil)
	require.NotEmpty(t, offer)
	info := offer[0]
	require.Equal(t, opusSDPName, info.SDPFullName())
	require.Equal(t, 48000, info.SampleRate)
	require.Equal(t, 48000, info.RTPClockRate, "RTP clock rate must be 48kHz per RFC 7587")
}

// TestMediaPortWithOpusNoResample verifies that the media pipeline can run at
// the opus native rate. RoomSampleRate = 48000 matches opus → no resampling.
func TestMediaPortWithOpusNoResample(t *testing.T) {
	// The SIP pipeline uses RoomSampleRate = 48000 for MediaPort creation.
	// Opus codec operates at 48kHz natively, so there is zero resampling overhead.
	const roomSampleRate = 48000

	codec := sdp.CodecByNameWith(sip.DefaultCodecs(), opusSDPName)
	require.NotNil(t, codec)
	offer := codec.Offer(nil)
	require.NotEmpty(t, offer)
	info := offer[0]
	require.Equal(t, roomSampleRate, info.SampleRate,
		"opus sample rate must match RoomSampleRate to avoid resampling")
	require.Equal(t, roomSampleRate, info.RTPClockRate,
		"opus RTP clock rate must match RoomSampleRate")
}

// TestInboundCallAudioCodecAttr verifies the audio codec name recorded on SIPCallInfo
// when Opus is negotiated (media_pipeline records Info.SDPFullName()).
func TestInboundCallAudioCodecAttr(t *testing.T) {
	codec := sdp.CodecByNameWith(sip.DefaultCodecs(), opusSDPName)
	require.NotNil(t, codec)
	offer := codec.Offer(nil)
	require.NotEmpty(t, offer)
	require.Equal(t, opusSDPName, offer[0].SDPFullName(),
		"opus SDP full name must be reported for SIPCallInfo.AudioCodec attribute")
}

// TestCodecOfferOrdering verifies the SDP offer order for the full default codec set.
// This is critical: the first codec in the offer is what the remote side selects.
func TestCodecOfferOrdering(t *testing.T) {
	cs := sip.DefaultCodecs()
	offered := sdp.OfferCodecsWith(cs)

	require.NotEmpty(t, offered)
	require.Equal(t, opusSDPName, offered[0].Info.SDPFullName(),
		"first offered codec must be opus (priority 10)")

	// Print the order for documentation.
	var order []string
	for _, c := range offered {
		order = append(order, fmt.Sprintf("%s(p=%d)", c.Info.SDPFullName(), c.Info.Priority))
	}
	t.Logf("SDP offer order: %v", order)
}

// BenchmarkCodecLookup measures codec lookup performance.
func BenchmarkCodecLookup(b *testing.B) {
	cs := sip.DefaultCodecs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cs.IsEnabledByName(opusSDPName)
	}
}

// BenchmarkCodecList measures ListEnabled performance.
func BenchmarkCodecList(b *testing.B) {
	cs := sip.DefaultCodecs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cs.ListEnabled()
	}
}

// BenchmarkOfferCodecGeneration measures full SDP offer codec generation.
func BenchmarkOfferCodecGeneration(b *testing.B) {
	cs := sip.DefaultCodecs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sdp.OfferCodecsWith(cs)
	}
}

// TestNoResamplePathWithOpus documents that Opus → LiveKit room is a zero-resample
// path: RoomSampleRate (48000) == opus SampleRate (48000).
//
// This is a significant performance win over G.711 (8000 Hz) which requires
// resampling to 48000 Hz, and G.722 (16000 Hz actual) which also requires it.
func TestNoResamplePathWithOpus(t *testing.T) {
	const roomSampleRate = 48000 // from media.go:RoomSampleRate

	tests := []struct {
		codecName     string
		sdpName       string
		sampleRate    int
		needsResample bool
	}{
		{"opus", opusSDPName, 48000, false},
		{"PCMU", pcmuFullName, 8000, true},
		{"PCMA", pcmaFullName, 8000, true},
		{"G722", "G722/8000", 16000, true},
	}

	for _, tt := range tests {
		t.Run(tt.codecName, func(t *testing.T) {
			c := sdp.CodecByNameWith(sip.DefaultCodecs(), tt.sdpName)
			require.NotNil(t, c, "codec %s must be available", tt.codecName)

			offer := c.Offer(nil)
			require.NotEmpty(t, offer)
			info := offer[0]
			require.Equal(t, tt.sampleRate, info.SampleRate, "codec must support its native rate")

			if tt.needsResample {
				require.NotEqual(t, roomSampleRate, info.SampleRate,
					"%s needs resampling to %d", tt.codecName, roomSampleRate)
			} else {
				require.Equal(t, roomSampleRate, info.SampleRate,
					"%s matches RoomSampleRate, zero-resample path", tt.codecName)
			}
		})
	}
}

// TestResampleWriterBehavior verifies Resample is a no-op at matching rates.
func TestResampleWriterBehavior(t *testing.T) {
	// When src rate == dst rate, Resample is a pass-through.
	// For Opus: both are 48000 → no resample wrapper.
	var pcm msdk.PCM16Sample = make([]int16, 480)
	result := msdk.Resample(nil, 48000, pcm, 48000)
	require.Equal(t, len(pcm), len(result),
		"resample at same rate should be identity (same length)")
}
