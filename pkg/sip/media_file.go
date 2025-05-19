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
	msdk "github.com/livekit/media-sdk"

	"github.com/livekit/sip/res"
)

type mediaRes struct {
	enterPin []msdk.PCM16Sample
	roomJoin []msdk.PCM16Sample
	wrongPin []msdk.PCM16Sample
}

func (s *Server) initMediaRes() {
	s.res.enterPin = res.ReadOggAudioFile(res.EnterPinOgg)
	s.res.roomJoin = res.ReadOggAudioFile(res.RoomJoinOgg)
	s.res.wrongPin = res.ReadOggAudioFile(res.WrongPinOgg)
}
