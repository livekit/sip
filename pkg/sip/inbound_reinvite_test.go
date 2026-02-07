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
	"testing"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// TestHandleReinvite_MissingCSeq tests handling of re-INVITE without CSeq header
func TestHandleReinvite_MissingCSeq(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	// Intentionally don't add CSeq header

	tx := &testServerTransaction{}
	call := newTestInboundCall(1, []byte("v=0\r\n"))

	call.handleReinvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusBadRequest, tx.responses[0].StatusCode, "should respond with 400 Bad Request")
}

// TestHandleReinvite_Retransmission tests re-INVITE retransmission (same CSeq)
func TestHandleReinvite_Retransmission(t *testing.T) {
	currentSDP := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\n")
	call := newTestInboundCall(1, currentSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      1, // Same as original INVITE
		MethodName: sip.INVITE,
	})

	tx := &testServerTransaction{}
	call.handleReinvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusOK, tx.responses[0].StatusCode, "should respond with 200 OK")
	require.Equal(t, currentSDP, tx.responses[0].Body(), "should send current SDP")
}

// TestHandleReinvite_OldCSeq tests handling of out-of-order INVITE
func TestHandleReinvite_OldCSeq(t *testing.T) {
	call := newTestInboundCall(5, []byte("v=0\r\n"))

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      3, // Lower than current (5)
		MethodName: sip.INVITE,
	})

	tx := &testServerTransaction{}
	call.handleReinvite(req, tx)

	require.Len(t, tx.responses, 0, "should not respond to old INVITE")
}

// TestHandleReinvite_NewReInvite tests normal re-INVITE with higher CSeq
func TestHandleReinvite_NewReInvite(t *testing.T) {
	currentSDP := []byte("v=0\r\no=- 123 456 IN IP4 1.2.3.4\r\n")
	call := newTestInboundCall(1, currentSDP)

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      2, // Higher than original (1)
		MethodName: sip.INVITE,
	})
	req.SetBody([]byte("v=0\r\no=- 789 012 IN IP4 5.6.7.8\r\n"))

	tx := &testServerTransaction{}
	call.handleReinvite(req, tx)

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
	call := newTestInboundCall(1, nil) // No SDP!

	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "test.com"})
	req.AppendHeader(&sip.CSeqHeader{
		SeqNo:      2,
		MethodName: sip.INVITE,
	})

	tx := &testServerTransaction{}
	call.handleReinvite(req, tx)

	require.Len(t, tx.responses, 1, "should send one response")
	require.Equal(t, sip.StatusInternalServerError, tx.responses[0].StatusCode,
		"should respond with 500 when no SDP available")
}

// Test helpers

// newTestInboundCall creates a minimal inboundCall for testing
func newTestInboundCall(inviteCSeq uint32, sdp []byte) *inboundCall {
	log := logger.GetLogger()
	call := &inboundCall{
		cc: &sipInbound{
			inviteCSeq: inviteCSeq,
			lastSDP:    sdp,
		},
	}
	call.logPtr.Store(&log)
	return call
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

// TestNonceUniqueness tests that nonce generation includes Call-ID for uniqueness
func TestNonceUniqueness(t *testing.T) {
	// This test verifies the nonce collision fix by actually calling the helper function
	// Two calls with different Call-IDs should get different nonces
	// even if generated at the same microsecond

	callID1 := "call1-123@test.com"
	callID2 := "call2-456@test.com"

	// Actually call the generateNonce helper function
	nonce1 := generateNonce(callID1)
	nonce2 := generateNonce(callID2)

	// Verify nonces are different (due to different Call-IDs)
	require.NotEqual(t, nonce1, nonce2, "nonces should be different for different Call-IDs")

	// Verify nonce format includes Call-ID
	require.Contains(t, nonce1, callID1, "nonce should contain Call-ID")
	require.Contains(t, nonce2, callID2, "nonce should contain Call-ID")

	// Verify nonce format includes timestamp
	require.Contains(t, nonce1, "-", "nonce should have timestamp-callid format")
	require.Contains(t, nonce2, "-", "nonce should have timestamp-callid format")
}
