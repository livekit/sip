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
	"maps"
	"strings"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/require"
)

func checkSIPCallID(t TB, exp, got map[string]string) (_, _ map[string]string) {
	if _, ok := exp[livekit.AttrSIPCallID]; !ok {
		return // not required
	}
	require.True(t, strings.HasPrefix(got[livekit.AttrSIPCallID], guid.SIPCallPrefix))
	exp, got = maps.Clone(exp), maps.Clone(got)
	delete(exp, livekit.AttrSIPCallID)
	delete(got, livekit.AttrSIPCallID)
	return exp, got
}

type SIPOutboundTestParams struct {
	TrunkOut    string // trunk ID for outbound call
	NumberOut   string // number to call fom
	RoomOut     string // room for outbound call
	IdentityOut string
	TrunkIn     string // trunk ID for inbound call
	RuleIn      string // rule ID for inbound call
	NumberIn    string // number to call to
	RoomIn      string // room for inbound call
	RoomPin     string // room pin for inbound call
	MetaIn      string // expected metadata for inbound participants
}

func TestSIPOutbound(t TB, ctx context.Context, lkOut, lkIn *LiveKit, params SIPOutboundTestParams) {
	t.Log("creating sip participant")
	const (
		outIdentity = "siptest_outbound"
		outName     = "Outbound Call"
		outMeta     = `{"test":true, "dir": "out"}`
	)
	var (
		inIdentity = "sip_" + params.NumberOut
		inName     = "Phone " + params.NumberOut
	)
	// Make sure we remove rooms when the test ends.
	// Some tests may reuse LK server, in which case the participants could stay in rooms for a long time.
	t.Cleanup(func() {
		_, _ = lkOut.Rooms.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: params.RoomOut})
		_, _ = lkIn.Rooms.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: params.RoomIn})
	})
	// Make sure we delete inbound SIP participant. Outbound is deleted automatically by CreateSIPParticipant.
	t.Cleanup(func() {
		_, _ = lkIn.Rooms.RemoveParticipant(context.Background(), &livekit.RoomParticipantIdentity{
			Room: params.RoomIn, Identity: inIdentity,
		})
	})

	// Start the outbound call. It should hit Trunk Provider and initiate an inbound call back to the second server.
	lkOut.CreateSIPParticipant(t, params.TrunkOut, params.RoomOut, outIdentity, outName, outMeta, params.NumberIn, params.RoomPin)

	const (
		nameOut = "testOut"
		nameIn  = "testIn"
	)

	var (
		dataOut = make(chan lksdk.DataPacket, 20)
		dataIn  = make(chan lksdk.DataPacket, 20)
	)

	// LK participants that will generate/listen for audio.
	t.Log("connecting lk participant (outbound)")
	pOut := lkOut.ConnectParticipant(t, params.RoomOut, nameOut, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				select {
				case dataOut <- data:
				default:
				}
			},
		},
	})
	t.Log("connecting lk participant (inbound)")
	pIn := lkIn.ConnectParticipant(t, params.RoomIn, nameIn, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				select {
				case dataIn <- data:
				default:
				}
			},
		},
	})

	t.Log("checking rooms (outbound)")
	lkOut.ExpectRoomWithParticipants(t, ctx, params.RoomOut, []ParticipantInfo{
		{Identity: nameOut, Kind: livekit.ParticipantInfo_STANDARD},
		{
			Identity: outIdentity,
			Name:     outName,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: outMeta,
			Attributes: map[string]string{
				"sip.callID":           "<test>", // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": params.NumberOut,
				"sip.phoneNumber":      params.NumberIn,
				"sip.trunkID":          params.TrunkOut,
			},
		},
	})
	t.Log("checking rooms (inbound)")
	lkIn.ExpectRoomWithParticipants(t, ctx, params.RoomIn, []ParticipantInfo{
		{Identity: nameIn, Kind: livekit.ParticipantInfo_STANDARD},
		{
			Identity: inIdentity,
			Name:     inName,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: params.MetaIn,
			Attributes: map[string]string{
				"sip.callID":           "<test>", // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": params.NumberIn,
				"sip.phoneNumber":      params.NumberOut,
				"sip.trunkID":          params.TrunkIn,
				"sip.ruleID":           params.RuleIn,
			},
		},
	})

	t.Log("testing audio")
	CheckAudioForParticipants(t, ctx, pOut, pIn)
	t.Log("testing dtmf")
	CheckDTMFForParticipants(t, ctx, pOut, pIn, dataOut, dataIn)
}
