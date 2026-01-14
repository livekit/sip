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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const AttrTestAny = "<any>"

func checkSIPAttrs(t TB, exp, got map[string]string) (_, _ map[string]string) {
	exp, got = maps.Clone(exp), maps.Clone(got)

	var keepKeys []string
	for _, a := range []string{
		livekit.AttrSIPCallID,
		livekit.AttrSIPPrefix + "callIDFull",
		livekit.AttrSIPPrefix + "callTag",
	} {
		expVal, ok := exp[a]
		if !ok {
			continue
		}
		v, ok := got[a]
		if !ok {
			// let the caller fail
			keepKeys = append(keepKeys, a)
			continue
		}
		require.True(t, ok, "missing attribute %q", a)
		require.NotEmpty(t, v, "empty attribute %q", a)
		switch a {
		case livekit.AttrSIPCallID:
			require.True(t, strings.HasPrefix(v, guid.SIPCallPrefix))
		}
		if expVal != "" && expVal != AttrTestAny {
			require.Equal(t, expVal, v)
		}
		delete(exp, a)
		delete(got, a)
	}
	// remove extra attributes from comparison
	for key := range got {
		if slices.Contains(keepKeys, key) {
			continue
		}
		if _, ok := exp[key]; !ok {
			delete(got, key)
		}
	}
	return exp, got
}

type SIPOutboundTestParams struct {
	TrunkOut string            // trunk ID for outbound call
	RoomOut  string            // room for outbound call
	AttrsOut map[string]string // expected attributes for outbound participants
	TrunkIn  string            // trunk ID for inbound call
	RuleIn   string            // rule ID for inbound call
	AttrsIn  map[string]string // expected attributes for inbound participants
	NoDMTF   bool              // do not test DTMF
	RingFor  time.Duration     // do not pick up the call for this long
}

func loadVal[T any](ptr *atomic.Pointer[T]) T {
	p := ptr.Load()
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func TestSIPOutbound(t TB, ctx context.Context, lkOut, lkIn *LiveKit, params SIPOutboundTestParams) {
	t.Log("getting trunk info")

	trsOut, err := lkOut.SIP.GetSIPOutboundTrunksByIDs(ctx, []string{params.TrunkOut})
	require.NoError(t, err)
	trOut := trsOut[0]
	require.NotNil(t, trOut, "trunk not found")
	require.NotEmpty(t, trOut.Numbers, "no trunk numbers for outbound")
	numOut := trOut.Numbers[0]
	t.Logf("using outbound trunk %q (%s, num: %s)", trOut.Name, trOut.SipTrunkId, numOut)

	trsIn, err := lkIn.SIP.GetSIPInboundTrunksByIDs(ctx, []string{params.TrunkIn})
	require.NoError(t, err)
	trIn := trsIn[0]
	require.NotNil(t, trIn, "trunk not found")
	require.NotEmpty(t, trIn.Numbers, "no trunk numbers for inbound")
	numIn := trIn.Numbers[0]
	t.Logf("using inbound trunk %q (%s, num: %s)", trIn.Name, trIn.SipTrunkId, numIn)

	rulesIn, err := lkIn.SIP.GetSIPDispatchRulesByIDs(ctx, []string{params.RuleIn})
	require.NoError(t, err)
	ruleIn := rulesIn[0]
	require.NotNil(t, ruleIn, "rule not found")
	require.True(t, len(ruleIn.TrunkIds) == 0 || slices.Contains(ruleIn.TrunkIds, trIn.SipTrunkId), "selected rule doesn't match the trunk")
	ruleDir, ok := ruleIn.Rule.Rule.(*livekit.SIPDispatchRule_DispatchRuleDirect)
	require.True(t, ok, "unsupported dispatch rule type %T", ruleIn.Rule.Rule)
	rule := ruleDir.DispatchRuleDirect
	roomIn := rule.RoomName
	roomPin := rule.Pin
	if roomPin != "" {
		roomPin = "ww" + roomPin + "#"
	}
	t.Logf("using dispatch rule %q (%s, room: %s)", ruleIn.Name, ruleIn.SipDispatchRuleId, roomIn)

	const (
		outIdentity = "siptest_outbound"
		outName     = "Outbound Call"
		outMeta     = `{"test":true, "dir": "out"}`
	)
	var (
		inIdentity = "sip_" + numOut
		inName     = "Phone " + numOut
	)
	// Make sure we remove rooms when the test ends.
	// Some tests may reuse LK server, in which case the participants could stay in rooms for a long time.
	t.Cleanup(func() {
		_, _ = lkOut.Rooms.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: params.RoomOut})
		_, _ = lkIn.Rooms.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: roomIn})
	})
	// Make sure we delete inbound SIP participant. Outbound is deleted automatically by CreateSIPParticipant.
	t.Cleanup(func() {
		_, _ = lkIn.Rooms.RemoveParticipant(context.Background(), &livekit.RoomParticipantIdentity{
			Room: roomIn, Identity: inIdentity,
		})
	})

	const (
		identityTest = "test_probe"
	)

	var (
		dataOut    = make(chan lksdk.DataPacket, 20)
		dataIn     = make(chan lksdk.DataPacket, 20)
		callIDOut  atomic.Pointer[string]
		callIDIn   atomic.Pointer[string]
		statusOut  atomic.Pointer[string]
		statusIn   atomic.Pointer[string]
		ringingOut atomic.Uint64
		connected  atomic.Bool

		outRingStart time.Time
	)
	defer func() {
		if params.RingFor > 0 {
			ringedFor := time.Duration(ringingOut.Load())
			const (
				dtmin = 2 * time.Second
				dtmax = 3 * time.Second
			)
			if ringedFor < params.RingFor-dtmin || ringedFor > params.RingFor+dtmax {
				t.Errorf("unexpected ringing duration, exp: %v, got: %v", params.RingFor, ringedFor)
			} else {
				t.Logf("ringing duration: %v", ringedFor)
			}
		}
		if !t.Failed() {
			return
		}
		idIn := loadVal(&callIDIn)
		idOut := loadVal(&callIDOut)
		// Try explaining the test result.
		if connected.Load() {
			t.Errorf(`SIP connected, but media tests failed.

Check logs for calls:
@callID:%s (outbound)
@callID:%s (inbound)

Possible causes:
- Media ports are closed
- SDP negotiation failed
- DTMF failed`,
				idOut, idIn,
			)
			return
		}
		if idIn != "" && idOut != "" {
			t.Errorf(`SIP participants connected, but participant info check failed.

Check logs for calls:
@callID:%s (outbound, last state: %q)
@callID:%s (inbound, last state: %q)`,
				idOut, loadVal(&statusOut),
				idIn, loadVal(&statusIn),
			)
		} else if idOut != "" {
			t.Errorf(`Outbound call connected, but no inbound calls were received.

Check logs for call:
@callID:%s (outbound, last state: %q)

And search for dropped call for numbers:
@fromUser:%s (from)
@toUser:%s (to)

Possible causes:
- Signaling is broken
- Signaling port is closed
- Signaling IP / Contact / Via are incorrect
- Password authentication failed`,

				idOut, loadVal(&statusOut),
				numOut, numIn,
			)
		} else {
			t.Errorf(`Outbound call did not connect.

Check logs for call:
@callID:%s (outbound, last state: %q)`,

				idOut, loadVal(&statusOut),
			)
		}
	}()

	// LK participants that will generate/listen for audio.
	t.Log("connecting test participants")
	var (
		pOut     *Participant
		pIn      *Participant
		readyOut sync.WaitGroup
		readyIn  sync.WaitGroup
	)
	readyOut.Add(1)
	readyIn.Add(1)
	go func() {
		defer readyOut.Done()
		pOut = lkOut.ConnectParticipant(t, params.RoomOut, identityTest, &RoomParticipantCallback{
			RoomCallback: lksdk.RoomCallback{
				ParticipantCallback: lksdk.ParticipantCallback{
					OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
						select {
						case dataOut <- data:
						default:
						}
					},
				},
			},
			OnSIPStatus: func(p *lksdk.RemoteParticipant, callID string, status string) {
				callIDOut.Store(&callID)
				prev := statusOut.Swap(&status)
				if prev != nil {
					switch {
					case *prev == "dialing" && status == "ringing":
						outRingStart = time.Now()
					case *prev == "ringing" && status != "ringing":
						ringingOut.Store(uint64(time.Since(outRingStart)))
					}
				}
				t.Logf("sip outbound call %s (%s) status %v", callID, p.Identity(), status)
			},
		})
	}()
	go func() {
		defer readyIn.Done()
		if params.RingFor > 0 {
			t.Log("ringing for", params.RingFor)
			select {
			case <-ctx.Done():
				return
			case <-time.After(params.RingFor):
			}
		}
		pIn = lkIn.ConnectParticipant(t, roomIn, identityTest, &RoomParticipantCallback{
			RoomCallback: lksdk.RoomCallback{
				ParticipantCallback: lksdk.ParticipantCallback{
					OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
						select {
						case dataIn <- data:
						default:
						}
					},
				},
			},
			OnSIPStatus: func(p *lksdk.RemoteParticipant, callID string, status string) {
				callIDIn.Store(&callID)
				statusIn.Store(&status)
				t.Logf("sip inbound call %s (%s) status %v", callID, p.Identity(), status)
			},
		})
	}()

	// Start the outbound call. It should hit Trunk Provider and initiate an inbound call back to the second server.
	t.Log("creating sip participant")
	r := lkOut.CreateSIPParticipant(t, &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          params.TrunkOut,
		SipCallTo:           numIn,
		RoomName:            params.RoomOut,
		ParticipantIdentity: outIdentity,
		ParticipantName:     outName,
		ParticipantMetadata: outMeta,
		Dtmf:                roomPin,
	})
	t.Logf("outbound call ID: %s", r.SipCallId)

	readyOut.Wait()
	t.Log("waiting for outbound participant to become ready")
	expAttrsOut := map[string]string{
		"sip.callID":           r.SipCallId, // special case
		"sip.callTag":          AttrTestAny, // special case
		"sip.callIDFull":       AttrTestAny, // special case
		"sip.callStatus":       "active",
		"sip.trunkPhoneNumber": numOut,
		"sip.phoneNumber":      numIn,
		"sip.trunkID":          params.TrunkOut,
	}
	for k, v := range params.AttrsOut {
		expAttrsOut[k] = v
	}
	lkOut.ExpectRoomWithParticipants(t, ctx, params.RoomOut, []ParticipantInfo{
		{Identity: identityTest, Kind: livekit.ParticipantInfo_STANDARD},
		{
			Identity:   outIdentity,
			Name:       outName,
			Kind:       livekit.ParticipantInfo_SIP,
			Metadata:   outMeta,
			Attributes: expAttrsOut,
		},
	})
	readyIn.Wait()
	t.Log("waiting for inbound participant to become ready")
	expAttrsIn := map[string]string{
		"sip.callID":           AttrTestAny, // special case
		"sip.callTag":          AttrTestAny, // special case
		"sip.callIDFull":       AttrTestAny, // special case
		"sip.callStatus":       "active",
		"sip.trunkPhoneNumber": numIn,
		"sip.phoneNumber":      numOut,
		"sip.trunkID":          params.TrunkIn,
		"sip.ruleID":           params.RuleIn,
	}
	for k, v := range params.AttrsIn {
		expAttrsIn[k] = v
	}
	lkIn.ExpectRoomWithParticipants(t, ctx, roomIn, []ParticipantInfo{
		{Identity: identityTest, Kind: livekit.ParticipantInfo_STANDARD},
		{
			Identity:   inIdentity,
			Name:       inName,
			Kind:       livekit.ParticipantInfo_SIP,
			Metadata:   ruleIn.Metadata,
			Attributes: expAttrsIn,
		},
	})
	connected.Store(true)

	t.Log("testing audio")
	CheckAudioForParticipants(t, ctx, pOut, pIn)
	if !params.NoDMTF {
		t.Log("testing dtmf")
		CheckDTMFForParticipants(t, ctx, pOut, pIn, dataOut, dataIn)

		t.Log("retesting audio")
		CheckAudioForParticipants(t, ctx, pOut, pIn)
	}
}
