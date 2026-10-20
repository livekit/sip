// Copyright 2026 LiveKit, Inc.
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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

func TestCallStateEmitSignalingEvent(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewCallState(h, &livekit.SIPCallInfo{CallId: "SCL_test", SipCallId: "sip-id", TrunkId: "ST_test"})
	t0 := time.Now()
	st.EmitSignalingEvent(SignalingEvent{Type: EventInviteReceived, Time: t0})
	st.EmitSignalingEvent(SignalingEvent{Type: EventTryingSent}) // no time: dropped
	st.Update(func(info *livekit.SIPCallInfo) { info.RoomId = "RM_test" })
	st.EmitSignalingEvent(SignalingEvent{Type: EventRoomJoined, Time: t0})

	evs := h.Events()
	require.Equal(t, []SignalingEventType{EventInviteReceived, EventRoomJoined}, h.EventTypes())
	require.Equal(t, t0, evs[0].Time)
	require.Equal(t, "SCL_test", evs[0].CallID)
	require.Equal(t, "ST_test", evs[0].TrunkID)
	require.Empty(t, evs[0].RoomID, "room is unknown before it is joined")
	require.Equal(t, "RM_test", evs[1].RoomID)
}

func TestCallStateEmitSignalingEventIgnored(t *testing.T) {
	ev := SignalingEvent{Type: EventCallEnded, Time: time.Now()}
	require.NotPanics(t, func() {
		NewCallState(NewRPCStateHandler(nil), nil).EmitSignalingEvent(ev) // handler without event support
		(*CallState)(nil).EmitSignalingEvent(ev)
	})
}

// waitCallEnded waits for EventCallEnded and returns all events, checking they
// were emitted in time order.
func waitCallEnded(t *testing.T, h *recordingStateHandler) []SignalingEvent {
	t.Helper()
	require.Eventually(t, func() bool {
		types := h.EventTypes()
		return len(types) > 0 && types[len(types)-1] == EventCallEnded
	}, 5*time.Second, 10*time.Millisecond, "call end should be reported")
	evs := h.Events()
	for i := 1; i < len(evs); i++ {
		require.False(t, evs[i].Time.Before(evs[i-1].Time), "%s emitted after %s but happened earlier", evs[i].Type, evs[i-1].Type)
	}
	return evs
}

// sendInvite sends an INVITE and returns its client transaction.
func sendInvite(t *testing.T, st *serviceTest) sip.ClientTransaction {
	t.Helper()
	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)
	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)
	return tx
}

func TestInboundSignalingEventsAnswered(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
	call, ic := st.CreateInboundCall(t)
	require.Eventually(t, ic.started.IsBroken, 5*time.Second, 10*time.Millisecond)
	st.TestUA.TransactionRequest(t, call.NewRequest(sip.BYE), true)

	evs := waitCallEnded(t, h)
	require.Equal(t, []SignalingEventType{
		EventInviteReceived, EventTryingSent, EventRingingSent, EventRoomJoined, EventAnsweredSent, EventCallEnded,
	}, h.EventTypes())
	require.Equal(t, ic.sigTs.InviteTime, evs[0].Time, "INVITE keeps its arrival time")
	require.Equal(t, testRoomSID, evs[3].RoomID)
	require.Equal(t, "test-participant-id", evs[3].ParticipantID)
	require.False(t, evs[4].Time.After(ic.sigTs.AcceptTime), "200 OK is sent before Accept returns")
	require.Equal(t, string(stats.ResultSuccess), evs[5].Result)
	require.Equal(t, "hangup", evs[5].Reason)
}

func TestInboundSignalingEventsCancelledWhileRinging(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{
		GetRoom:         newTestRoomConfig(&testRoomConfig{ringForever: true}),
		GetStateHandler: h.GetStateHandler(),
	})
	tx := sendInvite(t, st)
	require.Eventually(t, func() bool {
		types := h.EventTypes()
		return len(types) > 0 && types[len(types)-1] == EventRoomJoined
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, tx.Cancel())

	evs := waitCallEnded(t, h)
	require.Equal(t, []SignalingEventType{
		EventInviteReceived, EventTryingSent, EventRingingSent, EventRoomJoined, EventCallEnded,
	}, h.EventTypes())
	ended := evs[len(evs)-1]
	require.Equal(t, 487, ended.StatusCode)
	require.Equal(t, "cancelled", ended.Reason)
	require.Equal(t, testRoomSID, ended.RoomID)
}

// INVITEs rejected before a call is set up still get a call.ended, classified
// like the termination metrics.
func TestInboundSignalingEventsRejected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		auth     func(context.Context, *rpc.SIPCall) (AuthInfo, error)
		status   int
		term     stats.Termination
		snapshot bool // whether call info is flushed upstream
	}{
		{
			name:     "no trunk",
			auth:     func(context.Context, *rpc.SIPCall) (AuthInfo, error) { return AuthInfo{Result: AuthNotFound}, nil },
			status:   404,
			term:     stats.ClientError("no-rule"),
			snapshot: true,
		},
		{
			// No call info: it would add RPCs to a backend that is already failing.
			name:   "auth lookup failed",
			auth:   func(context.Context, *rpc.SIPCall) (AuthInfo, error) { return AuthInfo{}, errors.New("unavailable") },
			status: 503,
			term:   stats.ServerError("auth-error"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingStateHandler{}
			st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
			st.Handler.(*TestHandler).GetAuthCredentialsFunc = tc.auth
			sendInvite(t, st)

			evs := waitCallEnded(t, h)
			require.Equal(t, []SignalingEventType{EventInviteReceived, EventTryingSent, EventCallEnded}, h.EventTypes())
			require.Equal(t, tc.status, evs[2].StatusCode)
			require.Equal(t, string(tc.term.Result), evs[2].Result)
			require.Equal(t, tc.term.Reason, evs[2].Reason)
			require.Equal(t, tc.snapshot, h.Last() != nil)
		})
	}
}
