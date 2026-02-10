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
	"fmt"
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

type SIPOutboundRequestTestParams struct {
	Req                     *livekit.CreateSIPParticipantRequest
	RingFor                 time.Duration
	MediaEncryption         bool
	ExpectEncryptionFailure bool
}

type SIPOutboundRequestTestIDs struct {
	CallID        string
	SipCallID     string
	PatricipantID string
	RoomID        string
	TrunkID       string
}

func getInboundTrunksByNumbers(ctx context.Context, lkIn *LiveKit, numbers []string) ([]*livekit.SIPInboundTrunkInfo, error) {
	trsIn, err := lkIn.SIP.ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{
		Numbers: numbers,
	})
	if err != nil {
		return nil, err
	}
	if len(trsIn.Items) == 0 {
		return nil, fmt.Errorf("no trunks found for numbers: %v", numbers)
	}
	return trsIn.Items, nil
}

func getDispatchRulesByTrunks(ctx context.Context, lkIn *LiveKit, trunks []*livekit.SIPInboundTrunkInfo) ([]*livekit.SIPDispatchRuleInfo, error) {
	ids := make([]string, len(trunks))
	for i, tr := range trunks {
		ids[i] = tr.SipTrunkId
	}
	resp, err := lkIn.SIP.ListSIPDispatchRule(ctx, &livekit.ListSIPDispatchRuleRequest{
		TrunkIds: ids,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Items) == 0 {
		return nil, fmt.Errorf("no dispatch rules found for trunks: %v", ids)
	}
	return resp.Items, nil
}

type roomIDFunc func(ctx context.Context, lk *LiveKit, rule *livekit.SIPDispatchRuleInfo, req *livekit.CreateSIPParticipantRequest) (string, error)

func TestSIPOutboundRequest(t TB, ctx context.Context, lkOut, lkIn *LiveKit, params SIPOutboundRequestTestParams) (outIDs, inIDs *SIPOutboundRequestTestIDs, err error) {
	require.Equal(t, "", params.Req.SipTrunkId, "SipTrunkId must be empty")
	require.NotNil(t, params.Req.Trunk, "A trunk must be inlined")
	require.NotEmpty(t, params.Req.Trunk.DestinationCountry, "DestinationCountry must be set")
	require.NotEmpty(t, params.Req.SipCallTo, "SipCallTo must be set")
	require.NotEmpty(t, params.Req.RoomName, "RoomName must be set")
	t.Logf("Calling from %s@%s to %s@%s", params.Req.SipNumber, params.Req.Trunk.DestinationCountry, params.Req.SipCallTo, params.Req.Trunk.Hostname)
	t.Logf("Using outbound room %q", params.Req.RoomName)

	trsIn, err := getInboundTrunksByNumbers(ctx, lkIn, []string{params.Req.SipCallTo})
	if err != nil {
		return nil, nil, err
	}
	trIn := trsIn[0]
	t.Logf("Expecting call in inbound trunk %q (%s, via number: %s)", trIn.Name, trIn.SipTrunkId, params.Req.SipCallTo)

	rulesIn, err := getDispatchRulesByTrunks(ctx, lkIn, trsIn)
	if err != nil {
		return nil, nil, err
	}
	ruleIn := rulesIn[0]
	t.Logf("Expecting call in dispatch rule %q (%s)", ruleIn.Name, ruleIn.SipDispatchRuleId)

	var getRoomID roomIDFunc
	if _, ok := ruleIn.Rule.Rule.(*livekit.SIPDispatchRule_DispatchRuleIndividual); ok {
		// Inbound room name is dynamic: e2e_{SipNumber}_{guid}. SipNumber is unique per test, so we
		// create the outbound call first, then poll for a room whose name has prefix "e2e_"+SipNumber+"_".
		getRoomID = getRoomFromIndividualRule
	} else if _, ok := ruleIn.Rule.Rule.(*livekit.SIPDispatchRule_DispatchRuleDirect); ok {
		// Inbound room name is fixed. Just return the name.
		getRoomID = getRoomFromDirectRule
		require.False(t, ok, "Using direct rule does not support concurrent tests")
	}
	if getRoomID == nil {
		return nil, nil, fmt.Errorf("unsupported dispatch rule type %T", ruleIn.Rule.Rule)
	}

	t.Logf("DEBUG: Connecting local outbound participant")

	// 1. Connect local audio to outbound room
	roomOut := params.Req.RoomName
	const identityTest = "test_probe"
	dataOut := make(chan lksdk.DataPacket, 20)
	dataIn := make(chan lksdk.DataPacket, 20)
	var (
		pOut     *Participant
		pIn      *Participant
		readyOut sync.WaitGroup
	)
	readyOut.Add(1)
	go func() {
		defer readyOut.Done()
		pOut = lkOut.ConnectParticipant(t, roomOut, identityTest, &RoomParticipantCallback{
			RoomCallback: lksdk.RoomCallback{
				ParticipantCallback: lksdk.ParticipantCallback{
					OnDataPacket: func(data lksdk.DataPacket, _ lksdk.DataReceiveParams) {
						select {
						case dataOut <- data:
						default:
						}
					},
				},
			},
		})
	}()
	readyOut.Wait()

	t.Logf("DEBUG: Connecting outbound call")

	// 2. Create outbound SIP participant (triggers inbound call and dynamic room creation)
	req := &livekit.CreateSIPParticipantRequest{
		Trunk:     params.Req.Trunk,
		SipCallTo: params.Req.SipCallTo,
		SipNumber: params.Req.SipNumber,
		RoomName:  params.Req.RoomName,
	}
	const outIdentity = "siptest_outbound"
	const outName = "Outbound Call"
	const outMeta = `{"test":true, "dir": "out"}`
	req.ParticipantIdentity = outIdentity
	req.ParticipantName = outName
	req.ParticipantMetadata = outMeta
	if params.MediaEncryption {
		req.MediaEncryption = livekit.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_REQUIRE
	} else {
		req.MediaEncryption = livekit.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_DISABLE
	}
	if params.ExpectEncryptionFailure {
		req.WaitUntilAnswered = true // We expect this to get rejected
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, createErr := lkOut.SIP.CreateSIPParticipant(ctx, req)
		require.Error(t, createErr)
		sipStatus := lksdk.SIPStatusFrom(createErr)
		require.NotNil(t, sipStatus)
		require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_INTERNAL_SERVER_ERROR, sipStatus.Code)
		return outIDs, nil, nil // Success!
	}
	r := lkOut.CreateSIPParticipant(t, req)
	outIDs = &SIPOutboundRequestTestIDs{
		CallID: r.SipCallId, SipCallID: r.SipCallId,
		PatricipantID: r.ParticipantIdentity, RoomID: roomOut, TrunkID: "",
	}

	t.Logf("DEBUG: Finding inbound room")

	// 3. Find inbound room by prefix (poll until it appears)
	roomIn, err := getRoomID(ctx, lkIn, ruleIn, req)
	if err != nil {
		return outIDs, nil, fmt.Errorf("find inbound room: %w", err)
	}
	t.Logf("DEBUG: Found inbound room: %s", roomIn)

	t.Cleanup(func() {
		_, _ = lkOut.Rooms.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: roomOut})
		_, _ = lkIn.Rooms.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{Room: roomIn})
	})
	inIdentity := "sip_" + params.Req.SipNumber
	t.Cleanup(func() {
		_, _ = lkIn.Rooms.RemoveParticipant(context.Background(), &livekit.RoomParticipantIdentity{
			Room: roomIn, Identity: inIdentity,
		})
	})

	t.Logf("DEBUG: Connecting local audio to inbound room: %s", roomIn)

	// 4. Connect local audio to inbound room (optionally after RingFor delay)
	var readyIn sync.WaitGroup
	readyIn.Add(1)
	go func() {
		defer readyIn.Done()
		if params.RingFor > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(params.RingFor):
			}
		}
		pIn = lkIn.ConnectParticipant(t, roomIn, identityTest, &RoomParticipantCallback{
			RoomCallback: lksdk.RoomCallback{
				ParticipantCallback: lksdk.ParticipantCallback{
					OnDataPacket: func(data lksdk.DataPacket, _ lksdk.DataReceiveParams) {
						select {
						case dataIn <- data:
						default:
						}
					},
				},
			},
		})
	}()
	readyIn.Wait()

	t.Logf("DEBUG: Asserting outbound room")

	// Assert outbound room
	expAttrsOut := map[string]string{
		livekit.AttrSIPCallID:                 r.SipCallId,
		livekit.AttrSIPPrefix + "callTag":     AttrTestAny,
		livekit.AttrSIPPrefix + "callIDFull":  AttrTestAny,
		livekit.AttrSIPPrefix + "callStatus":  "active",
		livekit.AttrSIPPrefix + "phoneNumber": params.Req.SipCallTo,
	}
	lkOut.ExpectRoomWithParticipants(t, ctx, roomOut, []ParticipantInfo{
		{Identity: identityTest, Kind: livekit.ParticipantInfo_STANDARD},
		{Identity: outIdentity, Name: outName, Kind: livekit.ParticipantInfo_SIP, Metadata: outMeta, Attributes: expAttrsOut},
	})

	t.Logf("DEBUG: Asserting inbound room")

	// Assert inbound room
	inName := "Phone " + params.Req.SipNumber
	expAttrsIn := map[string]string{
		livekit.AttrSIPPrefix + "callTag":          AttrTestAny,
		livekit.AttrSIPPrefix + "callIDFull":       AttrTestAny,
		livekit.AttrSIPPrefix + "callStatus":       "active",
		livekit.AttrSIPPrefix + "trunkPhoneNumber": params.Req.SipCallTo,
		livekit.AttrSIPPrefix + "phoneNumber":      params.Req.SipNumber,
		livekit.AttrSIPPrefix + "trunkID":          trIn.SipTrunkId,
		livekit.AttrSIPPrefix + "ruleID":           ruleIn.SipDispatchRuleId,
	}
	lkIn.ExpectRoomWithParticipants(t, ctx, roomIn, []ParticipantInfo{
		{Identity: identityTest, Kind: livekit.ParticipantInfo_STANDARD},
		{Identity: inIdentity, Name: inName, Kind: livekit.ParticipantInfo_SIP, Metadata: ruleIn.Metadata, Attributes: expAttrsIn},
	})

	// Fill inIDs from inbound room
	participants := lkIn.RoomParticipants(t, roomIn)
	for _, p := range participants {
		if p.Identity == inIdentity {
			inIDs = &SIPOutboundRequestTestIDs{
				RoomID: roomIn, TrunkID: trIn.SipTrunkId, PatricipantID: p.Sid,
			}
			if v := maps.Clone(p.Attributes)[livekit.AttrSIPCallID]; v != "" {
				inIDs.CallID, inIDs.SipCallID = v, v
			}
			break
		}
	}

	t.Log("DEBUG: testing audio")
	CheckAudioForParticipants(t, ctx, pOut, pIn)

	t.Log("DEBUG: testing dtmf")
	CheckDTMFForParticipants(t, ctx, pOut, pIn, dataOut, dataIn)

	t.Log("DEBUG: retesting audio")
	CheckAudioForParticipants(t, ctx, pOut, pIn)

	return outIDs, inIDs, nil
}

func getRoomFromDirectRule(ctx context.Context, lk *LiveKit, rule *livekit.SIPDispatchRuleInfo, req *livekit.CreateSIPParticipantRequest) (string, error) {
	directRule, ok := rule.Rule.Rule.(*livekit.SIPDispatchRule_DispatchRuleDirect)
	if !ok {
		return "", fmt.Errorf("invalid rule type type %T", rule.Rule.Rule)
	}
	return directRule.DispatchRuleDirect.RoomName, nil
}

func getRoomFromIndividualRule(ctx context.Context, lk *LiveKit, rule *livekit.SIPDispatchRuleInfo, req *livekit.CreateSIPParticipantRequest) (string, error) {
	indvRule, ok := rule.Rule.Rule.(*livekit.SIPDispatchRule_DispatchRuleIndividual)
	if !ok {
		return "", fmt.Errorf("invalid rule type type %T", rule.Rule.Rule)
	}
	inboundRoomPrefix := indvRule.DispatchRuleIndividual.RoomPrefix + "_" + req.SipNumber + "_"

	const pollInterval = 250 * time.Millisecond
	for {
		resp, err := lk.Rooms.ListRooms(ctx, &livekit.ListRoomsRequest{})
		if err != nil {
			return "", err
		}
		for _, room := range resp.Rooms {
			if strings.HasPrefix(room.Name, inboundRoomPrefix) {
				return room.Name, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
