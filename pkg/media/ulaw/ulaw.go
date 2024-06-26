// Copyright 2023 LiveKit, Inc.
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

package ulaw

import (
	prtp "github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

const SDPName = "PCMU/8000"

func init() {
	media.RegisterCodec(rtp.NewAudioCodec(media.CodecInfo{
		SDPName:     SDPName,
		SampleRate:  8000,
		RTPDefType:  prtp.PayloadTypePCMU,
		RTPIsStatic: true,
		Priority:    -10,
	}, Decode, Encode))
}

type Sample []byte

func (s Sample) Decode() media.PCM16Sample {
	return DecodeUlaw(s)
}

func (s *Sample) Encode(data media.PCM16Sample) {
	*s = EncodeUlaw(data)
}

type Writer = media.Writer[Sample]

type Decoder struct {
	w   media.PCM16Writer
	buf media.PCM16Sample
}

func (d *Decoder) SampleRate() int {
	return d.w.SampleRate()
}

func (d *Decoder) WriteSample(in Sample) error {
	if len(in) >= cap(d.buf) {
		d.buf = make(media.PCM16Sample, len(in))
	} else {
		d.buf = d.buf[:len(in)]
	}
	DecodeUlawTo(d.buf, in)
	return d.w.WriteSample(d.buf)
}

func Decode(w media.PCM16Writer) Writer {
	switch w.SampleRate() {
	default:
		w = media.ResampleWriter(w, 8000)
	case 8000:
	}
	return &Decoder{w: w}
}

type Encoder struct {
	w   Writer
	buf Sample
}

func (e *Encoder) SampleRate() int {
	return e.w.SampleRate()
}

func (e *Encoder) WriteSample(in media.PCM16Sample) error {
	if len(in) >= cap(e.buf) {
		e.buf = make(Sample, len(in))
	} else {
		e.buf = e.buf[:len(in)]
	}
	EncodeUlawTo(e.buf, in)
	return e.w.WriteSample(e.buf)
}

func Encode(w Writer) media.PCM16Writer {
	switch w.SampleRate() {
	default:
		panic("unsupported sample rate")
	case 8000:
	}
	return &Encoder{w: w}
}
