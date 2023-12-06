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

type PCM16Sample []int16

type PCM16Writer = Writer[PCM16Sample]

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
