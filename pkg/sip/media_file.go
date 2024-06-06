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

package sip

import (
	"bytes"
	"io"

	"github.com/jfreymuth/oggvorbis"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/res"
)

type mediaRes struct {
	enterPin []media.PCM16Sample
	roomJoin []media.PCM16Sample
	wrongPin []media.PCM16Sample
}

const embedSampleRate = 48000

func (s *Server) initMediaRes() {
	s.res.enterPin = readOggAudioFile(res.EnterPinOgg)
	s.res.roomJoin = readOggAudioFile(res.RoomJoinOgg)
	s.res.wrongPin = readOggAudioFile(res.WrongPinOgg)
}

func readOggAudioFile(data []byte) []media.PCM16Sample {
	const perFrame = embedSampleRate / rtp.DefFramesPerSec
	r, err := oggvorbis.NewReader(bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	if r.SampleRate() != embedSampleRate {
		panic("unexpected sample rate")
	}
	if r.Channels() != 1 {
		panic("expected mono audio")
	}
	// Frames in the source file may be shorter,
	// so we collect all samples and split them to frames again.
	var samples media.PCM16Sample
	buf := make([]float32, perFrame)
	for {
		n, err := r.Read(buf)
		if n != 0 {
			frame := make(media.PCM16Sample, n)
			for i := range frame {
				frame[i] = int16(buf[i] * 0x7fff)
			}
			samples = append(samples, frame...)
		}
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
	}
	var frames []media.PCM16Sample
	for len(samples) > 0 {
		cur := samples
		if len(cur) > perFrame {
			cur = cur[:perFrame]
		}
		frames = append(frames, cur)
		samples = samples[len(cur):]
	}
	return frames
}
