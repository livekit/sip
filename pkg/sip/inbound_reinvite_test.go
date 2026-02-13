// Copyright 2023 LiveKit, Inc.
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
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sipgo/sip"
)

// TestHandleReinvite_MissingCSeq tests handling of re-INVITE without CSeq header
func TestHandleReinvite_MissingCSeq(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	// Intentionally don't add CSeq header

	tx := &testServerTransaction{}
	call, _ := newTestInboundCall(t, 1, []byte("v=0\r\n"))

	call.s.processInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusBadRequest, tx.responses[0].StatusCode, "should respond with 400 Bad Request")
}

// TestHandleReinvite_Retransmission tests re-INVITE retransmission (same CSeq)
func TestHandleReinvite_Retransmission(t *testing.T) {
	currentSDP := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\n")
	call, _ := newTestInboundCall(t, 1, currentSDP)

	invite := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	invite.AppendHeader(&sip.CSeqHeader{
		SeqNo:      1,
		MethodName: sip.INVITE,
	})
	// Reinvite should receive the copy of the previous response.
	call.cc.inviteOk = sip.NewResponseFromRequest(invite, sip.StatusOK, "OK", currentSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      1, // Same as original INVITE
		MethodName: sip.INVITE,
	})

	tx := &testServerTransaction{}
	call.s.processInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
	require.Equal(t, currentSDP, tx.responses[0].Body(), "should send current SDP")
}

// TestHandleReinvite_OldCSeq tests handling of out-of-order INVITE
func TestHandleReinvite_OldCSeq(t *testing.T) {
	call, _ := newTestInboundCall(t, 5, []byte("v=0\r\n"))

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      3, // Lower than current (5)
		MethodName: sip.INVITE,
	})

	tx := &testServerTransaction{}
	call.s.processInvite(req, tx)

	require.Len(t, tx.responses, 0, "should not respond to old INVITE")
}

// TestHandleReinvite_NewReInvite tests normal re-INVITE with higher CSeq
func TestHandleReinvite_NewReInvite(t *testing.T) {
	currentSDP := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\n")
	call, handler := newTestInboundCall(t, 1, currentSDP)

	req := NewINVITE("")
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      2, // Higher than original (1)
		MethodName: sip.INVITE,
	})
	req.SetBody([]byte("v=0\r\no=- 789 012 IN IP4 5.6.7.8\r\n"))

	req.SetSource("1.2.3.4:5060")

	tx := &testServerTransaction{}
	handler.AddAuthResponse(req.To().Address.Host, AuthAccept)
	call.s.processInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
	require.Equal(t, currentSDP, tx.responses[0].Body(), "should send current SDP, not offered SDP")

	// Verify Content-Type header
	ctHeader := tx.responses[0].GetHeader("Content-Type")
	require.NotNil(t, ctHeader, "should have Content-Type header")
	require.Equal(t, "application/sdp", ctHeader.Value(), "Content-Type should be application/sdp")
}

// TestHandleReinvite_NoSDP tests error handling when no SDP available
func TestHandleReinvite_NoSDP(t *testing.T) {
	call, _ := newTestInboundCall(t, 1, nil) // No SDP!
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      1,
		MethodName: sip.INVITE,
	})

	// Setup call, then send empty reinvite

	tx := &testServerTransaction{}
	call.s.processInvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusInternalServerError, tx.responses[0].StatusCode,
		"should respond with 500 when no SDP available")
}

// Test helpers

func NewINVITE(sipCallID string) *sip.Request {
	to := sip.Uri{User: "calee", Host: "livekit.io"}
	from := sip.Uri{User: "caller", Host: "caller.com"}
	req := sip.NewRequest(sip.INVITE, to)
	if sipCallID == "" {
		sipCallID = "test-call-id"
	}
	callID := sip.CallIDHeader(sipCallID)
	req.AppendHeader(&callID)
	fromHeader := &sip.FromHeader{Address: from}
	fromHeader.Params = sip.NewParams()
	fromHeader.Params.Add("tag", "test-from-tag")
	req.AppendHeader(fromHeader)
	req.AppendHeader(&sip.ToHeader{Address: to})
	return req
}

// newTestInboundCall creates a minimal inboundCall for testing
func newTestInboundCall(t *testing.T, inviteCSeq uint32, sdp []byte) (*inboundCall, *testInboundCallHandler) {
	log := logger.NewTestLoggerLevel(t, 1)
	config := &config.Config{
		NodeID:            "test-node",
		MaxCpuUtilization: 0.99,
	}
	sconf := &ServiceConfig{
		SignalingIP: netip.MustParseAddr("4.3.2.1"),
		MediaIP:     netip.MustParseAddr("4.3.2.1"),
	}
	handler := NewTestInboundCallHandler()

	mon, err := stats.NewMonitor(config)
	require.NoError(t, err)
	err = mon.Start(config)
	require.NoError(t, err)
	t.Cleanup(func() { mon.Stop() })

	srv := &Server{
		log:            log,
		conf:           config,
		sconf:          sconf,
		mon:            mon,
		handler:        &TestHandler{GetAuthCredentialsFunc: handler.GetAuthCredentials},
		getIOClient:    func(_ string) rpc.IOInfoClient { return &testRPCClient{} },
		infos: struct {
			sync.Mutex
			byCallID *expirable.LRU[string, *inboundCallInfo]
		}{
			byCallID: expirable.NewLRU[string, *inboundCallInfo](5, nil, time.Minute),
		},
	}
	call := &inboundCall{
		s: srv,
		cc: &sipInbound{
			log:        log,
			s:          srv,
			inviteCSeq: inviteCSeq,
			lastSDP:    sdp,
		},
	}
	call.logPtr.Store(&log)
	return call, handler
}

type testInboundCallHandler struct {
	mu       sync.Mutex
	callAuth map[string][]AuthResult
}

func NewTestInboundCallHandler() *testInboundCallHandler {
	return &testInboundCallHandler{
		callAuth: make(map[string][]AuthResult),
	}
}

func (h *testInboundCallHandler) AddAuthResponse(key string, auth AuthResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	slice, ok := h.callAuth[key]
	if !ok {
		slice = make([]AuthResult, 0)
	}
	slice = append(slice, auth)
	h.callAuth[key] = slice
}

func (h *testInboundCallHandler) GetAuthResponse(key string) AuthResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	slice, ok := h.callAuth[key]
	if !ok {
		return AuthNotFound
	}
	if len(slice) == 0 {
		return AuthNotFound
	}
	res := slice[0]
	slice = slice[1:]
	h.callAuth[key] = slice
	return res
}

func (h *testInboundCallHandler) GetAuthCredentials(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
	return AuthInfo{
		Result:    h.GetAuthResponse(call.To.Host),
		ProjectID: "p_testProject",
		TrunkID:   "ST_testTrunk",
		Username:  "test-username",
		Password:  "test-password",
	}, nil
}

// testServerTransaction implements sip.ServerTransaction for testing
type testServerTransaction struct {
	responses []*sip.Response
}

func (t *testServerTransaction) Respond(r *sip.Response) error {
	t.responses = append(t.responses, r)
	return nil
}

func (t *testServerTransaction) Acks() <-chan *sip.Request {
	return make(chan *sip.Request)
}

func (t *testServerTransaction) Cancels() <-chan *sip.Request {
	return make(chan *sip.Request)
}

func (t *testServerTransaction) Done() <-chan struct{} {
	return make(chan struct{})
}

func (t *testServerTransaction) Err() error {
	return nil
}

func (t *testServerTransaction) Terminate() {}

type testRPCClient struct {
	rpc.IOInfoClient
}

func (*testRPCClient) UpdateSIPCallState(_ context.Context, _ *rpc.UpdateSIPCallStateRequest, _ ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
