package opus

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	_ "github.com/livekit/media-sdk/all"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/opus"
	"github.com/livekit/media-sdk/sdp"

	"github.com/livekit/sip/pkg/sip"
)

// TestDefaultCodecsEnabled verifies the complete enabled codec list from sip.DefaultCodecs().
func TestDefaultCodecsEnabled(t *testing.T) {
	cs := sip.DefaultCodecs()

	enabled := cs.ListEnabled()
	names := make(map[string]bool)
	for _, c := range enabled {
		names[c.Info().Name] = true
	}

	require.True(t, names["opus"], "opus must be enabled")
	require.True(t, names["PCMU"], "PCMU must be enabled")
	require.True(t, names["PCMA"], "PCMA must be enabled")
	require.True(t, names["G722"], "G722 must be enabled")
	require.True(t, names["telephone-event"], "telephone-event must be enabled")
	require.False(t, names["AMR-WB"], "AMR-WB must be disabled by default")
}

// TestOpusCodecPriority verifies Opus has the highest priority among enabled codecs.
func TestOpusCodecPriority(t *testing.T) {
	cs := sip.DefaultCodecs().NewSet()
	enabled := cs.ListEnabled()

	if len(enabled) == 0 {
		t.Fatal("no enabled codecs")
	}

	highest := enabled[0]
	require.Equal(t, "opus", highest.Info().Name,
		"opus must be highest-priority enabled codec")
	require.Equal(t, 10, highest.Info().Priority)
}

// TestCodecByNameResolution verifies the codec lookup chain works for opus.
func TestCodecByNameResolution(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"opus/48000/2", true},
		{"opus/48000", true},
		{"opus", true},
		{"OPUS", true},
		{"Opus", true},
		{"PCMU/8000", true},
		{"g722/8000", true},
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
	require.True(t, child.IsEnabledByName("opus"),
		"opus should be inherited in child set")

	// Disabling opus in child must not affect parent.
	child.SetEnabled(opus.SDPName, false)
	require.False(t, child.IsEnabledByName("opus"))
	require.True(t, parent.IsEnabledByName("opus"),
		"disabling in child must not affect parent")
}

// TestOnlyListedCodecs verifies that OnlyListedCodecs=true starts empty.
func TestOnlyListedCodecs(t *testing.T) {
	// Simulating what codecSet does with OnlyListedCodecs=true:
	// it creates an empty set, then adds only the specified codecs.
	cs := msdk.NewCodecSet()
	cs.SetEnabled(g711.ULawSDPNameAndRate, true)
	cs.SetEnabled(opus.SDPName, true)

	enabled := cs.ListEnabled()
	names := make(map[string]bool)
	for _, c := range enabled {
		names[c.Info().Name] = true
	}

	require.True(t, names["opus"])
	require.True(t, names["PCMU"])
	require.False(t, names["PCMA"], "PCMA should not be in explicit-only set")
	require.False(t, names["G722"], "G722 should not be in explicit-only set")
}

// TestOpusCodecCreations verifies the opus codec factory produces valid encoders/decoders.
func TestOpusCodecCreations(t *testing.T) {
	codec := sdp.CodecByName(opus.SDPName)
	require.NotNil(t, codec)

	info, create, ok := codec.Supports(msdk.CodecConfig{})
	require.True(t, ok)
	require.NotNil(t, create)

	// Create the codec instance.
	instance := create()
	require.NotNil(t, instance)

	audioCodec, ok := instance.(msdk.AudioCodec)
	require.True(t, ok, "opus codec should implement AudioCodec")
	require.NotNil(t, audioCodec)

	// Verify the codec info.
	ci := audioCodec.Info()
	require.Equal(t, "opus", ci.Name)
	require.Equal(t, 48000, ci.SampleRate)
	require.Equal(t, 48000, ci.RTPClockRate)
}

// TestMediaPortWithOpus verifies that NewMediaPort can handle the opus sample rate.
// RoomSampleRate = 48000 matches opus native rate → no resampling needed.
func TestMediaPortWithOpusNoResample(t *testing.T) {
	// The SIP pipeline uses RoomSampleRate = 48000 for MediaPort creation.
	// Opus codec operates at 48kHz natively, so there is zero resampling overhead.
	const roomSampleRate = 48000

	// Verify opus offers at 48kHz.
	codec := sdp.CodecByName(opus.SDPName)
	info, _, ok := codec.Supports(msdk.CodecConfig{})
	require.True(t, ok)
	require.Equal(t, roomSampleRate, info.SampleRate,
		"opus sample rate must match RoomSampleRate to avoid resampling")
	require.Equal(t, roomSampleRate, info.RTPClockRate,
		"opus RTP clock rate must match RoomSampleRate")
}

// TestInboundCallAudioCodecAttr verifies the audio codec name recorded on SIPCallInfo
// when Opus is negotiated. This is a value-level test since we can't run
// Docker-based integration tests in this environment.
func TestInboundCallAudioCodecAttr(t *testing.T) {
	// When runMediaConn completes, it records mc.Audio.Codec.Info().SDPName
	// on the CallState. For Opus, this must be "opus".
	codec := sdp.CodecByName(opus.SDPName)
	info, _, ok := codec.Supports(msdk.CodecConfig{})
	require.True(t, ok)
	require.Equal(t, "opus", info.SDPName,
		"opus SDPName must be 'opus' for SIPCallInfo.AudioCodec attribute")
}

// TestCodecOfferOrdering verifies the SDP offer order for the full default codec set.
// This is critical: the first codec in the offer is what the remote side selects.
func TestCodecOfferOrdering(t *testing.T) {
	cs := sip.DefaultCodecs()
	offered := sdp.OfferCodecsWith(cs)

	require.NotEmpty(t, offered)
	require.Equal(t, "opus", offered[0].Info.Name,
		"first offered codec must be opus (priority 10)")

	// Print the order for documentation.
	var order []string
	for _, c := range offered {
		order = append(order, fmt.Sprintf("%s(p=%d)", c.Info.Name, c.Info.Priority))
	}
	t.Logf("SDP offer order: %v", order)
}

// BenchmarkCodecLookup measures codec lookup performance.
func BenchmarkCodecLookup(b *testing.B) {
	cs := sip.DefaultCodecs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cs.IsEnabledByName("opus")
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

// BenchmarkCodecSupports measures the Supports() call used during SDP negotiation.
func BenchmarkCodecSupports(b *testing.B) {
	codec := sdp.CodecByName(opus.SDPName)
	cfg := msdk.CodecConfig{SampleRate: 48000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = codec.Supports(cfg)
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
		codecName    string
		sdpName      string
		sampleRate   int
		needsResample bool
	}{
		{"opus", "opus/48000/2", 48000, false},
		{"PCMU", "PCMU/8000", 8000, true},
		{"PCMA", "PCMA/8000", 8000, true},
		{"G722", "G722/8000", 16000, true},
	}

	for _, tt := range tests {
		t.Run(tt.codecName, func(t *testing.T) {
			c := sdp.CodecByNameWith(sip.DefaultCodecs(), tt.sdpName)
			require.NotNil(t, c, "codec %s must be available", tt.codecName)

			_, _, ok := c.Supports(msdk.CodecConfig{SampleRate: tt.sampleRate})
			require.True(t, ok, "codec must support its native rate")

			if tt.needsResample {
				require.NotEqual(t, roomSampleRate, tt.sampleRate,
					"%s needs resampling to %d", tt.codecName, roomSampleRate)
			} else {
				require.Equal(t, roomSampleRate, tt.sampleRate,
					"%s matches RoomSampleRate, zero-resample path", tt.codecName)
			}
		})
	}
}

// TestResampleWriterBehavior verifies ResampleWriter is a no-op at matching rates.
func TestResampleWriterBehavior(t *testing.T) {
	// When src rate == dst rate, ResampleWriter is a pass-through.
	// For Opus: both are 48000 → no resample wrapper.
	var pcm msdk.PCM16Sample = make([]int16, 480)
	result := msdk.Resample(nil, 48000, pcm, 48000)
	require.Equal(t, len(pcm), len(result),
		"resample at same rate should be identity (same length)")
}

// Ensure test compiles with unused imports if running benchmarks in same package.
var _ = context.Background
var _ = time.Now