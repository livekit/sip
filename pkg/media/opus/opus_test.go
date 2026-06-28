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

// Closing the encoder with a non-frame-aligned tail must flush a final packet
// (zero-padded up to a full frame) instead of erroring on a partial frame.
func TestOpusFlushPadsPartialFrame(t *testing.T) {
	const rate = 48000

	c := &sampleCollector{rate: rate}
	enc, err := EncodeWith(c, EncodeOptions{Channels: 1}, logger.GetLogger())
	require.NoError(t, err)

	// 1000 samples is not a multiple of one 20ms frame (960 @ 48kHz): one full
	// frame is emitted immediately, leaving 40 samples buffered.
	in := make(msdk.PCM16Sample, 1000)
	for i := range in {
		in[i] = int16(4000 * (i % 50))
	}
	require.NoError(t, enc.WriteSample(in))
	require.Equal(t, 1, c.packets, "one full frame should be emitted before close")

	// Close must flush the 40-sample remainder as a zero-padded final frame.
	require.NoError(t, enc.Close())
	require.Equal(t, 2, c.packets, "Close must flush the padded trailing partial frame")
	require.Positive(t, c.bytes, "flushed frame must carry encoded bytes")
}

// A stereo Opus packet decoded with a mono target must be downmixed to a single
// mono frame (channel-adaptive decode), and the result must be non-silent.
func TestOpusDecodeStereoToMono(t *testing.T) {
	const (
		rate       = 48000
		perChannel = rate / 50 // 960 samples/channel = 20ms
	)

	// Build a genuine stereo Opus packet with a raw 2-channel encoder.
	enc, err := hopus.NewEncoder(rate, 2, hopus.AppAudio)
	require.NoError(t, err)
	stereo := make([]int16, perChannel*2) // interleaved L/R
	for i := 0; i < perChannel; i++ {
		v := int16(5000 * (i % 60))
		stereo[2*i] = v   // L
		stereo[2*i+1] = v // R
	}
	pkt := make([]byte, 4000)
	n, err := enc.Encode(stereo, pkt)
	require.NoError(t, err)
	pkt = pkt[:n]

	// Decode with a mono target; the decoder should detect 2 channels and downmix.
	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, rate)
	dec, err := DecodeWith(sink, 1, logger.GetLogger())
	require.NoError(t, err)
	require.NoError(t, dec.WriteSample(Sample(pkt)))

	require.Equal(t, perChannel, len(out), "stereo packet must downmix to one mono frame")
	var energy int64
	for _, s := range out {
		energy += int64(s) * int64(s)
	}
	require.Positive(t, energy, "downmixed audio must be non-silent")
}

// The decoder must tolerate a small run of corrupt packets without returning an
// error, then recover and decode a subsequent valid packet.
func TestOpusDecodeToleratesCorruptPackets(t *testing.T) {
	const (
		rate     = 48000
		frameLen = rate / 50 // 960
	)

	var out msdk.PCM16Sample
	sink := msdk.NewPCM16BufferWriter(&out, rate)
	dec, err := DecodeWith(sink, 1, logger.GetLogger())
	require.NoError(t, err)

	// Single-byte code-3 packet (TOC lower 2 bits = 3): a code-3 frame requires
	// at least 2 bytes, so libopus returns OPUS_INVALID_PACKET. The mono `s` bit
	// (bit 2) is 0, so per-packet channel detection still succeeds and we reach
	// the decode-tolerance path. Five corrupt packets is exactly the tolerated
	// threshold (a 6th would return an error).
	corrupt := []Sample{
		{0x03}, {0x03}, {0x03}, {0x03}, {0x03},
	}
	for i, pkt := range corrupt {
		require.NoError(t, dec.WriteSample(pkt), "corrupt packet %d must be tolerated, not error", i)
	}
	require.Empty(t, out, "corrupt packets must not produce PCM")

	// A valid packet afterwards must decode and reset the tolerance counter.
	venc, err := hopus.NewEncoder(rate, 1, hopus.AppVoIP)
	require.NoError(t, err)
	sig := make([]int16, frameLen)
	for i := range sig {
		sig[i] = int16(4000 * (i % 50))
	}
	buf := make([]byte, 4000)
	vn, err := venc.Encode(sig, buf)
	require.NoError(t, err)
	require.NoError(t, dec.WriteSample(Sample(buf[:vn])))
	require.Equal(t, frameLen, len(out), "decoder must recover and decode a valid packet")
}
