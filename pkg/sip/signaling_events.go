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
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/sip/pkg/stats"
)

// SignalingEventType names one step in a call's signaling lifecycle.
type SignalingEventType string

const (
	// EventInviteReceived: an initial INVITE arrived. Emitted once the call
	// state exists (after the auth lookup), with the original arrival time.
	//
	// A call answered with a 407 digest challenge gets this event (and
	// EventTryingSent) twice under the same CallID: once for the challenged
	// INVITE and once for the retry with credentials, told apart by CSeq. Both
	// INVITEs really were received. The call still ends with exactly one
	// EventCallEnded: from the retry when the call finishes, or, if no retry
	// arrives within the challenge timeout, from the challenged INVITE.
	// Measure setup latency from the first INVITE, as the carrier sees it.
	EventInviteReceived SignalingEventType = "sip.invite.received"
	// EventTryingSent: 100 Trying was sent for the INVITE. Twice for a
	// challenged call; see EventInviteReceived.
	EventTryingSent SignalingEventType = "sip.trying.sent"
	// EventRingingSent: the first 180 Ringing was sent.
	EventRingingSent SignalingEventType = "sip.ringing.sent"
	// EventRoomJoined: the SIP participant joined the LiveKit room.
	EventRoomJoined SignalingEventType = "sip.room.joined"
	// EventAnsweredSent: the first 200 OK for the INVITE was sent.
	EventAnsweredSent SignalingEventType = "sip.answered.sent"
	// EventCallEnded: the call is over, for any reason, including rejections
	// before a call was set up.
	EventCallEnded SignalingEventType = "sip.call.ended"
)

// SignalingEvent is one step in a call's signaling lifecycle.
//
// Callers set Type, Time and the event-specific fields. CallState fills in
// the call identifiers when the event is emitted.
type SignalingEvent struct {
	Type SignalingEventType
	// Time is when the step happened, not when the event was emitted. Some
	// events are emitted later than they happen, so order a call's events by Time.
	Time time.Time

	// Call identifiers, copied from the call state at emit time.
	CallID    string
	SIPCallID string
	Direction livekit.SIPCallDirection
	TrunkID   string
	RoomID    string
	// ParticipantID is the SID of the SIP participant in the room. It is set on
	// EventRoomJoined, so the call joins the room's participant events.
	ParticipantID string

	// SIP message details, set when the event corresponds to a SIP message.
	SIPMethod string
	// StatusCode is the SIP status of the response. On EventCallEnded it is the
	// final response sent to the INVITE: 200 for every answered call, however
	// it ended later. Result and Reason say how the call ended.
	StatusCode int
	CSeq       uint32
	Transport  Transport

	// Reason says why the step happened. For EventCallEnded it is the
	// termination reason used in metrics (e.g. "hangup", "no-trunk", "no-ack").
	Reason string
	// Result classifies the end of the call, so it is set on EventCallEnded only:
	// one of the stats.TerminationResult values ("success", "client_error",
	// "server_error", "upstream_error", "indeterminate").
	Result string
}

// inviteResponseEvent builds an event for a response sent to the INVITE.
func inviteResponseEvent(typ SignalingEventType, t time.Time, statusCode int, cseq uint32) SignalingEvent {
	return SignalingEvent{Type: typ, Time: t, SIPMethod: "INVITE", StatusCode: statusCode, CSeq: cseq}
}

// callEndedEvent builds EventCallEnded from the call's termination and the
// final status sent for the INVITE (0 if none was sent).
func callEndedEvent(t time.Time, term stats.Termination, statusCode int) SignalingEvent {
	return SignalingEvent{
		Type:       EventCallEnded,
		Time:       t,
		StatusCode: statusCode,
		Result:     string(term.Result),
		Reason:     term.Reason,
	}
}
