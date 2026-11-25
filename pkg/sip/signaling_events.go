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

// Inbound calls.
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
)

// Outbound calls.
const (
	// EventCallRequested: CreateSIPParticipant asked for the call. Time is when
	// the request arrived; the INVITE follows once the room is joined.
	EventCallRequested SignalingEventType = "sip.call.requested"
	// EventInviteSent: an INVITE was sent. Once per attempt: the retry with
	// credentials after a 401/407 is a second event, told apart by CSeq.
	EventInviteSent SignalingEventType = "sip.invite.sent"
	// EventProvisionalReceived: the first response with a given 1xx code (100,
	// 180, 183, ...) arrived for the call, with that code in StatusCode.
	EventProvisionalReceived SignalingEventType = "sip.provisional.received"
	// EventAuthChallenged: the INVITE got a 401 or 407. The call retries with
	// credentials if it has them; if it can't, EventFailed follows.
	EventAuthChallenged SignalingEventType = "sip.auth.challenged"
	// EventAnsweredReceived: the 200 OK for the INVITE arrived.
	EventAnsweredReceived SignalingEventType = "sip.answered.received"
	// EventAckSent: the ACK for the 200 OK was sent.
	EventAckSent SignalingEventType = "sip.ack.sent"
	// EventFailed: the INVITE got a final non-2xx response, with its
	// StatusCode. Reason is the cause from its Reason header, if any; the
	// reason phrase is left out, as the remote side controls it.
	EventFailed SignalingEventType = "sip.failed"
	// EventCancelSent: the INVITE was cancelled before a final response.
	// Reason says why: "timeout" (no answer within the ringing timeout),
	// "shutdown" (the service is stopping) or "hangup" (the call was ended).
	// The call ends without waiting for the final response to the cancelled
	// INVITE (usually 487), so EventCallEnded has StatusCode 0. A 2xx that races
	// the CANCEL gets EventLateAnswerReceived.
	EventCancelSent SignalingEventType = "sip.cancel.sent"
	// EventLateAnswerReceived: a 2xx arrived for an INVITE we had cancelled. It
	// is acknowledged and hung up at once (no EventAckSent or EventByeSent), and
	// the call never joins the room. It can come before or after EventCallEnded.
	EventLateAnswerReceived SignalingEventType = "sip.late_answer.received"
	// EventRemoteAudioReceived: the first audio frame from the callee that is
	// above the silence threshold (see voiceThresholdDBFS) reached the room.
	// The callee's audio is connected to the room only after the 200 OK, so
	// early media (ringback, announcements) does not count. Once per call.
	// Time is when the frame was written to the room, after decoding and the
	// media processor (e.g. noise cancellation), if any. Background noise or
	// in-band DTMF above the threshold also counts.
	EventRemoteAudioReceived SignalingEventType = "sip.remote_audio.received"
)

// Both directions.
const (
	// EventByeReceived: the remote side hung up with a BYE. Reason is the
	// cause from its Reason header (e.g. "q.850;cause=16"), if any.
	EventByeReceived SignalingEventType = "sip.bye.received"
	// EventByeSent: we hung up with a BYE. Time is when it was sent, which is
	// after EventCallEnded's time (the call ends when we decide to end it); the
	// event is emitted when the BYE transaction finishes. Result is the outcome
	// ("ok", "error" or "timeout"), StatusCode the response (0 if none) and
	// Duration the time to it. An outbound BYE sent while the service shuts
	// down is not waited for, so its Result is empty.
	EventByeSent SignalingEventType = "sip.bye.sent"
	// EventCallEnded: the call is over, for any reason, including rejections
	// before a call was set up.
	EventCallEnded SignalingEventType = "sip.call.ended"
	// EventRoomAudioSent: the first audio frame of a room track that is above
	// the silence threshold (see voiceThresholdDBFS) went to the SIP side. Once
	// per participant and track name: an agent's voice and its background audio
	// are separate events, and TrackName tells them apart. Tracks with the same
	// name share the event: a track that is published again, or a second track
	// with that name, does not report again. Only frames sent while the room
	// output to the SIP side is connected count, so it is never before the
	// 200 OK. Tones played by sip (PIN prompts, dial tones) do not count.
	// Time is when the frame entered the room mixer; the mixer adds about one
	// frame (20 ms) before it is sent.
	EventRoomAudioSent SignalingEventType = "sip.room_audio.sent"
)

// Transfers, both directions. Each event has the TransferID of the transfer.
const (
	// EventTransferRequested: a transfer of the call started. A repeated
	// request for a transfer that is in progress does not start a new one, so
	// it gets no event.
	EventTransferRequested SignalingEventType = "sip.transfer.requested"
	// EventReferSent: we tried to send the REFER for the transfer. Time is the
	// time of the try; the event is emitted when its transaction ends. Result
	// is the outcome (see referOutcome): "error" can mean that the REFER was
	// never sent. StatusCode is the final response (0 if none) and Duration the
	// time to it.
	EventReferSent SignalingEventType = "sip.refer.sent"
	// EventReferNotifyReceived: a NOTIFY for the REFER arrived. CSeq is the
	// CSeq of the NOTIFY, not of the REFER. StatusCode is the status in its
	// sipfrag body, 0 if it has no body. Result is the Subscription-State
	// ("active", "pending", "terminated"), empty if the NOTIFY has none, and
	// Reason its reason, if any.
	EventReferNotifyReceived SignalingEventType = "sip.refer.notify.received"
	// EventTransferEnded: the transfer is over. Result is "success" or
	// "failed", Reason says why (see transferReasonName), StatusCode is the
	// SIP status of a rejection (0 otherwise) and Duration the time from
	// EventTransferRequested.
	EventTransferEnded SignalingEventType = "sip.transfer.ended"
)

// isTransferEvent reports whether events of type typ belong to a transfer.
func isTransferEvent(typ SignalingEventType) bool {
	switch typ {
	case EventTransferRequested, EventReferSent, EventReferNotifyReceived, EventTransferEnded:
		return true
	}
	return false
}

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
	// DispatchRuleID is copied from the call state, except on
	// EventCallDispatched, which reports its own lookup's rule (empty if none
	// matched).
	DispatchRuleID string
	// ParticipantID is the SID of the participant the event is about, so the
	// call joins the room's participant events: the SIP participant on
	// EventRoomJoined, the track's participant on EventRoomAudioSent.
	ParticipantID string
	// ParticipantKind is the kind of that participant, for example "AGENT" or
	// "STANDARD". Set on EventRoomAudioSent.
	ParticipantKind string
	// TrackID and TrackName identify the track the event is about. Set on
	// EventRoomAudioSent.
	TrackID   string
	TrackName string
	// TransferID identifies the transfer on the transfer events. The REFER and
	// NOTIFY code does not know it, so CallState adds the ID of the transfer
	// that started most recently. The ID stays after that transfer ends, so a
	// NOTIFY that arrives late still gets it.
	TransferID string

	// SIP message details, set when the event corresponds to a SIP message.
	SIPMethod string
	// StatusCode is the SIP status of the response. On EventCallEnded it is the
	// final response to the INVITE, sent (inbound) or received (outbound), or 0 if
	// there was none (e.g. a cancelled or dropped INVITE): 200 for every answered
	// call, however it ended later. Result and Reason say how the call ended.
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
	// the BYE and REFER transactions, the transfer).
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

// referOutcome classifies the result of sendRefer for EventReferSent:
// "accepted" (200 or 202), "rejected" (another final response), "timeout" (no
// final response) or "error" (the REFER could not be sent, or the service is
// stopping).
func referOutcome(res *sip.Response, err error) string {
	switch {
	case err == nil && res != nil:
		return "accepted"
	case res != nil:
		return "rejected"
	case errors.As(err, &transactionTimeoutError{}) || errors.Is(err, ErrSIPRequestTimeout):
		return "timeout"
	default:
		return "error"
	}
}

// referSentEvent builds EventReferSent for a REFER sent at t with sendRefer's
// result.
func referSentEvent(req *sip.Request, t time.Time, res *sip.Response, err error) SignalingEvent {
	ev := SignalingEvent{
		Type:      EventReferSent,
		Time:      t,
		SIPMethod: string(sip.REFER),
		CSeq:      requestCSeq(req),
		Result:    referOutcome(res, err),
		Duration:  time.Since(t),
	}
	if res != nil {
		ev.StatusCode = int(res.StatusCode)
	}
	return ev
}

// referNotifyEvent builds EventReferNotifyReceived for the NOTIFY req, which
// arrived at t and parsed to info.
func referNotifyEvent(req *sip.Request, t time.Time, info notifyInfo) SignalingEvent {
	return SignalingEvent{
		Type:       EventReferNotifyReceived,
		Time:       t,
		SIPMethod:  string(sip.NOTIFY),
		StatusCode: info.Status,
		// Not info.CSeq: that is the CSeq of the REFER, from the id parameter
		// of the Event header ("Event: refer;id=2").
		CSeq:   requestCSeq(req),
		Result: info.Sub.State,
		Reason: info.Sub.Reason,
	}
}

// transferReasonName names a transfer reason for EventTransferEnded.
func transferReasonName(r livekit.SIPTransferReason) string {
	switch r {
	case livekit.SIPTransferReason_STR_COMPLETED:
		return "completed"
	case livekit.SIPTransferReason_STR_CALL_ENDED:
		return "call-ended"
	case livekit.SIPTransferReason_STR_REJECTED:
		return "rejected"
	case livekit.SIPTransferReason_STR_RINGING_TIMEOUT:
		return "ringing-timeout"
	case livekit.SIPTransferReason_STR_SUBSCRIPTION_TERMINATED:
		return "subscription-terminated"
	case livekit.SIPTransferReason_STR_CANCELED:
		return "canceled"
	default:
		return "unspecified"
	}
}

// transferEndedEvent builds EventTransferEnded for a transfer that started at
// start and ended at t with err (nil on success).
func transferEndedEvent(t, start time.Time, err error) SignalingEvent {
	reason, sipStatus := transferReason(err)
	ev := SignalingEvent{
		Type:     EventTransferEnded,
		Time:     t,
		Result:   "success",
		Reason:   transferReasonName(reason),
		Duration: t.Sub(start),
	}
	if err != nil {
		ev.Result = "failed"
	}
	if sipStatus != nil {
		ev.StatusCode = int(sipStatus.Code)
	}
	return ev
}

// reasonCause renders a Reason header as "<protocol>;cause=<code>" for
// events. Its free text is left out: the remote side controls it.
func reasonCause(r ReasonHeader) string {
	if r.IsZero() {
		return ""
	}
	return fmt.Sprintf("%s;cause=%d", r.Type, r.Cause)
}

// messageCause renders the Reason header of a request or response as
// reasonCause does, or "" if it has none or it doesn't parse.
func messageCause(m interface{ GetHeader(string) sip.Header }) string {
	h := m.GetHeader("Reason")
	if h == nil {
		return ""
	}
	r, err := ParseReasonHeader(h.Value())
	if err != nil {
		return ""
	}
	return reasonCause(r)
}

// requestCSeq returns the request's CSeq number, or 0 if it has none.
func requestCSeq(req *sip.Request) uint32 {
	if h := req.CSeq(); h != nil {
		return h.SeqNo
	}
	return 0
}

// inviteResponseEvent builds an event for a response sent to the INVITE.
func inviteResponseEvent(typ SignalingEventType, t time.Time, statusCode int, cseq uint32) SignalingEvent {
	return SignalingEvent{Type: typ, Time: t, SIPMethod: "INVITE", StatusCode: statusCode, CSeq: cseq}
}

// roomAudioSent emits EventRoomAudioSent. It is the room's OnRoomAudio.
func (s *CallState) roomAudioSent(t time.Time, tr RoomAudioTrack) {
	s.EmitSignalingEvent(SignalingEvent{
		Type:            EventRoomAudioSent,
		Time:            t,
		ParticipantID:   tr.ParticipantID,
		ParticipantKind: tr.ParticipantKind,
		TrackID:         tr.TrackID,
		TrackName:       tr.TrackName,
	})
}

// remoteAudioReceived emits EventRemoteAudioReceived.
func (s *CallState) remoteAudioReceived(t time.Time) {
	s.EmitSignalingEvent(SignalingEvent{Type: EventRemoteAudioReceived, Time: t})
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
