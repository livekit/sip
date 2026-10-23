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
	"errors"
	"fmt"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sipgo/sip"

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
	// EventInviteAuthorized: the auth lookup for the INVITE answered (or
	// failed). Result is the lookup result (see authResultName) and Duration
	// the lookup time. Twice for a challenged call.
	EventInviteAuthorized SignalingEventType = "sip.invite.authorized"
	// EventRingingSent: the first 180 Ringing was sent.
	EventRingingSent SignalingEventType = "sip.ringing.sent"
	// EventCallDispatched: the dispatch rule lookup answered. Result is the
	// dispatch result (see dispatchResultName) and Duration the lookup time.
	// A call that asks for a PIN gets it a second time, with WithPIN set, after
	// the caller entered the PIN.
	EventCallDispatched SignalingEventType = "sip.call.dispatched"
	// EventRoomJoined: the SIP participant joined the LiveKit room.
	EventRoomJoined SignalingEventType = "sip.room.joined"
	// EventAnsweredSent: the first 200 OK for the INVITE was sent.
	EventAnsweredSent SignalingEventType = "sip.answered.sent"
	// EventAckReceived: the first ACK for the INVITE's 200 OK arrived.
	EventAckReceived SignalingEventType = "sip.ack.received"
	// EventCancelReceived: the caller cancelled the INVITE before it was answered.
	EventCancelReceived SignalingEventType = "sip.cancel.received"
	// EventRejected: a final non-2xx response was sent to the INVITE, with its
	// StatusCode and reason phrase in Reason. A 407 digest challenge is not a
	// rejection; a dropped INVITE gets no response, so no event. When a call
	// that was being set up is closed unanswered (e.g. 487 after a CANCEL), the
	// response goes out after EventCallEnded's time, as a BYE would.
	EventRejected SignalingEventType = "sip.rejected"
	// EventByeReceived: the remote side hung up with a BYE. Reason is the
	// cause from its Reason header (e.g. "Q.850;cause=16"), if any.
	EventByeReceived SignalingEventType = "sip.bye.received"
	// EventByeSent: we hung up with a BYE. Time is when it was sent, which is
	// after EventCallEnded's time (the call ends when we decide to end it); the
	// event is emitted when the BYE transaction finishes. Result is the outcome
	// ("ok", "error" or "timeout"), StatusCode the response (0 if none) and
	// Duration the time to it.
	EventByeSent SignalingEventType = "sip.bye.sent"
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
	// DispatchRuleID is copied from the call state unless the event sets it.
	DispatchRuleID string
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
	// Result is the outcome of the step. On EventCallEnded it is one of the
	// stats.TerminationResult values ("success", "client_error",
	// "server_error", "upstream_error", "indeterminate"); the event types that
	// set it document their own values.
	Result string
	// Duration is how long the step took, for events that time one (lookups,
	// the BYE transaction).
	Duration time.Duration
	// WithPIN marks the second EventCallDispatched of a PIN call.
	WithPIN bool
}

// authResultName names an auth lookup result for EventInviteAuthorized.
func authResultName(r AuthResult) string {
	switch r {
	case AuthNotFound:
		return "not-found"
	case AuthDrop:
		return "drop"
	case AuthPassword:
		return "password"
	case AuthAccept:
		return "accept"
	case AuthQuotaExceeded:
		return "quota-exceeded"
	case AuthNoTrunkFound:
		return "no-trunk"
	case AuthRouteNotAllowed:
		return "route-not-allowed"
	case AuthRejectedAsError:
		return "rejected-as-error"
	default:
		return "other"
	}
}

// dispatchResultName names a dispatch result for EventCallDispatched.
func dispatchResultName(r DispatchResult) string {
	switch r {
	case DispatchAccept:
		return "accept"
	case DispatchRequestPin:
		return "request-pin"
	case DispatchNoRuleReject:
		return "no-rule-reject"
	case DispatchNoRuleDrop:
		return "no-rule-drop"
	case DispatchServiceUnavailable:
		return "service-unavailable"
	default:
		return "other"
	}
}

// lookupEvent builds EventInviteAuthorized or EventCallDispatched for a lookup
// that took d and answered at t.
func lookupEvent(typ SignalingEventType, t time.Time, d time.Duration, result string) SignalingEvent {
	return SignalingEvent{Type: typ, Time: t, Result: result, Duration: d}
}

// byeOutcome classifies the result of a BYE transaction for EventByeSent.
func byeOutcome(res *sip.Response, err error) string {
	switch {
	case byeSucceeded(res, err):
		return "ok"
	case res != nil:
		return "error"
	case errors.As(err, &transactionTimeoutError{}) || errors.Is(err, ErrSIPRequestTimeout):
		return "timeout"
	default:
		return "error"
	}
}

// reasonCause renders a Reason header as "<protocol>;cause=<code>" for
// events. Its free text is left out: the remote side controls it.
func reasonCause(r ReasonHeader) string {
	if r.IsZero() {
		return ""
	}
	return fmt.Sprintf("%s;cause=%d", r.Type, r.Cause)
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
