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
	"sync/atomic"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media/opus"
)

const (
	// OpusSDPName is the rtpmap encoding name advertised for Opus. Per RFC 7587
	// the channel field MUST be 2 even when the actual media is mono.
	OpusSDPName = "opus/48000/2"
	// OpusSampleRate is the (and only sensible) RTP clock rate for Opus.
	OpusSampleRate = 48000
	// opusChannels is the number of channels we encode/decode internally. SIP
	// telephony is mono; we still advertise opus/48000/2 per RFC 7587 and the
	// decoder adapts to whatever channel count peers actually send.
	opusChannels = 1
	// opusPriority makes Opus the preferred codec during priority-based codec
	// selection (higher than AMR-WB -4, G722 -5, PCMU -10, PCMA -20).
	opusPriority = 10
)

// opusOptions holds the live encoder configuration. It is read when each call's
// encoder is created, so SetOpusOptions can be called after the codec is
// registered (e.g. once the YAML config has been parsed).
var opusOptions atomic.Pointer[opus.EncodeOptions]

// SetOpusOptions updates the Opus encoder options used for new calls. The
// channel count is always forced to mono for SIP media.
func SetOpusOptions(opts opus.EncodeOptions) {
	opts.Channels = opusChannels
	opusOptions.Store(&opts)
}

func currentOpusOptions() opus.EncodeOptions {
	if o := opusOptions.Load(); o != nil {
		return *o
	}
	return opus.EncodeOptions{Channels: opusChannels}
}

// TODO(opus, experimental): before marking Opus GA:
//   - Advertise Opus fmtp params for better SBC interop, e.g.
//     "a=fmtp:<pt> useinbandfec=1;minptime=10;stereo=0". media-sdk's sdp package
//     currently emits/parses only rtpmap (+DTMF fmtp), so this needs support
//     there or a local post-processing step on the generated SDP.
//   - Validate live interop with FreeSWITCH, Asterisk, and at least one cloud
//     SIP trunk (Twilio/Telnyx), covering Opus offer/answer, hold/resume, and
//     transfer. Unit tests cover negotiation/RTP but not real endpoints.
func init() {
	msdk.RegisterCodec(msdk.NewAudioCodec(msdk.CodecInfo{
		SDPName:      OpusSDPName,
		SampleRate:   OpusSampleRate,
		RTPClockRate: OpusSampleRate,
		RTPIsStatic:  false, // dynamic payload type, assigned during SDP negotiation
		Priority:     opusPriority,
		Disabled:     true, // opt-in only; enabled via the enable_opus config flag
		FileExt:      "opus",
	}, opusDecode, opusEncode))
}

// SetOpusEnabled toggles Opus end-to-end, making the enable_opus config flag
// authoritative. It must update both:
//   - defaultCodecs: the per-call base set used to build SDP offers and answers
//     (see codecSet in media_codecs.go), and
//   - the media-sdk GlobalCodecs set, used by any global/deprecated code path.
//
// When disabled, Opus appears in neither, so it is never offered or selected.
//
// TODO(opus): defaultCodecs is a plain map with no mutex. This is safe today
// because SetOpusEnabled is called once during Service.Start, before any call
// is accepted. If config hot-reload is ever added, mutating defaultCodecs while
// calls read it concurrently would be a data race; guard it then.
func SetOpusEnabled(enabled bool) {
	defaultCodecs.SetEnabled(OpusSDPName, enabled)
	msdk.CodecSetEnabled(OpusSDPName, enabled)
}

func opusDecode(w msdk.PCM16Writer) msdk.WriteCloser[opus.Sample] {
	dec, err := opus.DecodeWith(w, opusChannels, logger.GetLogger())
	if err != nil {
		logger.GetLogger().Errorw("cannot create opus decoder", err)
		return discardSampleWriter{w: w}
	}
	return dec
}

func opusEncode(w msdk.WriteCloser[opus.Sample]) msdk.PCM16Writer {
	enc, err := opus.EncodeWith(w, currentOpusOptions(), logger.GetLogger())
	if err != nil {
		logger.GetLogger().Errorw("cannot create opus encoder", err)
		return discardPCMWriter{w: w}
	}
	return enc
}

// discardSampleWriter / discardPCMWriter keep the media pipeline alive if codec
// initialization unexpectedly fails (already logged at error level). They drop
// samples and forward Close to the underlying writer so resources are released.
// Initialization only fails on invalid encoder options (bitrate/complexity out
// of range); with mono/48kHz and validated options this should not happen.
type discardSampleWriter struct{ w msdk.PCM16Writer }

func (d discardSampleWriter) String() string                { return "OPUS(decode-discard)" }
func (d discardSampleWriter) SampleRate() int               { return d.w.SampleRate() }
func (d discardSampleWriter) WriteSample(opus.Sample) error { return nil }
func (d discardSampleWriter) Close() error                  { return d.w.Close() }

type discardPCMWriter struct{ w msdk.WriteCloser[opus.Sample] }

func (d discardPCMWriter) String() string                     { return "OPUS(encode-discard)" }
func (d discardPCMWriter) SampleRate() int                    { return d.w.SampleRate() }
func (d discardPCMWriter) WriteSample(msdk.PCM16Sample) error { return nil }
func (d discardPCMWriter) Close() error                       { return d.w.Close() }
