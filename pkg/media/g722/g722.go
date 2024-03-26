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
	"github.com/gotranspile/g722"
	prtp "github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

const SDPName = "G722/8000"

func init() {
	media.RegisterCodec(rtp.NewAudioCodec(media.CodecInfo{
		SDPName:     SDPName,
		RTPDefType:  prtp.PayloadTypeG722,
		RTPIsStatic: true,
		Priority:    1,
	}, Decode, Encode))
}

type Sample []byte

func (s Sample) Decode() media.PCM16Sample {
	return g722.Decode(s, g722.Rate64000, g722.FlagSampleRate8000)
}

func (s *Sample) Encode(data media.PCM16Sample) {
	*s = g722.Encode(data, g722.Rate64000, g722.FlagSampleRate8000)
}

type Writer = media.Writer[Sample]

type Decoder struct {
	w media.PCM16Writer
}

func (d *Decoder) WriteSample(in Sample) error {
	// TODO: reuse buffer
	out := in.Decode()
	return d.w.WriteSample(out)
}

func Decode(w media.PCM16Writer) Writer {
	return &Decoder{w: w}
}

type Encoder struct {
	w Writer
}

func (e *Encoder) WriteSample(in media.PCM16Sample) error {
	// TODO: reuse buffer
	var out Sample
	out.Encode(in)
	return e.w.WriteSample(out)
}

func Encode(w Writer) media.PCM16Writer {
	return &Encoder{w: w}
}
