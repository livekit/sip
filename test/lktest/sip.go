// Copyright 2024 LiveKit, Inc.
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

package lktest

import (
	"context"

	"github.com/livekit/protocol/livekit"
)

type SIPOutboundTestParams struct {
	TrunkOut  string // trunk ID for outbound call
	NumberOut string // number to call fom
	RoomOut   string // room for outbound call
	NumberIn  string // number to call to
	RoomIn    string // room for inbound call
	RoomPin   string // room pin for inbound call
}

func TestSIPOutbound(t TB, ctx context.Context, lkOut, lkIn *LiveKit, params SIPOutboundTestParams) {
	lkOut.CreateSIPParticipant(t, params.TrunkOut, params.RoomOut, "Outbound Call", params.NumberIn, params.RoomPin)

	const (
		nameOut = "testOut"
		nameIn  = "testIn"
	)

	// LK participants that will generate/listen for audio.
	pOut := lkOut.ConnectParticipant(t, params.RoomOut, nameOut, nil)
	pIn := lkIn.ConnectParticipant(t, params.RoomIn, nameIn, nil)

	lkOut.ExpectRoomWithParticipants(t, ctx, params.RoomOut, []ParticipantInfo{
		{Identity: nameOut, Kind: livekit.ParticipantInfo_STANDARD},
		{Identity: "Outbound Call", Kind: livekit.ParticipantInfo_SIP},
	})
	lkIn.ExpectRoomWithParticipants(t, ctx, params.RoomIn, []ParticipantInfo{
		{Identity: nameIn, Kind: livekit.ParticipantInfo_STANDARD},
		{Identity: "Phone " + params.NumberOut, Kind: livekit.ParticipantInfo_SIP},
	})

	CheckAudioForParticipants(t, ctx, pOut, pIn)
}
