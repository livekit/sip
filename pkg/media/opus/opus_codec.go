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

// This file holds the SIP-side Opus codec implementation: a configurable
// encoder (bitrate/complexity/FEC) and a decoder that adapts to the actual
// channel count of each packet. It is intentionally separate from the
// LiveKit/WebRTC encode/decode in opus.go (Decode/Encode), which other call
// paths (e.g. the room track) depend on and which must not change. Nothing here
// runs unless the SIP Opus codec is negotiated, which only happens when Opus is
// explicitly enabled.

package opus

import (
	"errors"
	"fmt"
	"sync"

	"gopkg.in/hraban/opus.v2"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

/*
#cgo pkg-config: opus
#include <opus.h>
*/
import "C"

// maxOpusFrameMs is the largest frame duration a single Opus packet can
// represent. Used to size the decode buffer so over-long packets don't fail.
const maxOpusFrameMs = 120

// maxOpusFrameSamples returns the per-channel sample count of the largest
// possible Opus frame at the given sample rate.
func maxOpusFrameSamples(sampleRate int) int {
	return sampleRate / 1000 * maxOpusFrameMs
}

// EncodeOptions controls how the SIP Opus encoder is configured. The zero value
// keeps libopus defaults, so callers only set the fields they care about.
type EncodeOptions struct {
	// Channels is the number of channels to encode (1 for mono, 2 for stereo).
	// Defaults to 1 (mono) when unset.
	Channels int
	// Bitrate is the target bitrate in bits per second. 0 leaves the libopus
	// automatic bitrate selection in place.
	Bitrate int
	// Complexity is the computational complexity, 1-10. 0 keeps the libopus default.
	Complexity int
	// FEC enables in-band Forward Error Correction.
	FEC bool
	// PacketLossPercent is the expected packet loss percentage (0-100) used to
	// tune FEC. Only meaningful when FEC is enabled.
	PacketLossPercent int
}

func (o EncodeOptions) channels() int {
	if o.Channels <= 0 {
		return 1
	}
	return o.Channels
}

// DecodeWith creates a SIP Opus decoder writing PCM to w. targetChannels is the
// channel layout the downstream pipeline expects (1 or 2); the decoder detects
// each packet's actual channel count and converts to targetChannels.
func DecodeWith(w msdk.PCM16Writer, targetChannels int, log logger.Logger) (Writer, error) {
	if targetChannels != 1 && targetChannels != 2 {
		return nil, fmt.Errorf("unsupported channel count: %d", targetChannels)
	}
	log = log.WithValues("codec", "opus-sip", "targetChannels", targetChannels)
	return &sipDecoder{
		w:              w,
		targetChannels: targetChannels,
		log:            log,
	}, nil
}

// EncodeWith creates a SIP Opus encoder using the provided options.
func EncodeWith(w Writer, opts EncodeOptions, log logger.Logger) (msdk.PCM16Writer, error) {
	channels := opts.channels()
	rate := w.SampleRate()
	log = log.WithValues("codec", "opus-sip", "rate", rate, "channels", channels)
	enc, err := opus.NewEncoder(rate, channels, opus.AppVoIP)
	if err != nil {
		log.Errorw("cannot initialize opus encoder", err)
		return nil, err
	}
	if opts.Bitrate > 0 {
		if err := enc.SetBitrate(opts.Bitrate); err != nil {
			log.Errorw("cannot set opus bitrate", err, "bitrate", opts.Bitrate)
			return nil, err
		}
	}
	if opts.Complexity > 0 {
		if err := enc.SetComplexity(opts.Complexity); err != nil {
			log.Errorw("cannot set opus complexity", err, "complexity", opts.Complexity)
			return nil, err
		}
	}
	if opts.FEC {
		if err := enc.SetInBandFEC(true); err != nil {
			log.Errorw("cannot enable opus FEC", err)
			return nil, err
		}
		if opts.PacketLossPercent > 0 {
			if err := enc.SetPacketLossPerc(opts.PacketLossPercent); err != nil {
				log.Errorw("cannot set opus packet loss percentage", err, "loss", opts.PacketLossPercent)
				return nil, err
			}
		}
	}
	samples := rate / rtp.DefFramesPerSec
	return &sipEncoder{
		w:        w,
		channels: channels,
		enc:      enc,
		samples:  samples,
		inbuf:    make(msdk.PCM16Sample, 0, samples*channels),
		buf:      make([]byte, 4*channels*samples),
		log:      log,
	}, nil
}

type sipDecoder struct {
	log logger.Logger

	mu             sync.Mutex
	w              msdk.PCM16Writer
	dec            *opus.Decoder
	targetChannels int
	lastChannels   int
	buf            msdk.PCM16Sample
	buf2           msdk.PCM16Sample

	successiveErrorCount int
}

func (d *sipDecoder) String() string {
	return fmt.Sprintf("OPUS(decode) -> %s", d.w)
}

func (d *sipDecoder) SampleRate() int {
	return d.w.SampleRate()
}

// resetForSample inspects the incoming Opus packet and (re)creates the decoder
// to match the actual channel count of the bitstream. This lets us decode
// streams whose channel count differs from what we advertised in SDP.
func (d *sipDecoder) resetForSample(in Sample) (int, error) {
	channels := int(C.opus_packet_get_nb_channels((*C.uchar)(&in[0])))
	if channels != 1 && channels != 2 {
		return 0, fmt.Errorf("unexpected opus packet channel count: %d", channels)
	}
	if d.dec == nil || d.lastChannels != channels {
		dec, err := opus.NewDecoder(d.w.SampleRate(), channels)
		if err != nil {
			d.log.Errorw("cannot initialize opus decoder", err, "channels", channels)
			return 0, err
		}
		d.dec = dec
		// Size for the largest possible Opus frame (120 ms) so we can decode
		// packets longer than our 20 ms ptime if a peer sends them.
		d.buf = make([]int16, maxOpusFrameSamples(d.w.SampleRate())*channels)
		d.lastChannels = channels
	}
	return channels, nil
}

func (d *sipDecoder) WriteSample(in Sample) error {
	if len(in) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	channels, err := d.resetForSample(in)
	if err != nil {
		return err
	}
	n, err := d.dec.Decode(in, d.buf)
	if err != nil {
		// Some workflows can cause a spurious decoding error, so ignore a small
		// number of corruption errors before giving up.
		if !errors.Is(err, opus.ErrInvalidPacket) || d.successiveErrorCount >= 5 {
			d.log.Warnw("error decoding opus sample", err)
			return err
		}
		d.log.Debugw("opus decoder failed decoding a sample", "error", err)
		d.successiveErrorCount++
		return nil
	}
	d.successiveErrorCount = 0

	// Decoded interleaved PCM; convert to the target channel layout if needed.
	out := d.buf[:n*channels]
	if channels < d.targetChannels {
		n2 := len(out) * 2
		if len(d.buf2) < n2 {
			d.buf2 = make(msdk.PCM16Sample, n2)
		}
		msdk.MonoToStereo(d.buf2, out)
		out = d.buf2[:n2]
	} else if channels > d.targetChannels {
		n2 := len(out) / 2
		if len(d.buf2) < n2 {
			d.buf2 = make(msdk.PCM16Sample, n2)
		}
		msdk.StereoToMono(d.buf2, out)
		out = d.buf2[:n2]
	}
	return d.w.WriteSample(out)
}

func (d *sipDecoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w.Close()
}

type sipEncoder struct {
	log      logger.Logger
	channels int
	samples  int

	mu    sync.Mutex
	w     Writer
	enc   *opus.Encoder
	inbuf msdk.PCM16Sample
	buf   Sample

	successiveErrorCount int
}

func (e *sipEncoder) String() string {
	return fmt.Sprintf("OPUS(encode) -> %s", e.w)
}

func (e *sipEncoder) SampleRate() int {
	return e.w.SampleRate()
}

func (e *sipEncoder) WriteSample(in msdk.PCM16Sample) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inbuf = append(e.inbuf, in...)
	// One Opus frame is `samples` PCM samples per channel, interleaved.
	frame := e.samples * e.channels
	for len(e.inbuf) >= frame {
		n, err := e.enc.Encode(e.inbuf[:frame], e.buf)

		sz := copy(e.inbuf, e.inbuf[frame:])
		e.inbuf = e.inbuf[:sz]
		if err != nil {
			if e.successiveErrorCount < 5 {
				e.log.Errorw("error encoding opus sample", err, "frame", frame, "buf", len(e.buf), "n", n)
				e.successiveErrorCount++
			}
			return err
		}
		e.successiveErrorCount = 0
		if err = e.w.WriteSample(e.buf[:n]); err != nil {
			return err
		}
	}
	return nil
}

func (e *sipEncoder) flush() error {
	if len(e.inbuf) == 0 {
		return nil
	}
	// libopus only accepts valid frame sizes, so zero-pad the trailing partial
	// buffer up to one full frame before encoding. Encoding the raw remainder
	// would return OPUS_BAD_ARG.
	frame := e.samples * e.channels
	if len(e.inbuf) < frame {
		e.inbuf = append(e.inbuf, make(msdk.PCM16Sample, frame-len(e.inbuf))...)
	}
	n, err := e.enc.Encode(e.inbuf[:frame], e.buf)
	if err != nil {
		return err
	}
	return e.w.WriteSample(e.buf[:n])
}

func (e *sipEncoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err1 := e.flush()
	err2 := e.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
