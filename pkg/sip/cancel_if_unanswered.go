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
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

// CancelIfUnanswered stops ringing an outbound call without hanging up a callee
// whose 2xx crosses the CANCEL (RFC 3261 §9.1, §9.2, §13.2.2.4).
//
// Removing the participant is unconditional: it cancels the INVITE and drops a
// callee who has just answered. The RPC below only asks the goroutine that owns
// the INVITE client transaction to cancel it. That goroutine alone sees the SIP
// responses, so it alone decides:
//
//   - no INVITE sent yet: nothing is sent, result "cancelled";
//   - no 1xx yet: the CANCEL is deferred until the first 1xx (§9.1);
//   - a 2xx arrives (before or after the CANCEL): it is ACKed and the call is
//     kept like a normal answer, result "answered";
//   - a non-2xx final (487 or other): result "cancelled", the participant leaves;
//   - no final response 64*T1 after the CANCEL, Timer B, or a transport error:
//     result "unconfirmed", the participant leaves.
//
// After a cancel request, the ringing timeout and room disconnects no longer
// abort the wait for the final response, so a crossing 2xx is still ACKed. A
// room disconnect then hangs up the answered call as before (BYE after ACK).
// Service shutdown keeps its existing behaviour.
//
// The result is returned by the RPC (or "pending" if it is not known within
// the reply wait) and published as the participant attribute
// AttrSIPCancelResult before the participant leaves. The RPC reply deadline
// never changes SIP state.
const (
	// RPCCancelIfUnanswered is the participant RPC method registered on outbound SIP participants.
	RPCCancelIfUnanswered = "lk.sip.CancelIfUnanswered"
	// AttrSIPCancelIfUnanswered is set to "true" on outbound participants that support RPCCancelIfUnanswered.
	AttrSIPCancelIfUnanswered = "sip.cancelIfUnanswered"
	// AttrSIPCancelResult carries the CancelResult once it is known, after a cancel request.
	AttrSIPCancelResult = "sip.cancelResult"
)

// CancelResult is the outcome of a CancelIfUnanswered request. Only SIP evidence decides it.
type CancelResult string

const (
	// CancelResultPending is only an RPC reply: the result was not known within the reply wait.
	CancelResultPending CancelResult = "pending"
	// CancelResultAnswered means a 2xx for the INVITE was received. The call is kept.
	CancelResultAnswered CancelResult = "answered"
	// CancelResultCancelled means proven not answered: a non-2xx final response,
	// or no INVITE was sent.
	CancelResultCancelled CancelResult = "cancelled"
	// CancelResultUnconfirmed means the INVITE ended without a final response
	// (timeout after the CANCEL, Timer B, transport error).
	CancelResultUnconfirmed CancelResult = "unconfirmed"
)

// cancelTimers are the timing values of one outbound call, copied at call creation.
type cancelTimers struct {
	replyWait     time.Duration // max time the RPC waits for a result before it replies "pending"
	cancelTimeout time.Duration // wait for the INVITE final response after the CANCEL (64*T1, RFC 3261 §9.1)
	retransmit    time.Duration // first CANCEL retransmission interval on unreliable transports (Timer E = T1)
	retransmitMax time.Duration // retransmission interval cap (T2)
	acceptedWatch time.Duration // how long 2xx retransmissions of an answered INVITE are ACKed (Timer M = 64*T1)
}

var defaultCancelTimers = cancelTimers{
	replyWait:     2 * time.Second,
	cancelTimeout: 64 * 500 * time.Millisecond,
	retransmit:    500 * time.Millisecond,
	retransmitMax: 4 * time.Second,
	acceptedWatch: 64 * 500 * time.Millisecond,
}

// inviteOutcome is shared by the RPC handler and the goroutine that owns the INVITE.
// The RPC only records the request; the owner settles the result. The first
// settled result wins and is never overwritten.
type inviteOutcome struct {
	mu        sync.Mutex
	requested bool
	aborted   bool // the owner took an exit without a request (ringing timeout, room disconnect)
	sent      bool // the send gate passed for an INVITE
	reqCh     chan struct{}
	result    CancelResult
	resultCh  chan struct{}
	published bool
	publish   func(CancelResult) // publishes AttrSIPCancelResult; called once, after a request
}

func newInviteOutcome() *inviteOutcome {
	return &inviteOutcome{
		reqCh:    make(chan struct{}),
		resultCh: make(chan struct{}),
	}
}

func (o *inviteOutcome) setPublisher(fnc func(CancelResult)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.publish = fnc
}

// request records a cancel request. It is idempotent. If the result is
// already known, the attribute is published in the background (the RPC reply
// carries the result anyway).
func (o *inviteOutcome) request() {
	o.mu.Lock()
	if !o.requested {
		o.requested = true
		close(o.reqCh)
	}
	fnc := o.takePublishLocked()
	o.mu.Unlock()
	if fnc != nil {
		go fnc()
	}
}

// abort arbitrates an exit of the INVITE owner without a request (ringing
// timeout, room disconnect) against a request. It returns false if a request
// was recorded first: the request wins and the exit is ignored. Otherwise the
// exit wins, and a later request only waits for the result.
func (o *inviteOutcome) abort() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.requested {
		return false
	}
	o.aborted = true
	return true
}

// requestedCh is closed once a cancel request was recorded.
func (o *inviteOutcome) requestedCh() <-chan struct{} {
	return o.reqCh
}

func (o *inviteOutcome) isRequested() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requested
}

// openSend is the send gate, the linearization point between a request and an
// INVITE. The owner calls it as the last step before it hands an INVITE to the
// transport. It returns false if a request came first: then no INVITE is sent
// for this call any more. A request recorded after it passed is handled by the
// owner as a request during Calling (CANCEL deferred to the first 1xx); the
// result then comes from SIP evidence. The lock is not held across the
// transport hand-off, which can block on a TCP/TLS connect, because the RPC
// handler takes the same lock.
func (o *inviteOutcome) openSend() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.requested {
		return false
	}
	o.sent = true
	return true
}

// settle records the result. The first call wins; later calls are ignored.
func (o *inviteOutcome) settle(res CancelResult) {
	o.mu.Lock()
	if o.result != "" {
		o.mu.Unlock()
		return
	}
	o.result = res
	close(o.resultCh)
	fnc := o.takePublishLocked()
	o.mu.Unlock()
	if fnc != nil {
		fnc()
	}
}

// settleAtClose is the backstop when the call ends: Invite settles every call
// that reached it, so an unsettled call never sent an INVITE (it failed or was
// stopped before) and was not answered. If an INVITE was sent, the result
// stays with SIP evidence (unconfirmed when none arrived).
func (o *inviteOutcome) settleAtClose() {
	o.mu.Lock()
	sent := o.sent
	o.mu.Unlock()
	if sent {
		o.settle(CancelResultUnconfirmed)
	} else {
		o.settle(CancelResultCancelled)
	}
}

// takePublishLocked returns the publish call once both a request and a result exist.
func (o *inviteOutcome) takePublishLocked() func() {
	if !o.requested || o.result == "" || o.published || o.publish == nil {
		return nil
	}
	o.published = true
	fnc, res := o.publish, o.result
	return func() { fnc(res) }
}

// wait returns the result, or CancelResultPending if it is not known within d.
func (o *inviteOutcome) wait(d time.Duration) CancelResult {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-o.resultCh:
	case <-t.C:
		return CancelResultPending
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.result
}

// cancelIfUnansweredError ends an INVITE that a cancel request stopped before any 2xx.
type cancelIfUnansweredError struct {
	result CancelResult
	cause  error
}

var _ inviteClassifier = cancelIfUnansweredError{}

func (e cancelIfUnansweredError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("INVITE cancelled by request (%s): %v", e.result, e.cause)
	}
	return fmt.Sprintf("INVITE cancelled by request (%s)", e.result)
}

func (e cancelIfUnansweredError) Unwrap() error { return e.cause }

func (e cancelIfUnansweredError) ClassifyInvite() inviteFailure {
	term := stats.Success("cancel-if-unanswered")
	if e.result != CancelResultCancelled {
		term = stats.Indeterminate("cancel-if-unanswered-unconfirmed")
	}
	return inviteFailure{
		EndCall: EndCall{
			Status: CallCancelled,
			Term:   term,
			Reason: livekit.DisconnectReason_CLIENT_INITIATED,
		},
		returnErr: psrpc.NewError(psrpc.Canceled, e),
	}
}

type cancelIfUnansweredResp struct {
	Result CancelResult `json:"result"`
}

// cancelIfUnanswered handles one RPC call: only callers for which isAgent
// returns true may use it. It only posts the request to the INVITE owner and
// waits a bounded time for the result.
func cancelIfUnanswered(log logger.Logger, o *inviteOutcome, isAgent func(identity string) bool, replyWait time.Duration, caller string, responseTimeout time.Duration) ([]byte, error) {
	if isAgent == nil || !isAgent(caller) {
		log.Infow("rejecting CancelIfUnanswered from a non-agent participant", "caller", caller)
		return nil, lksdk.NewRpcError(lksdk.RpcApplicationError, "only agent participants may cancel this call", nil)
	}
	log.Infow("CancelIfUnanswered requested", "caller", caller)
	o.request()
	wait := replyWait
	if half := responseTimeout / 2; responseTimeout > 0 && half < wait {
		wait = half
	}
	return json.Marshal(cancelIfUnansweredResp{Result: o.wait(wait)})
}

// newCancelIfUnansweredHandler returns the participant RPC handler.
func newCancelIfUnansweredHandler(log logger.Logger, o *inviteOutcome, isAgent func(identity string) bool, replyWait time.Duration) lksdk.RpcHandlerCtxFunc {
	return func(ctx context.Context, _ []byte) ([]byte, error) {
		var (
			caller  string
			timeout time.Duration
		)
		if meta := lksdk.RPCMetadataFromContext(ctx); meta != nil {
			caller, timeout = meta.CallerIdentity, meta.ResponseTimeout
		}
		return cancelIfUnanswered(log, o, isAgent, replyWait, caller, timeout)
	}
}

// roomAgentCheck reports whether identity is an agent participant of the room.
func roomAgentCheck(room *lksdk.Room) func(identity string) bool {
	return func(identity string) bool {
		if room == nil || identity == "" {
			return false
		}
		rp := room.GetParticipantByIdentity(identity)
		return rp != nil && rp.Kind() == lksdk.ParticipantAgent
	}
}

func registerCancelIfUnansweredRPC(log logger.Logger, r RoomInterface, room *lksdk.Room, o *inviteOutcome, replyWait time.Duration) error {
	return r.RegisterRpcCtxMethod(RPCCancelIfUnanswered, newCancelIfUnansweredHandler(log, o, roomAgentCheck(room), replyWait))
}

func toTagOf(res *sip.Response) string {
	if to := res.To(); to != nil && to.Params != nil {
		if tag, ok := to.Params.Get("tag"); ok {
			return tag
		}
	}
	return ""
}
