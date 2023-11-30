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

package media

import (
	"encoding/binary"
	"time"

	"github.com/pion/webrtc/v3/pkg/media"
)

func PlayAudio[T any](w Writer[T], sampleDur time.Duration, frames []T) error {
	tick := time.NewTicker(sampleDur)
	defer tick.Stop()
	for range tick.C {
		if len(frames) == 0 {
			break
		}
		samples := frames[0]
		frames = frames[1:]
		if err := w.WriteSample(samples); err != nil {
			return err
		}
	}
	return nil
}

type LPCM16Sample []byte

func (s LPCM16Sample) Decode() PCM16Sample {
	out := make(PCM16Sample, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		out[i/2] = int16(binary.LittleEndian.Uint16(s[i:]))
	}
	return out
}

type PCM16Sample []int16

func (s PCM16Sample) Encode() LPCM16Sample {
	out := make(LPCM16Sample, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

func DecodePCM(w Writer[PCM16Sample]) Writer[LPCM16Sample] {
	return WriterFunc[LPCM16Sample](func(in LPCM16Sample) error {
		return w.WriteSample(in.Decode())
	})
}

func EncodePCM(w Writer[LPCM16Sample]) Writer[PCM16Sample] {
	return WriterFunc[PCM16Sample](func(in PCM16Sample) error {
		return w.WriteSample(in.Encode())
	})
}

type MediaSampleWriter interface {
	WriteSample(sample media.Sample) error
}

func FromSampleWriter[T ~[]byte](w MediaSampleWriter, sampleDur time.Duration) Writer[T] {
	return WriterFunc[T](func(in T) error {
		data := make([]byte, len(in))
		copy(data, in)
		return w.WriteSample(media.Sample{Data: data, Duration: sampleDur})
	})
}
