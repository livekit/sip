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

package opus

import (
	"gopkg.in/hraban/opus.v2"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

type Sample []byte

func Decode(w media.Writer[media.PCM16Sample], channels int) (media.Writer[Sample], error) {
	dec, err := opus.NewDecoder(w.SampleRate(), channels)
	if err != nil {
		return nil, err
	}
	buf := make([]int16, w.SampleRate()/rtp.DefFramesPerSec)
	return media.NewWriterFunc(w.SampleRate(), func(in Sample) error {
		n, err := dec.Decode(in, buf)
		if err != nil {
			return err
		}
		return w.WriteSample(buf[:n])
	}), nil
}

func Encode(w media.Writer[Sample], channels int) (media.Writer[media.PCM16Sample], error) {
	enc, err := opus.NewEncoder(w.SampleRate(), channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, w.SampleRate()/rtp.DefFramesPerSec)
	return media.NewWriterFunc(w.SampleRate(), func(in media.PCM16Sample) error {
		n, err := enc.Encode(in, buf)
		if err != nil {
			return err
		}
		return w.WriteSample(buf[:n])
	}), nil
}
