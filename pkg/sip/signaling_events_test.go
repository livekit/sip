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
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
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

// waitCallEnded waits for EventCallEnded and returns all events ordered by
// Time, the order consumers use. Emission order differs: some events are
// emitted after they happen, and some after EventCallEnded (e.g. a CANCEL is
// reported only once the call goroutine was signalled).
func waitCallEnded(t *testing.T, h *recordingStateHandler) []SignalingEvent {
	t.Helper()
	require.Eventually(t, func() bool {
		return slices.Contains(h.EventTypes(), EventCallEnded)
	}, 5*time.Second, 10*time.Millisecond, "call end should be reported")
	return eventsByTime(h)
}

// waitEventTypes waits until the call's events, ordered by Time, have exactly
// the types in want, and returns them.
func waitEventTypes(t *testing.T, h *recordingStateHandler, want []SignalingEventType) []SignalingEvent {
	t.Helper()
	var got []SignalingEventType
	if !assert.Eventually(t, func() bool {
		got = eventTypes(eventsByTime(h))
		return slices.Equal(got, want)
	}, 5*time.Second, 10*time.Millisecond) {
		require.Equal(t, want, got, "events by time")
	}
	return eventsByTime(h)
}

// eventsByTime returns the recorded events ordered by Time.
func eventsByTime(h *recordingStateHandler) []SignalingEvent {
	evs := h.Events()
	slices.SortStableFunc(evs, func(a, b SignalingEvent) int { return a.Time.Compare(b.Time) })
	return evs
}

// eventTypes lists the types of evs, in order.
func eventTypes(evs []SignalingEvent) []SignalingEventType {
	types := make([]SignalingEventType, len(evs))
	for i, ev := range evs {
		types[i] = ev.Type
	}
	return types
}

// eventOf returns the first event of type typ.
func eventOf(t *testing.T, evs []SignalingEvent, typ SignalingEventType) SignalingEvent {
	t.Helper()
	i := slices.IndexFunc(evs, func(ev SignalingEvent) bool { return ev.Type == typ })
	require.GreaterOrEqual(t, i, 0, "no %s event", typ)
	return evs[i]
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

	evs := waitEventTypes(t, h, []SignalingEventType{
		EventInviteReceived, EventTryingSent, EventInviteAuthorized, EventRingingSent, EventCallDispatched,
		EventRoomJoined, EventAnsweredSent, EventAckReceived, EventByeReceived, EventCallEnded,
	})
	require.Equal(t, ic.sigTs.InviteTime, evs[0].Time, "INVITE keeps its arrival time")

	auth := eventOf(t, evs, EventInviteAuthorized)
	require.Equal(t, "accept", auth.Result)
	require.NotZero(t, auth.Duration)
	disp := eventOf(t, evs, EventCallDispatched)
	require.Equal(t, "accept", disp.Result)
	require.NotZero(t, disp.Duration)

	joined := eventOf(t, evs, EventRoomJoined)
	require.Equal(t, testRoomSID, joined.RoomID)
	require.Equal(t, "test-participant-id", joined.ParticipantID)
	require.False(t, eventOf(t, evs, EventAnsweredSent).Time.After(ic.sigTs.AcceptTime), "200 OK is sent before Accept returns")

	ended := eventOf(t, evs, EventCallEnded)
	require.Equal(t, string(stats.ResultSuccess), ended.Result)
	require.Equal(t, "hangup", ended.Reason)
}

func TestInboundSignalingEventsLiveKitHangsUp(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
	call, ic := st.CreateInboundCall(t)
	byes := call.RegisterRequestChannel(string(sip.BYE))
	t.Cleanup(func() { call.UnregisterRequestChannel(string(sip.BYE)) })
	require.Eventually(t, ic.started.IsBroken, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, ic.EndCall(t.Context(), nil))
	select {
	case msg := <-byes:
		require.NoError(t, msg.tx.Respond(sip.NewResponseFromRequest(msg.req, sip.StatusOK, "OK", nil)))
	case <-time.After(5 * time.Second):
		t.Fatal("no BYE from the server")
	}

	evs := waitCallEnded(t, h)
	types := eventTypes(evs)
	// The call ends when we decide to end it, before the BYE goes out.
	require.Equal(t, []SignalingEventType{EventCallEnded, EventByeSent}, types[len(types)-2:])
	bye := eventOf(t, evs, EventByeSent)
	require.Equal(t, "ok", bye.Result)
	require.Equal(t, 200, bye.StatusCode)
	require.NotZero(t, bye.Duration)
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

	evs := waitEventTypes(t, h, []SignalingEventType{
		EventInviteReceived, EventTryingSent, EventInviteAuthorized, EventRingingSent, EventCallDispatched,
		EventRoomJoined, EventCancelReceived, EventCallEnded, EventRejected,
	})
	require.Equal(t, 487, eventOf(t, evs, EventRejected).StatusCode)
	ended := eventOf(t, evs, EventCallEnded)
	require.Equal(t, 487, ended.StatusCode)
	require.Equal(t, "cancelled", ended.Reason)
	require.Equal(t, testRoomSID, ended.RoomID)
}

// INVITEs rejected before a call is set up still get a call.ended, classified
// like the termination metrics.
func TestInboundSignalingEventsRejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		auth       func(context.Context, *rpc.SIPCall) (AuthInfo, error)
		authResult string
		status     int
		term       stats.Termination
		snapshot   bool // whether call info is flushed upstream
	}{
		{
			name:       "no trunk",
			auth:       func(context.Context, *rpc.SIPCall) (AuthInfo, error) { return AuthInfo{Result: AuthNotFound}, nil },
			authResult: "not-found",
			status:     404,
			term:       stats.ClientError("no-rule"),
			snapshot:   true,
		},
		{
			// No call info: it would add RPCs to a backend that is already failing.
			name:       "auth lookup failed",
			auth:       func(context.Context, *rpc.SIPCall) (AuthInfo, error) { return AuthInfo{}, errors.New("unavailable") },
			authResult: "error",
			status:     503,
			term:       stats.ServerError("auth-error"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingStateHandler{}
			st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
			st.Handler.(*TestHandler).GetAuthCredentialsFunc = tc.auth
			sendInvite(t, st)

			evs := waitEventTypes(t, h, []SignalingEventType{
				EventInviteReceived, EventTryingSent, EventInviteAuthorized, EventRejected, EventCallEnded,
			})
			auth := eventOf(t, evs, EventInviteAuthorized)
			require.Equal(t, tc.authResult, auth.Result)
			require.NotZero(t, auth.Duration)
			require.Equal(t, tc.status, eventOf(t, evs, EventRejected).StatusCode)
			ended := eventOf(t, evs, EventCallEnded)
			require.Equal(t, tc.status, ended.StatusCode)
			require.Equal(t, string(tc.term.Result), ended.Result)
			require.Equal(t, tc.term.Reason, ended.Reason)
			require.Equal(t, tc.snapshot, h.Last() != nil)
		})
	}
}

func TestByeOutcome(t *testing.T) {
	req := sip.NewRequest(sip.BYE, sip.Uri{Host: "example.com"})
	for _, tc := range []struct {
		name string
		res  *sip.Response
		err  error
		want string
	}{
		{name: "200", res: sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil), want: "ok"},
		{name: "481", res: sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "", nil), want: "error"},
		{name: "no response", err: psrpc.NewError(psrpc.Canceled, transactionTimeoutError{}), want: "timeout"},
		{name: "deadline", err: psrpc.NewError(psrpc.Canceled, ErrSIPRequestTimeout), want: "timeout"},
		{name: "not sent", err: errors.New("no route"), want: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, byeOutcome(tc.res, tc.err))
		})
	}
}

func TestReasonCause(t *testing.T) {
	require.Empty(t, reasonCause(ReasonHeader{}))
	// The free text is left out: the remote side controls it.
	require.Equal(t, "Q.850;cause=16", reasonCause(ReasonHeader{Type: "Q.850", Cause: 16, Text: "Normal call clearing"}))
}

// A PIN call is dispatched twice: once to learn a PIN is needed, and again
// with the PIN the caller entered. Each dispatch event reports its own rule.
func TestInboundSignalingEventsPIN(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accept   bool   // the PIN is right
		wantRule string // rule of the second dispatch event
	}{
		{name: "right PIN", accept: true, wantRule: "SDR_pin"},
		{name: "wrong PIN", accept: false, wantRule: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingStateHandler{}
			st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
			st.Handler.(*TestHandler).DispatchCallFunc = func(_ context.Context, info *CallInfo) CallDispatch {
				if info.Pin == "" {
					return CallDispatch{Result: DispatchRequestPin, DispatchRuleID: "SDR_pin"}
				}
				if !tc.accept {
					return CallDispatch{Result: DispatchNoRuleReject}
				}
				identity := "test-participant-" + info.Call.SipCallId
				return CallDispatch{
					Result:         DispatchAccept,
					DispatchRuleID: "SDR_pin",
					Room: RoomConfig{
						RoomName:    "test-room",
						Participant: ParticipantConfig{Identity: identity, Name: identity},
					},
				}
			}
			call, ic := st.CreateInboundCall(t) // the 200 OK comes before the PIN prompt
			byes := call.RegisterRequestChannel(string(sip.BYE))
			t.Cleanup(func() { call.UnregisterRequestChannel(string(sip.BYE)) })
			for _, d := range []byte{'1', '2', '#'} {
				ic.dtmf <- dtmf.Event{Digit: d}
			}
			if tc.accept {
				require.Eventually(t, ic.started.IsBroken, 10*time.Second, 10*time.Millisecond)
				st.TestUA.TransactionRequest(t, call.NewRequest(sip.BYE), true)
			} else {
				// The call was answered for the prompt, so the server hangs up with a BYE.
				select {
				case msg := <-byes:
					require.NoError(t, msg.tx.Respond(sip.NewResponseFromRequest(msg.req, sip.StatusOK, "OK", nil)))
				case <-time.After(10 * time.Second):
					t.Fatal("no BYE from the server")
				}
			}

			evs := waitCallEnded(t, h)
			var disps []SignalingEvent
			for _, ev := range evs {
				if ev.Type == EventCallDispatched {
					disps = append(disps, ev)
				}
			}
			require.Len(t, disps, 2)
			require.Equal(t, "request-pin", disps[0].Result)
			require.False(t, disps[0].WithPIN)
			require.Equal(t, "SDR_pin", disps[0].DispatchRuleID)
			require.True(t, disps[1].WithPIN)
			require.NotZero(t, disps[1].Duration)
			require.Equal(t, tc.wantRule, disps[1].DispatchRuleID, "the first lookup's rule must not be inherited")
		})
	}
}

// Bad digest credentials get a 401, which handleInviteAuth sends itself.
func TestInboundSignalingEventsBadCredentials(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
	st.Handler.(*TestHandler).GetAuthCredentialsFunc = func(context.Context, *rpc.SIPCall) (AuthInfo, error) {
		return AuthInfo{Result: AuthPassword, Auth: InboundAuth{Username: "user", Password: "secret"}}, nil
	}
	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)
	req.AppendHeader(sip.NewHeader("Proxy-Authorization",
		`Digest username="wrong", realm="livekit", nonce="n", uri="sip:test@example.com", response="r"`))
	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)

	evs := waitEventTypes(t, h, []SignalingEventType{
		EventInviteReceived, EventTryingSent, EventInviteAuthorized, EventRejected, EventCallEnded,
	})
	require.Equal(t, "password", eventOf(t, evs, EventInviteAuthorized).Result)
	require.Equal(t, 401, eventOf(t, evs, EventRejected).StatusCode)
}

func TestSIPInboundAckEvent(t *testing.T) {
	ack := func(cseq uint32) *sip.Request {
		req := sip.NewRequest(sip.ACK, sip.Uri{Host: "example.com"})
		req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.ACK})
		return req
	}
	newLeg := func(okSent bool) (*sipInbound, *recordingStateHandler) {
		h := &recordingStateHandler{}
		c := &sipInbound{log: logger.GetLogger(), inviteCSeq: 1}
		c.state.Store(NewCallState(h, &livekit.SIPCallInfo{CallId: "SCL_test"}))
		c.okSending.Store(okSent)
		c.okSent.Store(okSent)
		return c, h
	}

	c, h := newLeg(true)
	c.AcceptAck(ack(2), nil) // for another INVITE
	require.Empty(t, h.Events())
	require.False(t, c.GotACK())

	c.AcceptAck(ack(1), nil)
	c.AcceptAck(ack(1), nil) // a retransmission
	require.Equal(t, []SignalingEventType{EventAckReceived}, h.EventTypes())
	require.Equal(t, uint32(1), h.Events()[0].CSeq)
	require.True(t, c.GotACK())

	// The ACK for an error response (e.g. the 487 after a CANCEL) is not reported.
	c, h = newLeg(false)
	c.AcceptAck(ack(1), nil)
	c.reportAck()
	require.Empty(t, h.Events())

	// An ACK that arrives before Accept records the 200 OK is reported once
	// Accept returns, with its own arrival time.
	c, h = newLeg(false)
	c.okSending.Store(true) // the 200 OK write started
	before := time.Now()
	c.AcceptAck(ack(1), nil)
	require.Empty(t, h.Events())
	c.okSent.Store(true)
	c.reportAck() // as acceptCall does after Accept returns
	c.AcceptAck(ack(1), nil)
	require.Equal(t, []SignalingEventType{EventAckReceived}, h.EventTypes())
	require.WithinRange(t, h.Events()[0].Time, before.Round(0), time.Now(), "the ACK keeps its arrival time")

	// A premature ACK (before any 200 OK) is not reported as the 200 OK's ACK;
	// the real one that follows is.
	c, h = newLeg(false)
	c.AcceptAck(ack(1), nil)
	c.okSending.Store(true)
	c.okSent.Store(true)
	c.reportAck()
	require.Empty(t, h.Events())
	require.True(t, c.GotACK(), "the call flow still sees the premature ACK")
	before = time.Now()
	c.AcceptAck(ack(1), nil)
	require.Equal(t, []SignalingEventType{EventAckReceived}, h.EventTypes())
	require.False(t, h.Events()[0].Time.Before(before.Round(0)), "the event has the real ACK's time")
}

// A peer that sends a second BYE (a new transaction, not a retransmission)
// before the call is closed still hung up only once.
func TestInboundSignalingEventsSecondBye(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
	call, ic := st.CreateInboundCall(t)
	require.Eventually(t, ic.started.IsBroken, 5*time.Second, 10*time.Millisecond)
	for range 2 {
		bye := call.NewRequest(sip.BYE)
		st.Server.onBye(nil, bye, &testSIPServerTransaction{
			log:       logger.GetLogger(),
			req:       bye,
			responses: make(chan *sip.Response, 1),
			done:      make(chan struct{}),
		})
	}

	evs := waitCallEnded(t, h)
	n := 0
	for _, ev := range evs {
		if ev.Type == EventByeReceived {
			n++
		}
	}
	require.Equal(t, 1, n)
}

// When the BYE on the INVITE's connection is rejected and the retry via
// Contact succeeds, the event reports the retry.
func TestInboundSignalingEventsByeRetry(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
	call, ic := st.CreateInboundCall(t, overTCP)
	byes := call.RegisterRequestChannel("BYE")
	require.NoError(t, ic.Close())
	expectBye(t, byes, 404, "Not Found")
	retry := expectBye(t, byes, 200, "OK")

	bye := eventOf(t, waitCallEnded(t, h), EventByeSent)
	require.Equal(t, "ok", bye.Result)
	require.Equal(t, 200, bye.StatusCode)
	require.Equal(t, retry.CSeq().SeqNo, bye.CSeq)
}
