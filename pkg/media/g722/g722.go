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

package g722

import (
	"fmt"

	"github.com/gotranspile/g722"
	prtp "github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

const SDPName = "G722/8000"

func init() {
	media.RegisterCodec(rtp.NewAudioCodec(media.CodecInfo{
		SDPName:      SDPName,
		SampleRate:   16000,
		RTPClockRate: 8000,
		RTPDefType:   prtp.PayloadTypeG722,
		RTPIsStatic:  true,
		Priority:     -5,
	}, Decode, Encode))
}

type Sample []byte

func decodeSize(flags g722.Flags, g722Len int) int {
	mul := 2
	if flags&g722.FlagSampleRate8000 != 0 {
		mul = 1
	}
	return g722Len * mul
}

func encodeSize(flags g722.Flags, pcmLen int) int {
	div := 2
	if flags&g722.FlagSampleRate8000 != 0 {
		div = 1
	}
	n := pcmLen / div
	if pcmLen%div != 0 {
		n++
	}
	return n
}

type Writer = media.WriteCloser[Sample]

type Decoder struct {
	f   g722.Flags
	d   *g722.Decoder
	buf media.PCM16Sample
	w   media.PCM16Writer
}

func (d *Decoder) String() string {
	return fmt.Sprintf("G722(decode) -> %s", d.w)
}

func (d *Decoder) SampleRate() int {
	return d.w.SampleRate()
}

func (d *Decoder) Close() error {
	return d.w.Close()
}

func (d *Decoder) WriteSample(in Sample) error {
	sz := decodeSize(d.f, len(in))
	if cap(d.buf) < sz {
		d.buf = make([]int16, sz)
	}
	n := d.d.Decode(d.buf, in)
	return d.w.WriteSample(d.buf[:n])
}

func Decode(w media.PCM16Writer) Writer {
	var f g722.Flags
	switch w.SampleRate() {
	case 8000:
		f |= g722.FlagSampleRate8000
	default:
		w = media.ResampleWriter(w, 16000)
		fallthrough
	case 16000:
		// default
	}
	return &Decoder{w: w, f: f, d: g722.NewDecoder(g722.Rate64000, f)}
}

type Encoder struct {
	f   g722.Flags
	e   *g722.Encoder
	buf Sample
	w   Writer
}

func (e *Encoder) String() string {
	return fmt.Sprintf("G722(encode) -> %s", e.w)
}

func (e *Encoder) SampleRate() int {
	return e.w.SampleRate()
}

func (e *Encoder) Close() error {
	return e.w.Close()
}

func (e *Encoder) WriteSample(in media.PCM16Sample) error {
	sz := encodeSize(e.f, len(in))
	if cap(e.buf) < sz {
		e.buf = make(Sample, sz)
	}
	n := e.e.Encode(e.buf, in)
	return e.w.WriteSample(e.buf[:n])
}

func Encode(w Writer) media.PCM16Writer {
	var f g722.Flags
	switch w.SampleRate() {
	default:
		panic("unsupported sample rate")
	case 8000:
		f |= g722.FlagSampleRate8000
	case 16000:
		// default
	}
	return &Encoder{w: w, f: f, e: g722.NewEncoder(g722.Rate64000, f)}
}
