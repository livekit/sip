// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lksdk

import (
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/livekit"
)

type Track interface {
	ID() string
}

type LocalTrackWithClose interface {
	webrtc.TrackLocal
	Close() error
}

type TrackKind string

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
)

func (k TrackKind) String() string {
	return string(k)
}

func (k TrackKind) RTPType() webrtc.RTPCodecType {
	return webrtc.NewRTPCodecType(k.String())
}

func (k TrackKind) ProtoType() livekit.TrackType {
	switch k {
	case TrackKindAudio:
		return livekit.TrackType_AUDIO
	case TrackKindVideo:
		return livekit.TrackType_VIDEO
	}
	return livekit.TrackType(0)
}

func KindFromRTPType(rt webrtc.RTPCodecType) TrackKind {
	return TrackKind(rt.String())
}
