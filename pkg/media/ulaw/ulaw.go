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
	"github.com/livekit/sip/pkg/media"
)

type Sample []byte

func (s Sample) Decode() media.PCM16Sample {
	return DecodeUlaw(s)
}

func (s *Sample) Encode(data media.PCM16Sample) {
	*s = EncodeUlaw(data)
}

func Decode(w media.Writer[media.PCM16Sample]) media.Writer[Sample] {
	return media.WriterFunc[Sample](func(in Sample) error {
		out := in.Decode()
		return w.WriteSample(out)
	})
}

func Encode(w media.Writer[Sample]) media.Writer[media.PCM16Sample] {
	return media.WriterFunc[media.PCM16Sample](func(in media.PCM16Sample) error {
		var s Sample
		s.Encode(in)
		return w.WriteSample(s)
	})
}
