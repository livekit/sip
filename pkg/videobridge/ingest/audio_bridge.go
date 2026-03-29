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

package ingest

import (
	"fmt"
	"sync"

	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"
)

const (
	pcmuPayloadType = 0
	pcmaPayloadType = 8
	pcmSampleRate   = 8000
	opusSampleRate  = 48000
)

// G711Codec represents the G.711 variant.
type G711Codec int

const (
	G711PCMU G711Codec = iota // mu-law
	G711PCMA                  // A-law
)

// AudioBridge decodes incoming G.711 RTP audio and re-encodes it as Opus
// for publishing into LiveKit. It handles sample rate conversion (8kHz → 48kHz)
// and frame assembly.
type AudioBridge struct {
	log   logger.Logger
	codec G711Codec

	mu     sync.Mutex
	output AudioOpusWriter

	// Decode buffer: accumulates PCM16 samples at 8kHz
	pcmBuf []int16
	// Resample buffer: holds upsampled PCM at 48kHz
	resBuf []int16

	// Expected samples per Opus frame at 48kHz (20ms = 960 samples)
	opusFrameSize int
	// Accumulated PCM16 at 48kHz waiting to fill an Opus frame
	accumBuf []int16
}

// AudioOpusWriter receives PCM16 audio samples at 48kHz for Opus encoding.
type AudioOpusWriter interface {
	// WriteOpusPCM writes PCM16 samples (48kHz, mono) that will be Opus-encoded.
	WriteOpusPCM(samples []int16) error
}

// NewAudioBridge creates a new G.711 → Opus audio bridge.
func NewAudioBridge(log logger.Logger, codec G711Codec) *AudioBridge {
	return &AudioBridge{
		log:           log,
		codec:         codec,
		pcmBuf:        make([]int16, 0, 320),  // 40ms at 8kHz
		resBuf:        make([]int16, 0, 1920), // 40ms at 48kHz
		opusFrameSize: opusSampleRate / 50,    // 960 samples = 20ms at 48kHz
		accumBuf:      make([]int16, 0, 960),
	}
}

// SetOutput sets the Opus PCM writer.
func (b *AudioBridge) SetOutput(w AudioOpusWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.output = w
}

// HandleRTP processes an incoming G.711 RTP audio packet.
func (b *AudioBridge) HandleRTP(pkt *rtp.Packet) error {
	if len(pkt.Payload) == 0 {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.output == nil {
		return nil
	}

	// Decode G.711 → PCM16 at 8kHz
	pcm := b.decodeToPCM(pkt.Payload)

	// Upsample 8kHz → 48kHz (simple linear interpolation)
	upsampled := b.upsample(pcm, pcmSampleRate, opusSampleRate)

	// Accumulate and emit 20ms Opus frames
	b.accumBuf = append(b.accumBuf, upsampled...)
	for len(b.accumBuf) >= b.opusFrameSize {
		frame := make([]int16, b.opusFrameSize)
		copy(frame, b.accumBuf[:b.opusFrameSize])
		b.accumBuf = b.accumBuf[b.opusFrameSize:]

		if err := b.output.WriteOpusPCM(frame); err != nil {
			return fmt.Errorf("writing opus PCM: %w", err)
		}
	}

	return nil
}

// decodeToPCM decodes G.711 bytes to PCM16 samples.
func (b *AudioBridge) decodeToPCM(payload []byte) []int16 {
	if cap(b.pcmBuf) < len(payload) {
		b.pcmBuf = make([]int16, len(payload))
	} else {
		b.pcmBuf = b.pcmBuf[:len(payload)]
	}

	switch b.codec {
	case G711PCMU:
		for i, v := range payload {
			b.pcmBuf[i] = ulawDecode(v)
		}
	case G711PCMA:
		for i, v := range payload {
			b.pcmBuf[i] = alawDecode(v)
		}
	}

	return b.pcmBuf
}

// upsample converts samples from srcRate to dstRate using linear interpolation.
func (b *AudioBridge) upsample(samples []int16, srcRate, dstRate int) []int16 {
	if srcRate == dstRate {
		return samples
	}

	ratio := float64(dstRate) / float64(srcRate)
	outLen := int(float64(len(samples)) * ratio)

	if cap(b.resBuf) < outLen {
		b.resBuf = make([]int16, outLen)
	} else {
		b.resBuf = b.resBuf[:outLen]
	}

	for i := 0; i < outLen; i++ {
		srcIdx := float64(i) / ratio
		idx0 := int(srcIdx)
		frac := srcIdx - float64(idx0)

		if idx0+1 < len(samples) {
			v0 := float64(samples[idx0])
			v1 := float64(samples[idx0+1])
			b.resBuf[i] = int16(v0 + frac*(v1-v0))
		} else if idx0 < len(samples) {
			b.resBuf[i] = samples[idx0]
		}
	}

	return b.resBuf
}

// G.711 mu-law and A-law decode tables (ITU-T G.711)
var (
	ulawTable [256]int16
	alawTable [256]int16
)

func init() {
	for i := 0; i < 256; i++ {
		ulawTable[i] = ulawToLinear(byte(i))
		alawTable[i] = alawToLinear(byte(i))
	}
}

func ulawDecode(v byte) int16 { return ulawTable[v] }
func alawDecode(v byte) int16 { return alawTable[v] }

func ulawToLinear(v byte) int16 {
	const bias = 0x84
	v = ^v
	t := (int(v&0x0F) << 3) + bias
	t <<= (uint(v) & 0x70) >> 4
	if (v & 0x80) != 0 {
		return int16(bias - t)
	}
	return int16(t - bias)
}

func alawToLinear(v byte) int16 {
	v ^= 0x55
	t := int(v & 0x0F)
	seg := int((uint(v) & 0x70) >> 4)
	if seg != 0 {
		t = (t + t + 1 + 32) << (uint(seg) + 2)
	} else {
		t = (t + t + 1) << 3
	}
	if (v & 0x80) != 0 {
		return int16(t)
	}
	return int16(-t)
}
