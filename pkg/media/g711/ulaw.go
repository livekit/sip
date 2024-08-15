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

const ULawSDPName = "PCMU/8000"

func init() {
	media.RegisterCodec(rtp.NewAudioCodec(media.CodecInfo{
		SDPName:     ULawSDPName,
		SampleRate:  8000,
		RTPDefType:  prtp.PayloadTypePCMU,
		RTPIsStatic: true,
		Priority:    -10,
	}, DecodeULaw, EncodeULaw))
}

type ULawSample []byte

func (s ULawSample) Decode() media.PCM16Sample {
	out := make(media.PCM16Sample, len(s))
	DecodeULawTo(out, s)
	return out
}

func (s *ULawSample) Encode(data media.PCM16Sample) {
	out := make(ULawSample, len(data))
	EncodeULawTo(out, data)
	*s = out
}

type ULawWriter = media.WriteCloser[ULawSample]

type ULawDecoder struct {
	w   media.PCM16Writer
	buf media.PCM16Sample
}

func (d *ULawDecoder) SampleRate() int {
	return d.w.SampleRate()
}

func (d *ULawDecoder) Close() error {
	return d.w.Close()
}

func (d *ULawDecoder) WriteSample(in ULawSample) error {
	if len(in) >= cap(d.buf) {
		d.buf = make(media.PCM16Sample, len(in))
	} else {
		d.buf = d.buf[:len(in)]
	}
	DecodeULawTo(d.buf, in)
	return d.w.WriteSample(d.buf)
}

func DecodeULaw(w media.PCM16Writer) ULawWriter {
	switch w.SampleRate() {
	default:
		w = media.ResampleWriter(w, 8000)
	case 8000:
	}
	return &ULawDecoder{w: w}
}

type ULawEncoder struct {
	w   ULawWriter
	buf ULawSample
}

func (e *ULawEncoder) SampleRate() int {
	return e.w.SampleRate()
}

func (e *ULawEncoder) Close() error {
	return e.w.Close()
}

func (e *ULawEncoder) WriteSample(in media.PCM16Sample) error {
	if len(in) >= cap(e.buf) {
		e.buf = make(ULawSample, len(in))
	} else {
		e.buf = e.buf[:len(in)]
	}
	EncodeULawTo(e.buf, in)
	return e.w.WriteSample(e.buf)
}

func EncodeULaw(w ULawWriter) media.PCM16Writer {
	switch w.SampleRate() {
	default:
		panic("unsupported sample rate")
	case 8000:
	}
	return &ULawEncoder{w: w}
}
