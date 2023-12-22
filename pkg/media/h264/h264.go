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

package h264

import (
	m "github.com/livekit/sip/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media"
	"time"
)

type Sample []byte

type Encoder struct {
	w   m.Writer[Sample]
	buf Sample
}

func (e *Encoder) WriteSample(in Sample) error {
	return e.w.WriteSample(in)
}

func Encode(w m.Writer[Sample]) m.Writer[Sample] {
	return &Encoder{w: w}
}

type SampleWriter interface {
	WriteSample(sample media.Sample) error
}

func BuildSampleWriter[T ~[]byte](w SampleWriter, sampleDur time.Duration) m.Writer[T] {
	return m.WriterFunc[T](func(in T) error {
		data := make([]byte, len(in))
		copy(data, in)
		return w.WriteSample(media.Sample{Data: data})
	})
}
