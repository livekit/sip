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

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/ulaw"
	"github.com/livekit/sip/res"
)

type mediaRes struct {
	enterPin []media.PCM16Sample
	roomJoin []media.PCM16Sample
	wrongPin []media.PCM16Sample
}

func (s *Server) initMediaRes() {
	s.res.enterPin = readMkvAudioFile(res.EnterPinMkv)
	s.res.roomJoin = readMkvAudioFile(res.RoomJoinMkv)
	s.res.wrongPin = readMkvAudioFile(res.WrongPinMkv)
}

func readMkvAudioFile(data []byte) []media.PCM16Sample {
	var ret struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(bytes.NewReader(data), &ret); err != nil {
		panic(err)
	}

	var frames []media.PCM16Sample
	for _, cluster := range ret.Segment.Cluster {
		for _, block := range cluster.SimpleBlock {
			for _, data := range block.Data {
				lpcm := ulaw.Sample(data).Decode()
				pcm := lpcm.Decode()
				frames = append(frames, pcm)
			}
		}
	}
	return frames
}
