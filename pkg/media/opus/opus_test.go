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

package opus

import (
	"testing"

	"github.com/stretchr/testify/require"
	hopus "gopkg.in/hraban/opus.v2"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
)

// A single Opus packet longer than our 20 ms ptime (here 60 ms) must decode
// without overflowing the decode buffer.
func TestDecodeLongFrame(t *testing.T) {
	const rate = 48000
	// 60 ms mono frame = 2880 samples, larger than one 20 ms frame (960).
	const frameSamples = rate / 1000 * 60

	enc, err := hopus.NewEncoder(rate, 1, hopus.AppVoIP)
	require.NoError(t, err)

	pcmIn := make([]int16, frameSamples)
	for i := range pcmIn {
		pcmIn[i] = int16(3000 * (i % 100))
	}
	pkt := make([]byte, 4000)
	n, err := enc.Encode(pcmIn, pkt)
	require.NoError(t, err)
	pkt = pkt[:n]

	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, rate)
	dec, err := DecodeWith(sink, 1, logger.GetLogger())
	require.NoError(t, err)

	require.NoError(t, dec.WriteSample(pkt))
	require.Equal(t, frameSamples, len(out), "60ms packet should decode to 2880 mono samples")
}

// Encoder options must be applied without error and produce decodable output.
func TestEncodeWithOptionsRoundTrip(t *testing.T) {
	const rate = 48000
	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, rate)

	dec, err := DecodeWith(sink, 1, logger.GetLogger())
	require.NoError(t, err)
	enc, err := EncodeWith(dec, EncodeOptions{
		Channels:          1,
		Bitrate:           16000,
		Complexity:        5,
		FEC:               true,
		PacketLossPercent: 10,
	}, logger.GetLogger())
	require.NoError(t, err)

	in := make(msdk.PCM16Sample, rate/50) // one 20ms frame
	for i := range in {
		in[i] = int16(2000 * (i % 50))
	}
	require.NoError(t, enc.WriteSample(in))
	require.NoError(t, enc.Close())
	require.NotEmpty(t, out)
}

// An invalid channel count must be rejected by Decode.
func TestDecodeRejectsBadChannels(t *testing.T) {
	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, 48000)
	_, err := DecodeWith(sink, 3, logger.GetLogger())
	require.Error(t, err)
}

// sampleCollector is a WriteCloser[Sample] sink that counts encoded packets and
// total bytes, for asserting on encoder output.
type sampleCollector struct {
	rate    int
	bytes   int
	packets int
}

func (c *sampleCollector) String() string  { return "collector" }
func (c *sampleCollector) SampleRate() int { return c.rate }
func (c *sampleCollector) Close() error    { return nil }
func (c *sampleCollector) WriteSample(s Sample) error {
	c.bytes += len(s)
	c.packets++
	return nil
}

// DTX (discontinuous transmission) silence packets must not error the decoder:
// empty "no data" frames are skipped, and the tiny packets libopus emits for
// silence decode cleanly into PCM.
func TestDTXPacketsNoError(t *testing.T) {
	const rate = 48000
	const frame = rate / 50 // 20ms mono

	enc, err := hopus.NewEncoder(rate, 1, hopus.AppVoIP)
	require.NoError(t, err)
	require.NoError(t, enc.SetDTX(true))

	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, rate)
	dec, err := DecodeWith(sink, 1, logger.GetLogger())
	require.NoError(t, err)

	// An explicit "no data" gap (e.g. DTX with nothing to send) is a no-op.
	require.NoError(t, dec.WriteSample(Sample(nil)))
	require.NoError(t, dec.WriteSample(Sample{}))
	require.Empty(t, out, "no-data frames must not produce PCM")

	// Encode many frames of silence with DTX on; libopus emits short (1-2 byte)
	// DTX packets once it detects sustained silence. Each must decode cleanly.
	silence := make([]int16, frame)
	buf := make([]byte, 4000)
	sawDTX := false
	decodedFrames := 0
	for i := 0; i < 50; i++ {
		n, err := enc.Encode(silence, buf)
		require.NoError(t, err)
		if n > 0 && n <= 2 {
			sawDTX = true // tiny packet == DTX/comfort-noise frame
		}
		before := len(out)
		require.NoError(t, dec.WriteSample(Sample(buf[:n])))
		if len(out) > before {
			decodedFrames++
		}
	}
	require.True(t, sawDTX, "DTX should have produced at least one tiny packet")
	require.Positive(t, decodedFrames, "DTX/silence packets should decode to PCM")
}

// Enabling in-band FEC must take effect end-to-end: it changes the encoded
// bitstream (vs FEC off) and every packet still decodes without being dropped.
func TestFECPassthrough(t *testing.T) {
	const rate = 48000
	const frame = rate / 50 // 20ms mono
	const frames = 20

	in := make(msdk.PCM16Sample, frame)
	for i := range in {
		in[i] = int16(4000 * (i % 80))
	}

	encodeAll := func(t *testing.T, opts EncodeOptions) *sampleCollector {
		t.Helper()
		c := &sampleCollector{rate: rate}
		enc, err := EncodeWith(c, opts, logger.GetLogger())
		require.NoError(t, err)
		for i := 0; i < frames; i++ {
			require.NoError(t, enc.WriteSample(in))
		}
		require.NoError(t, enc.Close())
		return c
	}

	// VBR (no fixed bitrate). Enabling FEC + packet-loss tuning makes libopus
	// re-budget the bitstream (redundant LBRR data, altered frame sizes), so the
	// encoded output must differ from the FEC-off stream — proving our FEC
	// option actually reaches the encoder.
	off := encodeAll(t, EncodeOptions{Channels: 1})
	on := encodeAll(t, EncodeOptions{Channels: 1, FEC: true, PacketLossPercent: 30})

	require.Equal(t, frames, off.packets, "no packets dropped without FEC")
	require.Equal(t, frames, on.packets, "no packets dropped with FEC")
	require.NotEqual(t, off.bytes, on.bytes, "enabling in-band FEC must change the encoded bitstream")

	// And the FEC stream decodes fully (passthrough, no drops).
	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, rate)
	dec, err := DecodeWith(sink, 1, logger.GetLogger())
	require.NoError(t, err)
	enc, err := EncodeWith(dec, EncodeOptions{Channels: 1, FEC: true, PacketLossPercent: 30}, logger.GetLogger())
	require.NoError(t, err)
	for i := 0; i < frames; i++ {
		require.NoError(t, enc.WriteSample(in))
	}
	require.NoError(t, enc.Close())
	require.Equal(t, frames*frame, len(out))
}
