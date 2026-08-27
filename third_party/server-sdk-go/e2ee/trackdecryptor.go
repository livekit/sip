// Copyright 2023 LiveKit, Inc.
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

package e2ee

import (
	"fmt"
	"io"
	"strings"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/livekit/server-sdk-go/v2/e2ee/types"
	"github.com/livekit/server-sdk-go/v2/pkg/samplebuilder"
)

const (
	// defaultMaxLate is the default number of packets the samplebuilder
	// will buffer before considering earlier packets lost. 150 is a
	// reasonable middle ground: handles moderate jitter without
	// ballooning memory on sustained loss.
	defaultMaxLate = 150
)

// TrackDecryptor reassembles RTP packets from a remote track into complete
// media frames, then decrypts each frame.
type TrackDecryptor struct {
	track     *webrtc.TrackRemote
	decryptor types.FrameDecryptor
	sb        *samplebuilder.SampleBuilder
}

// NewTrackDecryptor creates a decryptor for the given remote track.
// The depacketizer and maxLate are inferred from the track's codec.
func NewTrackDecryptor(track *webrtc.TrackRemote, decryptor types.FrameDecryptor) *TrackDecryptor {
	depacketizer := depacketizerForCodec(track.Codec().MimeType)
	maxLate := uint16(defaultMaxLate)

	return &TrackDecryptor{
		track:     track,
		decryptor: decryptor,
		sb:        samplebuilder.New(maxLate, depacketizer, track.Codec().ClockRate),
	}
}

// NewTrackDecryptorWithOptions creates a decryptor with explicit maxLate.
func NewTrackDecryptorWithOptions(track *webrtc.TrackRemote, decryptor types.FrameDecryptor, maxLate uint16) *TrackDecryptor {
	depacketizer := depacketizerForCodec(track.Codec().MimeType)
	return &TrackDecryptor{
		track:     track,
		decryptor: decryptor,
		sb:        samplebuilder.New(maxLate, depacketizer, track.Codec().ClockRate),
	}
}

// ReadSample reads RTP packets from the remote track, reassembles them
// into a complete frame, and decrypts the frame. Returns (nil, nil) for
// server-injected frames (SIF) that should be dropped. Returns io.EOF
// when the track is closed.
func (td *TrackDecryptor) ReadSample() (*media.Sample, error) {
	for {
		// Feed packets into the samplebuilder until a frame is ready.
		sample := td.sb.Pop()
		if sample != nil {
			decrypted, err := td.decryptor.DecryptFrame(sample.Data)
			if err != nil {
				return nil, fmt.Errorf("decrypt frame: %w", err)
			}
			if decrypted == nil {
				// Server-injected frame, skip it.
				continue
			}
			sample.Data = decrypted
			return sample, nil
		}

		// Read more RTP packets from the track.
		pkt, _, err := td.track.ReadRTP()
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("read RTP: %w", err)
		}
		td.sb.Push(pkt)
	}
}

func depacketizerForCodec(mimeType string) rtp.Depacketizer {
	mime := strings.ToLower(mimeType)
	switch {
	case strings.Contains(mime, "h264"):
		return &codecs.H264Packet{}
	case strings.Contains(mime, "h265") || strings.Contains(mime, "hevc"):
		return &codecs.H265Packet{}
	case strings.Contains(mime, "vp8"):
		return &codecs.VP8Packet{}
	case strings.Contains(mime, "vp9"):
		return &codecs.VP9Packet{}
	case strings.Contains(mime, "opus"):
		return &codecs.OpusPacket{}
	default:
		// For unknown codecs, use Opus depacketizer as a simple passthrough
		// (single RTP packet = single frame, no fragmentation logic).
		return &codecs.OpusPacket{}
	}
}
