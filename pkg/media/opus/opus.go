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
)

type Sample []byte

func Decode(w media.Writer[media.PCM16Sample], sampleRate int, channels int) (media.Writer[Sample], error) {
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return nil, err
	}
	buf := make([]int16, 1000)
	return media.WriterFunc[Sample](func(in Sample) error {
		n, err := dec.Decode(in, buf)
		if err != nil {
			return err
		}
		return w.WriteSample(buf[:n])
	}), nil
}

func Encode(w media.Writer[Sample], sampleRate int, channels int) (media.Writer[media.PCM16Sample], error) {
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1024)
	return media.WriterFunc[media.PCM16Sample](func(in media.PCM16Sample) error {
		n, err := enc.Encode(in, buf)
		if err != nil {
			return err
		}
		return w.WriteSample(buf[:n])
	}), nil
}
