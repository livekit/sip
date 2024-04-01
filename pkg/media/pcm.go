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
	"context"
	"time"

	"github.com/pion/webrtc/v3/pkg/media"
)

func PlayAudio[T any](ctx context.Context, w Writer[T], sampleDur time.Duration, frames []T) error {
	if len(frames) == 0 {
		return nil
	}
	tick := time.NewTicker(sampleDur)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		samples := frames[0]
		frames = frames[1:]
		if err := w.WriteSample(samples); err != nil {
			return err
		}
		if len(frames) == 0 {
			return nil
		}
	}
}

var _ PCM16Writer = (*PCM16Sample)(nil)

type PCM16Sample []int16

func (s PCM16Sample) Clear() {
	for i := range s {
		s[i] = 0
	}
}

func (s *PCM16Sample) WriteSample(data PCM16Sample) error {
	*s = append(*s, data...)
	return nil
}

type PCM16Writer = Writer[PCM16Sample]
type PCM16WriteCloser = WriteCloser[PCM16Sample]

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
