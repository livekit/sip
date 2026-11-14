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
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/traceid"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
)

// Tests for the lk.sip.CancelIfUnanswered participant RPC: stop ringing an
// outbound call, but keep it if the callee's 2xx crosses the CANCEL
// (RFC 3261 §9.1, §13.2.2.4).

const (
	cancelTestMethod = "lk.sip.CancelIfUnanswered"
	cancelTestAgent  = "agent-1"
	cancelTestSDP    = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
)

// setCancelTestTimers overrides the cancel timing values for calls created by this test.
func setCancelTestTimers(t *testing.T, fnc func(tm *cancelTimers)) {
	prev := defaultCancelTimers
	tm := prev
	fnc(&tm)
	defaultCancelTimers = tm
	t.Cleanup(func() { defaultCancelTimers = prev })
}

type cancelTestCall struct {
	t         *testing.T
	h         *testSIPHarness
	callID    LocalTag
	sipClient *testSIPClient
	room      *testRoom
	lkRoom    *lksdk.Room
	invite    *transactionRequest
	cancels   atomic.Int32
	cancelCh  chan struct{}
	done      chan error
}

// startCancelTestCall dials an outbound call against the test SIP client and
// returns once the INVITE transaction was created. An agent participant is in the room.
func startCancelTestCall(t *testing.T, mutate func(req *rpc.InternalCreateSIPParticipantRequest)) *cancelTestCall {
	t.Helper()
	rooms := make(chan *testRoom, 1)
	getRoom := func(log logger.Logger, st *RoomStats) RoomInterface {
		r := newTestRoomWithConfig(log, st, nil).(*testRoom)
		rooms <- r
		return r
	}
	h := NewTestSIP(t, TestSIPConfig{GetRoom: getRoom})
	c := &cancelTestCall{t: t, h: h, sipClient: h.client, cancelCh: make(chan struct{}, 100), done: make(chan error, 1)}
	req := MinimalCreateSIPParticipantRequest()
	req.WaitUntilAnswered = true
	if mutate != nil {
		mutate(req)
	}
	c.callID = LocalTag(req.SipCallId)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_, err := h.manager.CreateSIPParticipant(ctx, req)
		c.done <- err
	}()
	select {
	case c.room = <-rooms:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "expected room")
	}
	c.invite = c.nextInvite(2 * time.Second)
	require.NotNil(t, c.invite, "expected INVITE transaction")
	c.lkRoom = c.room.room.room.Load()
	// The agent that may call the RPC.
	c.lkRoom.OnParticipantUpdate([]*livekit.ParticipantInfo{{
		Sid:      "PA_agent1",
		Identity: cancelTestAgent,
		Kind:     livekit.ParticipantInfo_AGENT,
		State:    livekit.ParticipantInfo_ACTIVE,
	}})
	return c
}

// nextInvite returns the next INVITE transaction, or nil after d.
func (c *cancelTestCall) nextInvite(d time.Duration) *transactionRequest {
	tr, err := c.sipClient.WaitTransactionTimeout(d, string(c.callID), "")
	if err != nil {
		return nil
	}
	require.Equal(c.t, sip.INVITE, tr.req.Method)
	cancels := tr.transaction.cancels
	go func() {
		for range cancels {
			c.cancels.Add(1)
			c.cancelCh <- struct{}{}
		}
	}()
	return tr
}

func (c *cancelTestCall) response(code sip.StatusCode, reason, toTag string) *sip.Response {
	var res *sip.Response
	if code == sip.StatusOK {
		res = sip.NewSDPResponseFromRequest(c.invite.req, []byte(cancelTestSDP))
	} else {
		res = sip.NewResponseFromRequest(c.invite.req, code, reason, nil)
	}
	if toTag != "" {
		res.To().Params.Add("tag", toTag)
	}
	if code == sip.StatusProxyAuthRequired {
		res.AppendHeader(sip.NewHeader("Proxy-Authenticate", `Digest realm="example.com", nonce="abc123", algorithm=MD5`))
	}
	return res
}

// send delivers a response on the INVITE transaction (the test channel is unbuffered).
func (c *cancelTestCall) send(code sip.StatusCode, reason, toTag string) {
	c.t.Helper()
	res := c.response(code, reason, toTag)
	require.Eventually(c.t, func() bool { return c.invite.transaction.SendResponse(res) == nil }, 2*time.Second, time.Millisecond,
		"response %d not read by the INVITE owner", code)
}

type cancelRPCReply struct {
	payload string
	err     error
}

// rpc calls the CancelIfUnanswered handler like the LiveKit SDK does, asynchronously.
func (c *cancelTestCall) rpc(caller string) <-chan cancelRPCReply {
	return c.rpcWithTimeout(caller, 10*time.Second)
}

// rpcWithTimeout is rpc with the caller's response timeout; the handler waits at most half of it.
// It runs the handler core with the caller identity that the SDK takes from the RPC packet.
func (c *cancelTestCall) rpcWithTimeout(caller string, timeout time.Duration) <-chan cancelRPCReply {
	require.NotNil(c.t, c.room.rpcHandler(cancelTestMethod), "CancelIfUnanswered RPC is not registered")
	call := c.h.manager.getActiveCall(c.callID)
	require.NotNil(c.t, call, "outbound call not found")
	o, replyWait := call.cc.outcome, call.cc.timers.replyWait
	lk := c.lkRoom
	out := make(chan cancelRPCReply, 1)
	go func() {
		payload, err := cancelIfUnanswered(logger.NewTestLogger(c.t), o, roomAgentCheck(lk), replyWait, caller, timeout)
		out <- cancelRPCReply{payload: string(payload), err: err}
	}()
	return out
}

func (c *cancelTestCall) rpcResult(ch <-chan cancelRPCReply) string {
	c.t.Helper()
	select {
	case r := <-ch:
		require.NoError(c.t, r.err)
		var resp struct {
			Result string `json:"result"`
		}
		require.NoError(c.t, json.Unmarshal([]byte(r.payload), &resp))
		return resp.Result
	case <-time.After(5 * time.Second):
		require.FailNow(c.t, "no RPC reply")
		return ""
	}
}

func (c *cancelTestCall) waitCancel(d time.Duration) bool {
	select {
	case <-c.cancelCh:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *cancelTestCall) waitDone(d time.Duration) error {
	c.t.Helper()
	select {
	case err := <-c.done:
		return err
	case <-time.After(d):
		require.FailNow(c.t, "CreateSIPParticipant did not return")
		return nil
	}
}

// requests returns the requests written outside of transactions (ACK, BYE) within d.
func (c *cancelTestCall) requests(d time.Duration) []*sip.Request {
	var out []*sip.Request
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return out
		}
		r, err := c.sipClient.WaitRequestTimeout(left, string(c.callID), "")
		if err != nil {
			return out
		}
		out = append(out, r.req)
	}
}

// nextRequest returns the next request written outside of transactions, or nil after d.
func (c *cancelTestCall) nextRequest(d time.Duration) *sipRequest {
	r, err := c.sipClient.WaitRequestTimeout(d, string(c.callID), "")
	if err != nil {
		return nil
	}
	return r
}

// nextTransaction returns the next non-INVITE transaction request (e.g. BYE), or nil after d.
func (c *cancelTestCall) nextTransaction(d time.Duration) *transactionRequest {
	tr, err := c.sipClient.WaitTransactionTimeout(d, string(c.callID), "")
	if err != nil {
		return nil
	}
	return tr
}

func methodsOf(reqs []*sip.Request) []sip.RequestMethod {
	var out []sip.RequestMethod
	for _, r := range reqs {
		out = append(out, r.Method)
	}
	return out
}

func toTagOfRequest(r *sip.Request) string {
	if to := r.To(); to != nil && to.Params != nil {
		tag, _ := to.Params.Get("tag")
		return tag
	}
	return ""
}

func TestCancelIfUnansweredAdvertisedAndRegistered(t *testing.T) {
	c := startCancelTestCall(t, nil)
	require.Equal(t, "true", c.room.connectAttrs()["sip.cancelIfUnanswered"])
	require.NotNil(t, c.room.rpcHandler(cancelTestMethod))
	c.send(sip.StatusOK, "OK", "uas-1")
	require.NoError(t, c.waitDone(2*time.Second))
}

func TestCancelIfUnansweredDefersCancelUntilProvisional(t *testing.T) {
	c := startCancelTestCall(t, nil)

	reply := c.rpcWithTimeout(cancelTestAgent, 400*time.Millisecond)
	require.Equal(t, "pending", c.rpcResult(reply), "no SIP evidence yet")
	require.False(t, c.waitCancel(200*time.Millisecond), "CANCEL must not be sent before a provisional response (RFC 3261 §9.1)")

	c.send(sip.StatusRinging, "Ringing", "uas-1")
	require.True(t, c.waitCancel(2*time.Second), "deferred CANCEL must be sent on the first 1xx")

	reply = c.rpc(cancelTestAgent) // repeated request, waits for the result
	time.Sleep(50 * time.Millisecond)
	c.send(sip.StatusRequestTerminated, "Request Terminated", "uas-1")
	require.Equal(t, "cancelled", c.rpcResult(reply))
	require.Error(t, c.waitDone(2*time.Second))
	for _, r := range c.requests(200 * time.Millisecond) {
		require.NotEqual(t, sip.BYE, r.Method)
	}
	require.Nil(t, c.nextTransaction(100*time.Millisecond), "nothing is hung up")
}

func TestCancelIfUnansweredKeepsCrossingAnswer(t *testing.T) {
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")

	reply := c.rpc(cancelTestAgent)
	require.True(t, c.waitCancel(2*time.Second), "expected CANCEL while ringing")

	// The callee answered before the CANCEL reached it.
	c.send(sip.StatusOK, "OK", "uas-1")
	require.NoError(t, c.waitDone(2*time.Second), "a crossing 2xx must be kept like a normal answer")
	require.Equal(t, "answered", c.rpcResult(reply))
	reqs := c.requests(300 * time.Millisecond)
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(reqs), "the crossing 2xx is ACKed and not hung up")
}

func TestCancelIfUnansweredAfterAnswerIsNoop(t *testing.T) {
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	c.send(sip.StatusOK, "OK", "uas-1")
	require.NoError(t, c.waitDone(2*time.Second))

	require.Equal(t, "answered", c.rpcResult(c.rpc(cancelTestAgent)))
	require.False(t, c.waitCancel(200*time.Millisecond))
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(c.requests(200*time.Millisecond)))
}

func TestCancelIfUnansweredFinalBeforeProvisional(t *testing.T) {
	c := startCancelTestCall(t, nil)
	reply := c.rpc(cancelTestAgent)
	time.Sleep(50 * time.Millisecond)
	c.send(sip.StatusBusyHere, "Busy Here", "uas-1")
	require.Error(t, c.waitDone(2*time.Second))
	require.Equal(t, "cancelled", c.rpcResult(reply))
	require.Zero(t, c.cancels.Load(), "a final response before any 1xx settles the request without a CANCEL")
}

func TestCancelIfUnansweredDoesNotRetryAuth(t *testing.T) {
	c := startCancelTestCall(t, func(req *rpc.InternalCreateSIPParticipantRequest) {
		req.Username = "user"
		req.Password = "secret"
	})
	reply := c.rpc(cancelTestAgent)
	time.Sleep(50 * time.Millisecond)
	c.send(sip.StatusProxyAuthRequired, "Proxy Authentication Required", "uas-1")
	require.Error(t, c.waitDone(2*time.Second))
	require.Equal(t, "cancelled", c.rpcResult(reply))
	require.Nil(t, c.nextInvite(300*time.Millisecond), "no new INVITE may be sent after a cancel request")
}

func TestCancelIfUnansweredRingingTimeoutKeepsCrossingAnswer(t *testing.T) {
	c := startCancelTestCall(t, func(req *rpc.InternalCreateSIPParticipantRequest) {
		req.RingingTimeout = durationpb.New(300 * time.Millisecond)
	})
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	reply := c.rpc(cancelTestAgent)
	require.True(t, c.waitCancel(2*time.Second))

	time.Sleep(500 * time.Millisecond) // the ringing timeout passes while the CANCEL is pending
	c.send(sip.StatusOK, "OK", "uas-1")
	require.NoError(t, c.waitDone(2*time.Second))
	require.Equal(t, "answered", c.rpcResult(reply))
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(c.requests(300*time.Millisecond)))
}

func TestCancelIfUnansweredRoomDisconnectAcksThenHangsUp(t *testing.T) {
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	reply := c.rpc(cancelTestAgent)
	require.True(t, c.waitCancel(2*time.Second))

	// The room connection ends while the CANCEL is pending (terminal exit).
	c.room.simulateRoomClosed(livekit.DisconnectReason_PARTICIPANT_REMOVED)
	time.Sleep(50 * time.Millisecond)
	c.send(sip.StatusOK, "OK", "uas-1")
	require.Equal(t, "answered", c.rpcResult(reply))

	ack := c.nextRequest(2 * time.Second)
	require.NotNil(t, ack, "the crossing 2xx must be ACKed")
	require.Equal(t, sip.ACK, ack.req.Method)
	bye := c.nextTransaction(2 * time.Second)
	require.NotNil(t, bye, "the leg ends because its room connection is gone")
	require.Equal(t, sip.BYE, bye.req.Method)
	require.Less(t, ack.sequence, bye.sequence, "ACK before BYE")
	res := sip.NewResponseFromRequest(bye.req, sip.StatusOK, "OK", nil)
	require.Eventually(t, func() bool { return bye.transaction.SendResponse(res) == nil }, 2*time.Second, time.Millisecond)
}

func TestCancelIfUnansweredTimeoutIsUnconfirmed(t *testing.T) {
	setCancelTestTimers(t, func(tm *cancelTimers) {
		tm.cancelTimeout = 300 * time.Millisecond
		tm.retransmit = 0
	})
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	reply := c.rpc(cancelTestAgent)
	require.True(t, c.waitCancel(2*time.Second))
	require.Error(t, c.waitDone(2*time.Second))
	require.Equal(t, "unconfirmed", c.rpcResult(reply))
}

func TestCancelIfUnansweredRetransmitsCancelOverUDP(t *testing.T) {
	setCancelTestTimers(t, func(tm *cancelTimers) {
		tm.retransmit = 40 * time.Millisecond
		tm.retransmitMax = 80 * time.Millisecond
	})
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	reply := c.rpc(cancelTestAgent)
	time.Sleep(400 * time.Millisecond)
	require.GreaterOrEqual(t, c.cancels.Load(), int32(3), "CANCEL is retransmitted (Timer E) on UDP")
	c.send(sip.StatusRequestTerminated, "Request Terminated", "uas-1")
	require.Error(t, c.waitDone(2*time.Second))
	require.Equal(t, "cancelled", c.rpcResult(reply))
	n := c.cancels.Load()
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, n, c.cancels.Load(), "no CANCEL after the final response")
}

func TestCancelIfUnansweredRejectsNonAgent(t *testing.T) {
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	select {
	case r := <-c.rpc("someone-else"):
		require.Error(t, r.err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "no RPC reply")
	}
	require.False(t, c.waitCancel(200*time.Millisecond), "a non-agent caller changes nothing")
	c.send(sip.StatusOK, "OK", "uas-1")
	require.NoError(t, c.waitDone(2*time.Second))
}

func TestCancelIfUnansweredBeforeSendGate(t *testing.T) {
	h := NewTestSIP(t, TestSIPConfig{})
	client := h.manager
	tr := TransportFrom(livekit.SIPTransport_SIP_TRANSPORT_UDP)
	contact := client.ContactURI(tr)
	uri := CreateURIFromUserAndAddress("+1234567890", "sip.example.com", tr).GetURI()
	from := &sip.FromHeader{Address: sip.Uri{User: "+0987654321", Host: contact.GetHost()}, Params: sip.NewParams()}
	cc := client.newOutbound(logger.NewTestLogger(t), LocalTag("gate-test"), uri, &sip.ToHeader{Address: *uri}, from, contact, nil)
	rpcH := func(d lksdk.RpcInvocationData) (string, error) {
		out, err := cancelIfUnanswered(logger.NewTestLogger(t), cc.outcome, func(string) bool { return true }, 100*time.Millisecond, d.CallerIdentity, d.ResponseTimeout)
		return string(out), err
	}

	reply, err := rpcH(lksdk.RpcInvocationData{CallerIdentity: cancelTestAgent, ResponseTimeout: time.Second})
	require.NoError(t, err)
	require.JSONEq(t, `{"result":"pending"}`, reply)

	_, err = cc.Invite(context.Background(), "", "", nil, []byte(cancelTestSDP), nil)
	require.Error(t, err)
	_, werr := h.client.WaitTransactionTimeout(100*time.Millisecond, "gate-test", "")
	require.Error(t, werr, "no INVITE may be sent after a cancel request")
	reply, err = rpcH(lksdk.RpcInvocationData{CallerIdentity: cancelTestAgent, ResponseTimeout: time.Second})
	require.NoError(t, err)
	require.JSONEq(t, `{"result":"cancelled"}`, reply)
}

func TestInviteOKRetransmissionsAreAckedPerDialog(t *testing.T) {
	setCancelTestTimers(t, func(tm *cancelTimers) { tm.acceptedWatch = 5 * time.Second })
	c := startCancelTestCall(t, nil)
	c.send(sip.StatusRinging, "Ringing", "uas-1")
	c.send(sip.StatusOK, "OK", "uas-1")
	require.NoError(t, c.waitDone(2*time.Second))
	reqs := c.requests(200 * time.Millisecond)
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(reqs))

	// The first ACK was lost: the UAS retransmits the 2xx, which must be ACKed again.
	c.send(sip.StatusOK, "OK", "uas-1")
	reqs = c.requests(300 * time.Millisecond)
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(reqs), "a 2xx retransmission is ACKed again and not hung up")
	require.Equal(t, "uas-1", toTagOfRequest(reqs[0]))

	// A 2xx from a second dialog (fork) is ACKed and that dialog alone is hung up.
	c.send(sip.StatusOK, "OK", "uas-2")
	reqs = c.requests(300 * time.Millisecond)
	require.Equal(t, []sip.RequestMethod{sip.ACK, sip.BYE}, methodsOf(reqs))
	for _, r := range reqs {
		require.Equal(t, "uas-2", toTagOfRequest(r))
	}
}


// stubInviteTx models sipgo's INVITE client transaction where it matters here:
// Responses is buffered, Done and Responses close together on Terminate, and
// onCancel runs inside Cancel (sipgo runs the FSM synchronously there).
type stubInviteTx struct {
	responses chan *sip.Response
	done      chan struct{}
	cancels   atomic.Int32
	onCancel  func()
	// onTerminate runs once, inside the first Terminate.
	onTerminate func()
	once        sync.Once
	mu        sync.RWMutex
}

func newStubInviteTx(t *testing.T, buffer int) *stubInviteTx {
	tx := &stubInviteTx{responses: make(chan *sip.Response, buffer), done: make(chan struct{})}
	t.Cleanup(tx.Terminate) // ends the pump goroutine
	return tx
}

func (t *stubInviteTx) Terminate() {
	t.once.Do(func() {
		if t.onTerminate != nil {
			t.onTerminate()
		}
		t.mu.Lock() // like sipgo: delete excludes passUp
		close(t.done)
		close(t.responses)
		t.mu.Unlock()
	})
}

// pass delivers a response like sipgo's passUp: never after Terminate.
func (t *stubInviteTx) pass(r *sip.Response) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	select {
	case <-t.done:
		return false
	default:
	}
	select {
	case <-t.done:
		return false
	case t.responses <- r:
		return true
	}
}
func (t *stubInviteTx) Done() <-chan struct{}          { return t.done }
func (t *stubInviteTx) Err() error                     { return nil }
func (t *stubInviteTx) Responses() <-chan *sip.Response { return t.responses }
func (t *stubInviteTx) Cancel() error {
	t.cancels.Add(1)
	if t.onCancel != nil {
		t.onCancel()
	}
	return nil
}

// lockedSIPClient records requests written outside of transactions (goroutine-safe).
type lockedSIPClient struct {
	mu   sync.Mutex
	reqs []*sip.Request
}

func (r *lockedSIPClient) TransactionRequest(req *sip.Request, _ ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	return nil, errors.New("not supported")
}
func (r *lockedSIPClient) WriteRequest(req *sip.Request, _ ...sipgo.ClientRequestOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return nil
}
func (r *lockedSIPClient) Close() error { return nil }
func (r *lockedSIPClient) ResolveAddrPreferSRV(context.Context, string, string, int, string) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("not supported")
}
func (r *lockedSIPClient) written() []*sip.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*sip.Request(nil), r.reqs...)
}

func stubInvite() *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "callee", Host: "sip.example.com"})
	from := &sip.FromHeader{Address: sip.Uri{User: "caller", Host: "lk"}, Params: sip.NewParams()}
	from.Params.Add("tag", "caller-tag")
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "callee", Host: "sip.example.com"}, Params: sip.NewParams()})
	cid := sip.CallIDHeader("call-123")
	req.AppendHeader(&cid)
	req.AppendHeader(&sip.CSeqHeader{MethodName: sip.INVITE, SeqNo: 1})
	via := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "lk", Port: 5060, Params: sip.NewParams()}
	via.Params.Add("branch", "z9hG4bK.test")
	req.AppendHeader(via)
	return req
}

func stubResponse(req *sip.Request, code sip.StatusCode, toTag string) *sip.Response {
	var res *sip.Response
	if code == sip.StatusOK {
		res = sip.NewSDPResponseFromRequest(req, []byte(cancelTestSDP))
	} else {
		res = sip.NewResponseFromRequest(req, code, "", nil)
	}
	res.To().Params.Add("tag", toTag)
	return res
}

func newStubOutbound(t *testing.T) *sipOutbound {
	client := NewTestSIP(t, TestSIPConfig{}).manager
	tr := TransportFrom(livekit.SIPTransport_SIP_TRANSPORT_UDP)
	contact := client.ContactURI(tr)
	uri := CreateURIFromUserAndAddress("+1234567890", "sip.example.com", tr).GetURI()
	from := &sip.FromHeader{Address: sip.Uri{User: "+0987654321", Host: contact.GetHost()}, Params: sip.NewParams()}
	return client.newOutbound(logger.NewTestLogger(t), LocalTag("owner-test"), uri, &sip.ToHeader{Address: *uri}, from, contact, nil)
}

type waitResult struct {
	res       *sip.Response
	cancelled bool
	err       error
}

func runWaitInvite(c *sipOutbound, ctx context.Context, tx *stubInviteTx, pump *respPump, reliable bool) <-chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		res, cancelled, err := c.waitInviteResponse(ctx, tx, pump, reliable, 1, nil)
		out <- waitResult{res, cancelled != "", err}
	}()
	return out
}

// F1: the owner must not deadlock when the transaction layer can only finish
// Cancel after more responses were read (sipgo passUp on a full buffer).
func TestCancelIfUnansweredCancelUnderBackpressure(t *testing.T) {
	c := newStubOutbound(t)
	req := stubInvite()
	tx := newStubInviteTx(t, 2)
	blocked := make(chan bool, 1)
	tx.onCancel = func() {
		if tx.cancels.Load() != 1 {
			return
		}
		// Five 183s while inside Cancel: more than the buffer holds.
		for i := 0; i < 5; i++ {
			select {
			case tx.responses <- stubResponse(req, sip.StatusCode(183), "uas-1"):
			case <-time.After(2 * time.Second):
				blocked <- true
				return
			}
		}
		blocked <- false
	}
	pump := startRespPump(tx.Responses(), tx.Done())
	tx.responses <- stubResponse(req, sip.StatusRinging, "uas-1")
	out := runWaitInvite(c, context.Background(), tx, pump, true)
	time.Sleep(50 * time.Millisecond)
	c.outcome.request()
	require.False(t, <-blocked, "Cancel blocked: nobody drained the responses")
	tx.responses <- stubResponse(req, sip.StatusRequestTerminated, "uas-1")
	select {
	case r := <-out:
		require.NoError(t, r.err)
		require.Equal(t, sip.StatusRequestTerminated, r.res.StatusCode)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "owner did not return")
	}
}

// F2: a request recorded before the owner runs wins over an abort (ctx) that is
// ready at the same time: no early CANCEL, and a crossing 2xx is returned.
func TestCancelIfUnansweredRequestWinsOverAbort(t *testing.T) {
	c := newStubOutbound(t)
	req := stubInvite()
	// Both are ready when the owner first selects; repeat so a random select order cannot hide a loss.
	for i := 0; i < 30; i++ {
		c.outcome = newInviteOutcome()
		tx := newStubInviteTx(t, 10)
		pump := startRespPump(tx.Responses(), tx.Done())
		c.outcome.request()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // ringing timeout / room disconnect already happened
		out := runWaitInvite(c, ctx, tx, pump, true)
		time.Sleep(20 * time.Millisecond)
		require.Zero(t, tx.cancels.Load(), "no CANCEL before a 1xx")
		select {
		case r := <-out:
			require.FailNow(t, "the abort must not end the wait after a request", "iteration %d: %+v", i, r)
		default:
		}
		tx.responses <- stubResponse(req, sip.StatusRinging, "uas-1")
		require.Eventually(t, func() bool { return tx.cancels.Load() == 1 }, 2*time.Second, time.Millisecond)
		tx.responses <- stubResponse(req, sip.StatusOK, "uas-1")
		select {
		case r := <-out:
			require.NoError(t, r.err)
			require.Equal(t, sip.StatusOK, r.res.StatusCode)
		case <-time.After(2 * time.Second):
			require.FailNow(t, "owner did not return")
		}
		tx.Terminate()
	}
}

// F2 (other order): an abort that won leaves a later request waiting for the result.
func TestCancelIfUnansweredAbortBeforeRequest(t *testing.T) {
	o := newInviteOutcome()
	require.True(t, o.abort())
	o.request()
	require.Equal(t, CancelResultPending, o.wait(10*time.Millisecond))
	o.settle(CancelResultUnconfirmed)
	require.Equal(t, CancelResultUnconfirmed, o.wait(time.Second))
}

// A 2xx that the transaction passed up before the CANCEL deadline is the final
// response, even if the deadline fires before the pump queued it.
func TestCancelIfUnansweredDeadlineKeepsQueuedAnswer(t *testing.T) {
	for i := 0; i < 20; i++ {
		c := newStubOutbound(t)
		c.timers.cancelTimeout = 0
		req := stubInvite()
		tx := newStubInviteTx(t, 10)
		tx.onCancel = func() { tx.responses <- stubResponse(req, sip.StatusOK, "uas-1") } // crossing 200, before the deadline exists
		pump := startRespPump(tx.Responses(), tx.Done())
		c.outcome.request()
		tx.responses <- stubResponse(req, sip.StatusRinging, "uas-1")
		r := <-runWaitInvite(c, context.Background(), tx, pump, true)
		require.NoError(t, r.err, "iteration %d", i)
		require.NotNil(t, r.res, "iteration %d: the crossing 200 must be kept", i)
		require.Equal(t, sip.StatusOK, r.res.StatusCode)
	}
}

// No final response by the CANCEL deadline: unconfirmed, and the transaction is ended.
func TestCancelIfUnansweredDeadlineWithoutFinal(t *testing.T) {
	c := newStubOutbound(t)
	c.timers.cancelTimeout = 50 * time.Millisecond
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	pump := startRespPump(tx.Responses(), tx.Done())
	c.outcome.request()
	tx.responses <- stubResponse(req, sip.StatusRinging, "uas-1")
	r := <-runWaitInvite(c, context.Background(), tx, pump, true)
	require.Nil(t, r.res)
	require.False(t, r.cancelled, "the transaction already ended; no late watcher")
	var e cancelIfUnansweredError
	require.ErrorAs(t, r.err, &e)
	require.Equal(t, CancelResultUnconfirmed, e.result)
	select {
	case <-tx.Done():
	default:
		require.Fail(t, "transaction not terminated")
	}
}

// A backlog of responses cannot starve a request or the CANCEL deadline: the
// request is acted on while most of the backlog is still queued.
func TestCancelIfUnansweredProvisionalFloodDoesNotStarve(t *testing.T) {
	const backlog = 20000
	c := newStubOutbound(t)
	c.timers.cancelTimeout = 0
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	pump := startRespPump(tx.Responses(), tx.Done())
	for i := 0; i < backlog; i++ {
		pump.push(stubResponse(req, sip.StatusCode(183), "uas-1"))
	}
	queuedAtCancel := -1
	tx.onCancel = func() {
		if queuedAtCancel < 0 {
			pump.mu.Lock()
			queuedAtCancel = len(pump.q)
			pump.mu.Unlock()
		}
	}
	c.outcome.request()
	select {
	case r := <-runWaitInvite(c, context.Background(), tx, pump, true):
		var e cancelIfUnansweredError
		require.ErrorAs(t, r.err, &e)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "owner did not return")
	}
	require.Greater(t, queuedAtCancel, backlog/2, "the CANCEL waited for the backlog")
}

// F4: responses passed up before Timer M are handled although the transaction ended.
func TestAcceptedInviteHandlesResponsesQueuedBeforeEnd(t *testing.T) {
	c := newStubOutbound(t)
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	first := stubResponse(req, sip.StatusOK, "uas-1")
	tx.responses <- stubResponse(req, sip.StatusOK, "uas-2") // fork
	pump := startRespPump(tx.Responses(), tx.Done())
	tx.Terminate() // Timer M
	cli := &lockedSIPClient{}
	c.watchAcceptedInvite(cli, tx, pump, req, first, dialogACK(req, first))
	reqs := cli.written()
	require.Equal(t, []sip.RequestMethod{sip.ACK, sip.BYE}, methodsOf(reqs))
	for _, r := range reqs {
		require.Equal(t, "uas-2", toTagOfRequest(r))
	}
}

// A 2xx retransmission is ACKed at once, also before the call sent its own first
// ACK, and also when the transaction ends right after passing it up.
func TestAcceptedInviteAcksRetransmissionBeforeFirstACK(t *testing.T) {
	c := newStubOutbound(t)
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	first := stubResponse(req, sip.StatusOK, "uas-1")
	tx.responses <- stubResponse(req, sip.StatusOK, "uas-1") // retransmission, first ACK lost or not sent yet
	pump := startRespPump(tx.Responses(), tx.Done())
	tx.Terminate() // Timer M right after
	cli := &lockedSIPClient{}
	c.watchAcceptedInvite(cli, tx, pump, req, first, dialogACK(req, first))
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(cli.written()))
	require.Equal(t, "uas-1", toTagOfRequest(cli.written()[0]))
}

// The accepted watcher ends the transaction after acceptedWatch.
func TestAcceptedInviteTerminatesAfterWatch(t *testing.T) {
	c := newStubOutbound(t)
	c.timers.acceptedWatch = 50 * time.Millisecond
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	first := stubResponse(req, sip.StatusOK, "uas-1")
	pump := startRespPump(tx.Responses(), tx.Done())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchAcceptedInvite(&lockedSIPClient{}, tx, pump, req, first, dialogACK(req, first))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watcher did not end")
	}
	select {
	case <-tx.Done():
	default:
		require.Fail(t, "transaction not terminated")
	}
}

// The late watcher ACKs and BYEs every answered dialog, re-ACKs retransmissions,
// and still handles what was passed up when its grace ends.
func TestWatchCancelledInviteHandlesEveryDialog(t *testing.T) {
	defer func(d time.Duration) { cancelResponseGrace = d }(cancelResponseGrace)
	cancelResponseGrace = 0
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	tx.responses <- stubResponse(req, sip.StatusOK, "uas-1")
	tx.responses <- stubResponse(req, sip.StatusOK, "uas-1") // retransmission
	tx.responses <- stubResponse(req, sip.StatusOK, "uas-2") // fork
	pump := startRespPump(tx.Responses(), tx.Done())
	cli := &lockedSIPClient{}
	watchCancelledInviteFrom(logger.NewTestLogger(t), cli, nil, req, tx, pump, nil)
	reqs := cli.written()
	require.Equal(t, []sip.RequestMethod{sip.ACK, sip.BYE, sip.ACK, sip.ACK, sip.BYE}, methodsOf(reqs))
	require.Equal(t, []string{"uas-1", "uas-1", "uas-1", "uas-2", "uas-2"}, []string{
		toTagOfRequest(reqs[0]), toTagOfRequest(reqs[1]), toTagOfRequest(reqs[2]), toTagOfRequest(reqs[3]), toTagOfRequest(reqs[4])})
}

// A call that fails after the answer (media) ACKs the 2xx before its BYE.
func TestOutboundMediaFailureAcksBeforeBye(t *testing.T) {
	c := startCancelTestCall(t, nil)
	res := sip.NewResponseFromRequest(c.invite.req, sip.StatusOK, "OK",
		[]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 18\r\na=rtpmap:18 G729/8000\r\n"))
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.To().Params.Add("tag", "uas-1")
	require.Eventually(t, func() bool { return c.invite.transaction.SendResponse(res) == nil }, 2*time.Second, time.Millisecond)
	ack := c.nextRequest(2 * time.Second)
	require.NotNil(t, ack, "the 2xx must be ACKed")
	require.Equal(t, sip.ACK, ack.req.Method)
	bye := c.nextTransaction(2 * time.Second)
	require.NotNil(t, bye)
	require.Equal(t, sip.BYE, bye.req.Method)
	require.Less(t, ack.sequence, bye.sequence, "ACK before BYE")
	resp := sip.NewResponseFromRequest(bye.req, sip.StatusOK, "OK", nil)
	require.Eventually(t, func() bool { return bye.transaction.SendResponse(resp) == nil }, 2*time.Second, time.Millisecond)
	require.Error(t, c.waitDone(2*time.Second), "the call fails on the media error")
}

// A backlog of responses cannot keep a watcher from ending the transaction on time.
func TestInviteWatchersEndOnTimeUnderBacklog(t *testing.T) {
	const backlog = 20000
	setup := func(t *testing.T) (*stubInviteTx, *respPump, *int, *sip.Request) {
		req := stubInvite()
		tx := newStubInviteTx(t, 10)
		pump := startRespPump(tx.Responses(), tx.Done())
		for i := 0; i < backlog; i++ {
			pump.push(stubResponse(req, sip.StatusOK, "uas-1"))
		}
		queued := -1
		tx.onTerminate = func() {
			pump.mu.Lock()
			queued = len(pump.q)
			pump.mu.Unlock()
		}
		return tx, pump, &queued, req
	}
	t.Run("accepted", func(t *testing.T) {
		c := newStubOutbound(t)
		c.timers.acceptedWatch = 0
		tx, pump, queued, req := setup(t)
		first := stubResponse(req, sip.StatusOK, "uas-1")
		cli := &lockedSIPClient{}
		c.watchAcceptedInvite(cli, tx, pump, req, first, dialogACK(req, first))
		require.Greater(t, *queued, backlog/2, "the transaction ended only after the backlog")
		require.Len(t, cli.written(), backlog, "every queued 2xx is still ACKed")
	})
	t.Run("cancelled", func(t *testing.T) {
		defer func(d time.Duration) { cancelResponseGrace = d }(cancelResponseGrace)
		cancelResponseGrace = 0
		tx, pump, queued, req := setup(t)
		cli := &lockedSIPClient{}
		watchCancelledInviteFrom(logger.NewTestLogger(t), cli, nil, req, tx, pump, nil)
		require.Greater(t, *queued, backlog/2, "the transaction ended only after the backlog")
		require.Len(t, cli.written(), backlog+1, "ACK+BYE once, then every retransmission ACKed")
	})
}

// Any 2xx answers the INVITE, not only 200: it is ACKed and kept.
func TestOutboundNon200SuccessIsAnAnswer(t *testing.T) {
	c := startCancelTestCall(t, nil)
	res := sip.NewSDPResponseFromRequest(c.invite.req, []byte(cancelTestSDP))
	res.StatusCode = sip.StatusCode(202)
	res.Reason = "Accepted"
	res.To().Params.Add("tag", "uas-1")
	require.Eventually(t, func() bool { return c.invite.transaction.SendResponse(res) == nil }, 2*time.Second, time.Millisecond)
	require.NoError(t, c.waitDone(2*time.Second))
	require.Equal(t, []sip.RequestMethod{sip.ACK}, methodsOf(c.requests(300*time.Millisecond)))
	require.Nil(t, c.nextTransaction(100*time.Millisecond), "no BYE")
}

// A request whose call ends before any INVITE was sent (a failure before the
// INVITE, or shutdown) settles as cancelled, published before the participant leaves.
func TestCancelIfUnansweredSettlesWhenTheCallEndsBeforeAnyInvite(t *testing.T) {
	client := NewTestSIP(t, TestSIPConfig{}).manager
	req := MinimalCreateSIPParticipantRequest()
	mconf, err := newMediaConfig(&livekit.SIPMediaConfig{}, client.conf.MediaTimeout)
	require.NoError(t, err)
	state := NewCallState(NewRPCStateHandler(&MockIOInfoClient{}), &livekit.SIPCallInfo{})
	tr := TransportFrom(req.Transport)
	uri := CreateURIFromUserAndAddress(req.CallTo, req.Address, tr).GetURI()
	from := &sip.FromHeader{Address: sip.Uri{User: req.Number, Host: req.Hostname}, Params: sip.NewParams()}
	call, err := client.newCall(context.Background(), traceid.FromGUID(req.SipCallId), client.conf, logger.NewTestLogger(t),
		LocalTag(req.SipCallId), RoomConfig{RoomName: "test-room", Participant: ParticipantConfig{Identity: "test-participant"}},
		sipOutboundConfig{transport: req.Transport, uri: uri, to: &sip.ToHeader{Address: *uri}, from: from, mediaConfig: mconf}, state, "")
	require.NoError(t, err)
	var (
		mu        sync.Mutex
		published []CancelResult
		roomGone  bool
	)
	call.cc.outcome.setPublisher(func(res CancelResult) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, res)
		select {
		case <-call.lkRoom.Closed():
			roomGone = true
		default:
		}
	})
	call.cc.outcome.request()
	require.Equal(t, CancelResultPending, call.cc.outcome.wait(50*time.Millisecond))
	require.NoError(t, call.Close(context.Background()))
	require.Equal(t, CancelResultCancelled, call.cc.outcome.wait(time.Second))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []CancelResult{CancelResultCancelled}, published)
	require.False(t, roomGone, "the result is published before the participant leaves")
}

// The request event reports the hand-off to the transaction, never a CANCEL
// on the wire: sipgo ignores Cancel once the INVITE has a final response.
func TestCancelIfUnansweredEventIsARequest(t *testing.T) {
	c := newStubOutbound(t)
	var (
		mu  sync.Mutex
		evs []SignalingEventType
	)
	c.emit = func(ev SignalingEvent) {
		mu.Lock()
		defer mu.Unlock()
		evs = append(evs, ev.Type)
	}
	req := stubInvite()
	tx := newStubInviteTx(t, 10)
	// The crossing 200 is passed up while Cancel runs: sipgo ignores that Cancel.
	tx.onCancel = func() { tx.responses <- stubResponse(req, sip.StatusOK, "uas-1") }
	pump := startRespPump(tx.Responses(), tx.Done())
	c.outcome.request()
	tx.responses <- stubResponse(req, sip.StatusRinging, "uas-1")
	r := <-runWaitInvite(c, context.Background(), tx, pump, true)
	require.NoError(t, r.err)
	require.Equal(t, sip.StatusOK, r.res.StatusCode)
	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, evs, EventCancelRequested)
	require.NotContains(t, evs, EventCancelSent)
}
