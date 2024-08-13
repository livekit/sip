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

package g711

import (
	prtp "github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

const ALawSDPName = "PCMA/8000"

func init() {
	media.RegisterCodec(rtp.NewAudioCodec(media.CodecInfo{
		SDPName:     ALawSDPName,
		SampleRate:  8000,
		RTPDefType:  prtp.PayloadTypePCMA,
		RTPIsStatic: true,
		Priority:    -20,
	}, DecodeALaw, EncodeALaw))
}

type ALawSample []byte

func (s ALawSample) Decode() media.PCM16Sample {
	out := make(media.PCM16Sample, len(s))
	DecodeALawTo(out, s)
	return out
}

func (s *ALawSample) Encode(data media.PCM16Sample) {
	out := make(ALawSample, len(data))
	EncodeALawTo(out, data)
	*s = out
}

type ALawWriter = media.Writer[ALawSample]

type ALawDecoder struct {
	w   media.PCM16Writer
	buf media.PCM16Sample
}

func (d *ALawDecoder) SampleRate() int {
	return d.w.SampleRate()
}

func (d *ALawDecoder) WriteSample(in ALawSample) error {
	if len(in) >= cap(d.buf) {
		d.buf = make(media.PCM16Sample, len(in))
	} else {
		d.buf = d.buf[:len(in)]
	}
	DecodeALawTo(d.buf, in)
	return d.w.WriteSample(d.buf)
}

func DecodeALaw(w media.PCM16Writer) ALawWriter {
	switch w.SampleRate() {
	default:
		w = media.ResampleWriter(w, 8000)
	case 8000:
	}
	return &ALawDecoder{w: w}
}

type ALawEncoder struct {
	w   ALawWriter
	buf ALawSample
}

func (e *ALawEncoder) SampleRate() int {
	return e.w.SampleRate()
}

func (e *ALawEncoder) WriteSample(in media.PCM16Sample) error {
	if len(in) >= cap(e.buf) {
		e.buf = make(ALawSample, len(in))
	} else {
		e.buf = e.buf[:len(in)]
	}
	EncodeALawTo(e.buf, in)
	return e.w.WriteSample(e.buf)
}

func EncodeALaw(w ALawWriter) media.PCM16Writer {
	switch w.SampleRate() {
	default:
		panic("unsupported sample rate")
	case 8000:
	}
	return &ALawEncoder{w: w}
}
