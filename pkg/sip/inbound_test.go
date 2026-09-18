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

package sip

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sipgo/sip"
)

func TestProviderLabel(t *testing.T) {
	cases := []struct {
		name string
		info *livekit.ProviderInfo
		exp  string
	}{
		{
			name: "nil",
			info: nil,
			exp:  stats.ProviderUnknown,
		},
		{
			name: "internal",
			info: &livekit.ProviderInfo{Name: "someCarrier", Type: livekit.ProviderType_PROVIDER_TYPE_INTERNAL},
			exp:  "internal/somecarrier",
		},
		{
			name: "internal without a name",
			info: &livekit.ProviderInfo{Type: livekit.ProviderType_PROVIDER_TYPE_INTERNAL},
			exp:  "internal/unknown",
		},
		{
			name: "external",
			info: &livekit.ProviderInfo{
				Id:   "ST_customerTrunk",
				Name: "Some Customer's Twilio Trunk",
				Type: livekit.ProviderType_PROVIDER_TYPE_EXTERNAL,
			},
			exp: "external",
		},
		{
			name: "external without a name",
			info: &livekit.ProviderInfo{Type: livekit.ProviderType_PROVIDER_TYPE_EXTERNAL},
			exp:  "external",
		},
		{
			name: "unknown type",
			info: &livekit.ProviderInfo{Name: "someCarrier"},
			exp:  stats.ProviderUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.exp, providerLabel(c.info))
		})
	}
}

// lateOfferCall is an inbound call started with an offerless INVITE (RFC 3261 §13.2.1, "late offer").
type lateOfferCall struct {
	st     *serviceTest
	call   *sipUADialogTest
	invite *sip.Request
	tx     sip.ClientTransaction
	byes   <-chan *sipUARequest // BYE requests sent by the server for this dialog

	// Set by expectOffer.
	ok    *sip.Response // first 200 OK received
	offer *sdp.Offer    // SDP offer carried by the 200 OK
	ic    *inboundCall
}

// inviteWithoutOffer sends an INVITE with no body.
func inviteWithoutOffer(t *testing.T, st *serviceTest) *lateOfferCall {
	t.Helper()

	call := newTestCall(st.TestUA, false)
	byes := call.RegisterRequestChannel(string(sip.BYE))
	t.Cleanup(func() { call.UnregisterRequestChannel(string(sip.BYE)) })

	req := call.NewRequest(sip.INVITE) // no body, no Content-Type
	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)

	return &lateOfferCall{
		st:     st,
		call:   call,
		invite: req,
		tx:     tx,
		byes:   byes,
	}
}

// requireOffer asserts that resp is a 200 OK carrying an SDP offer, and returns the parsed offer.
func requireOffer(t *testing.T, resp *sip.Response) *sdp.Offer {
	t.Helper()
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "offerless INVITE should get 200 OK")
	ct := resp.ContentType()
	require.NotNil(t, ct, "200 OK for an offerless INVITE must declare a Content-Type")
	require.Equal(t, contentTypeSDP, ct.Value())
	require.NotEmpty(t, resp.Body(), "200 OK for an offerless INVITE must carry an SDP offer")
	offer, err := sdp.ParseOfferWith(defaultCodecs, resp.Body())
	require.NoError(t, err, "200 OK body should be a parsable SDP offer")
	return offer
}

// expectOffer waits for the final response to the INVITE and asserts it is a 200 OK with an SDP offer.
func (c *lateOfferCall) expectOffer(t *testing.T, ctx context.Context) {
	t.Helper()

	resp := getFinalResponseOrFail(t, ctx, c.tx)
	c.ok = resp
	c.offer = requireOffer(t, resp)

	remoteTag, ok := resp.To().Params.Get("tag")
	require.True(t, ok, "remote tag should be present")
	c.call.SetRemoteTag(LocalTag(remoteTag))
	c.call.SetRemoteSDP(resp.Body())
	c.call.SetRouteSet(resp, true)

	c.st.Server.cmu.Lock()
	c.ic, ok = c.st.Server.byLocalTag[c.call.remoteTag]
	c.st.Server.cmu.Unlock()
	require.True(t, ok, "call should be registered")
}

// requireRetransmit asserts that resp is a retransmission of the 200 OK recorded by expectOffer.
func (c *lateOfferCall) requireRetransmit(t *testing.T, resp *sip.Response) {
	t.Helper()
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "retransmission should be a 200 OK")
	require.Equal(t, c.ok.To().Params.GetOr("tag", ""), resp.To().Params.GetOr("tag", ""), "retransmission should belong to the same dialog")
	require.Equal(t, c.ok.Body(), resp.Body(), "retransmission should carry the same offer")
}

// media returns the call's media port.
func (c *lateOfferCall) media() MediaPort {
	c.ic.mmu.Lock()
	defer c.ic.mmu.Unlock()
	return c.ic.media
}

// answer builds an SDP answer for the offer received in the 200 OK.
func (c *lateOfferCall) answer(t *testing.T, addr netip.AddrPort) []byte {
	t.Helper()
	ans, _, err := c.offer.Answer(addr.Addr(), int(addr.Port()), sdp.EncryptionNone)
	require.NoError(t, err)
	data, err := ans.SDP.Marshal()
	require.NoError(t, err)
	return data
}

func (c *lateOfferCall) ack(t *testing.T, body []byte) {
	t.Helper()
	ack := sip.NewAckRequest(c.invite, c.ok, body)
	if body != nil {
		ack.AppendHeader(sip.NewHeader("Content-Type", contentTypeSDP))
	}
	require.NoError(t, c.st.TestUA.Client.WriteRequest(ack))
}

// nextResponse waits for another response on the INVITE transaction, i.e. a retransmitted 200 OK.
func (c *lateOfferCall) nextResponse(t *testing.T, ctx context.Context) *sip.Response {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for a retransmitted response: %v", ctx.Err())
	case <-c.tx.Done():
		t.Fatal("INVITE transaction terminated while waiting for a retransmitted response")
	case resp := <-c.tx.Responses():
		return resp
	}
	return nil
}

// expectBye waits for the server to send a BYE for this dialog and answers it with 200 OK.
func (c *lateOfferCall) expectBye(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for BYE from server: %v", ctx.Err())
	case msg := <-c.byes:
		c.answerBye(t, msg)
	}
}

// answerBye asserts that msg is a BYE for this dialog and answers it with 200 OK.
func (c *lateOfferCall) answerBye(t *testing.T, msg *sipUARequest) {
	t.Helper()
	require.NotNil(t, msg)
	require.Equal(t, sip.BYE, msg.req.Method)
	require.Equal(t, string(c.call.localTag), msg.req.To().Params.GetOr("tag", ""))
	require.NoError(t, msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 200, "OK", nil)))
}

// expectActive asserts that the server has negotiated media.
func (c *lateOfferCall) expectActive(t *testing.T, remote netip.AddrPort) {
	t.Helper()
	require.Eventually(t, func() bool {
		return c.media().NegotiatedAudio() != nil
	}, 5*time.Second, 10*time.Millisecond, "media should be negotiated")
	require.Equal(t, remote, getMediaPortRemoteAddr(t, c.media()), "RTP destination should come from the answer in the ACK")
	require.True(t, c.ic.cc.GotACK(), "server should have recorded the ACK")
	require.Eventually(t, c.ic.started.IsBroken, 5*time.Second, 10*time.Millisecond, "call should become active")
	require.False(t, c.ic.done.Load(), "call should still be up")
}

// expectClosedWithoutMedia asserts the server tore the call down without ever
// having negotiated media.
func (c *lateOfferCall) expectClosedWithoutMedia(t *testing.T) {
	t.Helper()
	require.Eventually(t, c.ic.done.Load, 5*time.Second, 10*time.Millisecond, "call should be closed")
	require.Nil(t, c.media().NegotiatedAudio(), "media must not be negotiated without a valid answer")
}

// reinvite sends an in-dialog INVITE with a fresh offer from the caller and returns the final response.
// sipgo ACKs non-2xx responses itself; a 2xx is ACKed here.
func (c *lateOfferCall) reinvite(t *testing.T, ctx context.Context) *sip.Response {
	t.Helper()
	req, _, err := c.call.Invite(nil)
	require.NoError(t, err)
	tx, err := c.st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)
	resp := getFinalResponseOrFail(t, ctx, tx)
	if resp.StatusCode < 300 {
		require.NoError(t, c.st.TestUA.Client.WriteRequest(sip.NewAckRequest(req, resp, nil)))
	}
	return resp
}

// hangup ends an established call from the caller side.
func (c *lateOfferCall) hangup(t *testing.T) {
	t.Helper()
	resp := c.call.TransactionRequest(t, c.call.NewRequest(sip.BYE))
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "BYE should get 200 OK")
}

func TestInboundLateOfferDisabled(t *testing.T) {
	// No feature flags: late offer is off for the project.
	st := NewServiceTest(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	c := inviteWithoutOffer(t, st)
	resp := getFinalResponseOrFail(t, ctx, c.tx)
	// Same rejection as before late offer support: negotiating an empty offer fails.
	require.Equal(t, sip.StatusBadRequest, resp.StatusCode, "offerless INVITE should be rejected when late offer is disabled")
	require.Empty(t, resp.Body(), "rejection must not carry an offer")

	// The response is sent before the call is deregistered.
	require.Eventually(t, func() bool {
		st.Server.cmu.RLock()
		defer st.Server.cmu.RUnlock()
		return len(st.Server.byLocalTag) == 0
	}, 5*time.Second, 10*time.Millisecond, "rejected call should be deregistered")
}

func TestInboundLateOffer(t *testing.T) {
	st := NewServiceTest(t, nil)
	// Enable late offer at the project level.
	st.Server.SetHandler(&TestHandler{FeatureFlags: map[string]string{lateOfferFeatureFlag: "true"}})
	callerRTP := netip.MustParseAddrPort("127.0.0.1:2827")

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := inviteWithoutOffer(t, st)
		c.expectOffer(t, ctx)

		// The offer must point at the media port allocated for this call.
		require.Equal(t, getMediaPort(t, c.media()).Port(), int(c.offer.Addr.Port()), "offer should advertise the call's RTP port")
		// Nothing can be negotiated until the answer arrives.
		require.Nil(t, c.media().NegotiatedAudio(), "media must not be negotiated before the ACK")

		c.ack(t, c.answer(t, callerRTP))
		c.expectActive(t, callerRTP)
		t.Cleanup(func() { c.hangup(t) })

		// Once ACKed, the 200 OK must not be retransmitted.
		expectNoResponse(t, c.tx)
	})

	t.Run("delayed_ack", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := inviteWithoutOffer(t, st)
		c.expectOffer(t, ctx)

		// Withhold the ACK: the UAS must retransmit the 200 OK, with the same offer.
		c.requireRetransmit(t, c.nextResponse(t, ctx))
		require.Nil(t, c.media().NegotiatedAudio(), "media must not be negotiated before the ACK")

		c.ack(t, c.answer(t, callerRTP))
		c.expectActive(t, callerRTP)
		t.Cleanup(func() { c.hangup(t) })

		expectNoResponse(t, c.tx)
	})

	t.Run("ack_never_arrives", func(t *testing.T) {
		t.Parallel()
		// UDP retries back off from 250ms to 3s; giving up takes ~10s.
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		c := inviteWithoutOffer(t, st)
		c.expectOffer(t, ctx)

		// Count 200 OK retransmissions until the server gives up and sends BYE.
		retransmits := 0
	loop:
		for {
			select {
			case <-ctx.Done():
				t.Fatalf("timed out waiting for the server to give up on the ACK: %v", ctx.Err())
			case resp := <-c.tx.Responses():
				c.requireRetransmit(t, resp)
				retransmits++
			case msg := <-c.byes:
				c.answerBye(t, msg)
				break loop
			}
		}
		require.GreaterOrEqual(t, retransmits, 2, "200 OK should be retransmitted while waiting for the ACK")
		require.False(t, c.ic.cc.GotACK(), "server received unexpected ACK")
		c.expectClosedWithoutMedia(t)
	})

	t.Run("ack_without_answer", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := inviteWithoutOffer(t, st)
		c.expectOffer(t, ctx)
		c.ack(t, nil)

		c.expectBye(t, ctx)
		c.expectClosedWithoutMedia(t)
	})

	t.Run("ack_with_invalid_answer", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := inviteWithoutOffer(t, st)
		c.expectOffer(t, ctx)
		c.ack(t, []byte("invalid SDP answer"))

		c.expectBye(t, ctx)
		c.expectClosedWithoutMedia(t)
	})

	t.Run("reinvite_before_ack", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := inviteWithoutOffer(t, st)
		c.expectOffer(t, ctx)

		// Our offer is still unanswered: a re-INVITE cannot be negotiated yet.
		resp := c.reinvite(t, ctx)
		require.Equal(t, statusRequestPending, resp.StatusCode, "re-INVITE before the late answer should get 491")
		require.Nil(t, c.media().NegotiatedAudio(), "rejected re-INVITE must not negotiate media")

		// The pending exchange still completes normally.
		c.ack(t, c.answer(t, callerRTP))
		c.expectActive(t, callerRTP)
		t.Cleanup(func() { c.hangup(t) })

		// With the exchange complete, re-INVITEs are accepted again, and the reply must be the
		// negotiated SDP rather than the multi-codec offer we sent in the original 200 OK.
		resp = c.reinvite(t, ctx)
		require.Equal(t, sip.StatusCode(200), resp.StatusCode, "re-INVITE after the late answer should get 200 OK")
		localSDP, err := c.media().GetLocalSDP()
		require.NoError(t, err)
		require.Equal(t, localSDP, resp.Body(), "re-INVITE reply should carry the negotiated local SDP")
		require.NotEqual(t, c.ok.Body(), resp.Body(), "re-INVITE reply must not echo the original offer")
	})
}

// TestInboundRoomIDReportedBeforeAnswer covers TEL-1048: the room identity must
// be recorded on the call state as soon as the room is joined, not only once the
// call goes active. A caller that hangs up while the call is still ringing would
// otherwise be reported with an empty RoomId, leaving the call record
// unattributable to the room it had already joined.
func TestInboundRoomIDReportedBeforeAnswer(t *testing.T) {
	states := &recordingStateHandler{}
	// ringForever keeps the call in the ringing state: the room never reports a
	// track subscription, so the server never answers the INVITE.
	st := NewServiceTest(t, &serviceTestConfig{
		GetRoom:         newTestRoomConfig(&testRoomConfig{ringForever: true}),
		GetStateHandler: states.GetStateHandler(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	inviteTx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer inviteTx.Terminate()

	res100 := getResponseOrFailTimeout(t, ctx, inviteTx)
	require.Equal(t, sip.StatusCode(100), res100.StatusCode, "should receive 100 Trying")
	res180 := getResponseOrFailTimeout(t, ctx, inviteTx)
	require.Equal(t, sip.StatusCode(180), res180.StatusCode, "should receive 180 Ringing")
	remoteTag, ok := res180.To().Params.Get("tag")
	require.True(t, ok, "remote tag should be present")
	call.SetRemoteTag(LocalTag(remoteTag))

	// The room is joined before the call is answered, so its identity must be
	// reported upstream while the call is still ringing.
	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.RoomId != ""
	}, 5*time.Second, 10*time.Millisecond, "room SID should be reported while the call is still ringing")

	ringing := states.Last()
	require.Equal(t, testRoomSID, ringing.RoomId)
	require.Equal(t, testRoomName, ringing.RoomName)
	require.Zero(t, ringing.StartedAtNs, "call must not have gone active yet")

	// The caller hangs up before the call is ever answered.
	require.NoError(t, inviteTx.Cancel(), "should be able to send CANCEL")
	res := getFinalResponseOrFail(t, ctx, inviteTx)
	require.Equal(t, sip.StatusCode(487), res.StatusCode, "CANCEL should terminate the INVITE")

	// The room identity must survive on the final record of the abandoned call.
	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the ended call should be reported")

	ended := states.Last()
	require.Equal(t, testRoomSID, ended.RoomId, "room SID must be retained on the ended call")
	require.Equal(t, testRoomName, ended.RoomName, "room name must be retained on the ended call")
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// TestInboundRoomIDReportedOnAnsweredCall is the companion to
// TestInboundRoomIDReportedBeforeAnswer: reporting the room identity early must
// not disturb the identity reported for a call that is answered normally, and
// once reported it must never be dropped from a later update.
func TestInboundRoomIDReportedOnAnsweredCall(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})

	_, ic := st.CreateInboundCall(t)
	require.Eventually(t, ic.started.IsBroken, 5*time.Second, 10*time.Millisecond, "call should become active")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.CallStatus == livekit.SIPCallStatus_SCS_ACTIVE
	}, 5*time.Second, 10*time.Millisecond, "active call should be reported")

	active := states.Last()
	require.Equal(t, testRoomSID, active.RoomId)
	require.Equal(t, testRoomName, active.RoomName)

	// The identity is reported from the join onwards, and never regresses.
	joined := false
	for i, u := range states.Updates() {
		if u.RoomId == "" {
			require.False(t, joined, "update %d dropped the room identity after it was reported", i)
			continue
		}
		joined = true
		require.Equal(t, testRoomSID, u.RoomId, "update %d reported an unexpected room SID", i)
		require.Equal(t, testRoomName, u.RoomName, "update %d reported an unexpected room name", i)
	}
	require.True(t, joined, "room identity should have been reported at least once")
}

// TestInboundCallStatusCodeOnNormalHangup covers the reported SIP status of a
// call that was answered and then completed normally: the caller sends BYE on an
// established dialog, so the status recorded on the call must be the 200 OK the
// INVITE was answered with, not one of the terminal rejection codes used for
// calls that never got that far.
func TestInboundCallStatusCodeOnNormalHangup(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})

	call, ic := st.CreateInboundCall(t)
	require.Eventually(t, ic.started.IsBroken, 5*time.Second, 10*time.Millisecond, "call should become active")

	// The caller hangs up an established call.
	resp := st.TestUA.TransactionRequest(t, call.NewRequest(sip.BYE), true)
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "BYE should be accepted")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the ended call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_DISCONNECTED, ended.CallStatus)
	require.NotZero(t, ended.StartedAtNs, "call was answered")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code, "sip status code must be recorded")
}

// TestInboundCallStatusCodeOnDispatchDrop covers the SIP status reported for a
// call that is dropped by DispatchNoRuleDrop: the caller is rung and then gets
// no final response at all, but the call record must still carry the rejection
// status so the drop is not reported as an unknown outcome.
func TestInboundCallStatusCodeOnDispatchDrop(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		return CallDispatch{Result: DispatchNoRuleDrop}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	// Dispatch is evaluated after ringing starts, so the caller sees the
	// provisional responses and then silence: a dropped call gets no final
	// response.
	res100 := getResponseOrFailTimeout(t, ctx, tx)
	require.Equal(t, sip.StatusCode(100), res100.StatusCode, "should receive 100 Trying")
	res180 := getResponseOrFailTimeout(t, ctx, tx)
	require.Equal(t, sip.StatusCode(180), res180.StatusCode, "should receive 180 Ringing")
	expectNoResponse(t, tx)

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the dropped call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.Nil(t, ended.CallStatusCode, "CallStatusCode must not be set")
}

// TestInboundCallStatusCodeOnRoomClosed covers the SIP status reported for an
// answered call that is ended by the LiveKit room closing rather than by the SIP
// peer. We tear the call down with a BYE on an established dialog, so the status
// recorded must be the 200 OK the INVITE was answered with; the room close is
// reported through DisconnectReason, not by rewriting the SIP status.
func TestInboundCallStatusCodeOnRoomClosed(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})

	call, ic := st.CreateInboundCall(t)
	require.Eventually(t, ic.started.IsBroken, 5*time.Second, 10*time.Millisecond, "call should become active")

	byeSink := st.TestUA.RegisterSink(call.localTag, "BYE")
	defer st.TestUA.UnregisterSink(call.localTag, "BYE")

	// The room goes away under an otherwise healthy call.
	ic.lkRoom.(*testRoom).simulateRoomClosed(livekit.DisconnectReason_ROOM_CLOSED)

	// The SIP leg is torn down from our side, so the caller gets a BYE.
	select {
	case msg := <-byeSink:
		require.NotNil(t, msg)
		require.Equal(t, sip.BYE, msg.req.Method)
		require.NoError(t, msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 200, "OK", nil)))
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for BYE")
	}

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the ended call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_DISCONNECTED, ended.CallStatus)
	require.NotZero(t, ended.StartedAtNs, "call was answered")
	require.Equal(t, livekit.DisconnectReason_ROOM_CLOSED, ended.DisconnectReason, "the room close must be reported as the disconnect reason")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code, "sip status code must be recorded")
}

// TestInboundCallStatusCodeOnDispatchReject covers the SIP status reported for a
// call rejected by DispatchNoRuleReject: the caller is told 404, so that is the
// status the call record must carry. Recording anything else contradicts what
// the peer was actually sent.
func TestInboundCallStatusCodeOnDispatchReject(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		return CallDispatch{Result: DispatchNoRuleReject}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	res := getFinalResponseOrFail(t, ctx, tx)
	require.Equal(t, sip.StatusCode(404), res.StatusCode, "a call matching no dispatch rule should be rejected with 404")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the rejected call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// TestInboundCallStatusCodeOnDispatchUnavailable covers the SIP status reported
// for a call rejected by DispatchServiceUnavailable, i.e. dispatch evaluation
// itself failed. The caller is told 503, so that is the status the call record
// must carry: a retryable server-side failure has to stay distinguishable from
// the rejections that tell the caller not to retry.
func TestInboundCallStatusCodeOnDispatchUnavailable(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		return CallDispatch{Result: DispatchServiceUnavailable}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	res := getFinalResponseOrFail(t, ctx, tx)
	require.Equal(t, sip.StatusCode(503), res.StatusCode, "a call whose dispatch evaluation failed should be rejected with 503")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the rejected call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// TestInboundCallStatusCodeOnLateOfferDisabled covers the SIP status reported
// for an offerless INVITE received while late offer is disabled: the SDP error
// is rejected with 400 (TestInboundLateOfferDisabled covers the wire behaviour),
// so 400 is what the call record must carry. The recorded status and the
// response sent to the caller have to agree — if 488 Media Failed is the outcome
// we want on record, then 488 is what the caller should be sent.
func TestInboundCallStatusCodeOnLateOfferDisabled(t *testing.T) {
	// No feature flags: late offer is off for the project.
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	c := inviteWithoutOffer(t, st)
	res := getFinalResponseOrFail(t, ctx, c.tx)
	require.Equal(t, sip.StatusBadRequest, res.StatusCode, "offerless INVITE should be rejected when late offer is disabled")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the rejected call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// TestInboundCallStatusCodeOnMediaConfigError covers the SIP status reported for
// a call rejected because its media config could not be built, e.g. a trunk
// configured to use only listed codecs but listing none. That is our own
// misconfiguration rather than anything the caller did, so the caller is sent
// 500 and the call record must say so: a server error has to stay
// distinguishable from a rejection blamed on the caller.
func TestInboundCallStatusCodeOnMediaConfigError(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		return CallDispatch{
			Result: DispatchAccept,
			Room:   RoomConfig{RoomName: testRoomName},
			// newMediaConfig rejects this: no codecs are listed to select from.
			MediaConfig: &livekit.SIPMediaConfig{OnlyListedCodecs: true},
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	res := getFinalResponseOrFail(t, ctx, tx)
	require.Equal(t, sip.StatusInternalServerError, res.StatusCode, "a call we cannot build a media config for should be rejected with 500")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the rejected call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.Equal(t, "no codecs specified", ended.Error, "the config error must be reported")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// TestInboundCallStatusCodeOnUnexpectedDispatch covers the SIP status reported for
// a call rejected because of an unexpected dispatch result
func TestInboundCallStatusCodeOnUnexpectedDispatch(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		return CallDispatch{Result: DispatchResult(1337)}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	res := getFinalResponseOrFail(t, ctx, tx)
	require.Equal(t, sip.StatusCode(501), res.StatusCode, "unknown dispatch result should result in 501")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the rejected call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// errTestPublishFailed stands in for whatever makes publishing our track fail:
// the room handle being gone, or the LiveKit publish itself erroring.
var errTestPublishFailed = errors.New("test: cannot publish track")

// TestInboundCallStatusCodeOnPublishTrackError covers the SIP status reported
// for a call that ends because publishTrack failed. The call is still ringing at
// that point, so the rejection status is what actually goes on the wire, and the
// record must agree with it.
func TestInboundCallStatusCodeOnPublishTrackError(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{
		GetRoom:         newTestRoomConfig(&testRoomConfig{inboundAudioErr: errTestPublishFailed}),
		GetStateHandler: states.GetStateHandler(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	// The INVITE was never answered, so the call is torn down with a final
	// response rather than a BYE.
	res := getFinalResponseOrFail(t, ctx, tx)
	require.Equal(t, sip.StatusBusyHere, res.StatusCode, "a call whose track could not be published should be rejected")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the failed call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.Contains(t, ended.Error, errTestPublishFailed.Error(), "the publish failure must be reported")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.EqualValues(t, res.StatusCode, ended.CallStatusCode.Code, "the recorded status must be the one sent to the caller")
}

// TestInboundCallStatusCodeOnHangupDuringAccept covers the SIP status reported
// for a call the caller hangs up inside acceptCallAndWaitForMedia: the INVITE
// was answered with 200 OK, but the BYE arrives while we are still waiting for
// the first RTP packet, so the call never reaches ACTIVE. The answer still
// happened, so that is the status the record must carry.
func TestInboundCallStatusCodeOnHangupDuringAccept(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})

	// CreateInboundCall returns once the 200 OK is in, which leaves the call
	// parked in waitMedia for up to audioBridgeMaxDelay: no RTP is ever sent.
	call, ic := st.CreateInboundCall(t)

	// The caller hangs up inside that window.
	resp := st.TestUA.TransactionRequest(t, call.NewRequest(sip.BYE), true)
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "BYE should be accepted")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the ended call should be reported")

	// Guard the window this test is about: had the BYE landed after media was
	// bridged, the call would have gone active and this would be a plain hangup.
	require.False(t, ic.started.IsBroken(), "call must not have gone active")
	for i, u := range states.Updates() {
		require.NotEqual(t, livekit.SIPCallStatus_SCS_ACTIVE, u.CallStatus, "update %d reported the call active", i)
	}

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_DISCONNECTED, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call never reached the active state")
	require.Equal(t, livekit.DisconnectReason_CLIENT_INITIATED, ended.DisconnectReason, "the caller hung up")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code, "the INVITE was answered, so the recorded status must be the 200 OK")
}

// TestInboundCallStatusCodeOnAcceptError covers the SIP status reported for a
// call that ends because c.cc.Accept failed: the INVITE transaction is gone by
// the time we answer, which is the race AcceptBye and the other drop paths leave
// behind. Nothing reaches the caller, so the record is the only account of the
// outcome, and it must not blame the caller for our failure to answer a call we
// had decided to accept.
func TestInboundCallStatusCodeOnAcceptError(t *testing.T) {
	states := &recordingStateHandler{}
	// ringForever holds the call in waitSubscribe, so the accept happens only
	// once this test releases it.
	st := NewServiceTest(t, &serviceTestConfig{
		GetRoom:         newTestRoomConfig(&testRoomConfig{ringForever: true}),
		GetStateHandler: states.GetStateHandler(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	res100 := getResponseOrFailTimeout(t, ctx, tx)
	require.Equal(t, sip.StatusCode(100), res100.StatusCode, "should receive 100 Trying")
	res180 := getResponseOrFailTimeout(t, ctx, tx)
	require.Equal(t, sip.StatusCode(180), res180.StatusCode, "should receive 180 Ringing")
	remoteTag, ok := res180.To().Params.Get("tag")
	require.True(t, ok, "remote tag should be present")
	call.SetRemoteTag(LocalTag(remoteTag))

	st.Server.cmu.Lock()
	ic, ok := st.Server.byLocalTag[call.remoteTag]
	st.Server.cmu.Unlock()
	require.True(t, ok, "call should be registered")

	// Drop the INVITE transaction while the call is still ringing, as a BYE
	// arriving at that moment would. Accept then has nothing left to answer on.
	ic.cc.mu.Lock()
	ic.cc.drop()
	ic.cc.mu.Unlock()

	// Release the call into the accept path.
	ic.lkRoom.(*testRoom).simulateSubscribed()

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the failed call should be reported")

	// There is no transaction left to answer on, so the caller is told nothing.
	expectNoResponse(t, tx)

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call was never answered")
	require.Contains(t, ended.Error, "call already rejected", "the accept failure must be reported")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_INTERNAL_SERVER_ERROR, ended.CallStatusCode.Code, "sip status code must be recorded")
}

// TestInboundCallStatusCodeOnNoACK covers the SIP status reported for a call
// that ends because acceptCall got errNoACK: the 200 OK was sent and
// retransmitted, and the caller never ACKed it. The answer did go out, so that
// is the status the record must carry; the missing ACK is what CallStatus and
// Error report. Takes ~10s: that is how long the UDP retransmissions run before
// the server gives up.
func TestInboundCallStatusCodeOnNoACK(t *testing.T) {
	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	// Late offer makes the server wait for the ACK, which is what produces errNoACK.
	st.Server.SetHandler(&TestHandler{FeatureFlags: map[string]string{lateOfferFeatureFlag: "true"}})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	c := inviteWithoutOffer(t, st)
	c.expectOffer(t, ctx)

	// Never ACK: absorb the retransmitted 200 OKs until the server gives up and
	// tears the call down with a BYE.
loop:
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the server to give up on the ACK: %v", ctx.Err())
		case resp := <-c.tx.Responses():
			c.requireRetransmit(t, resp)
		case msg := <-c.byes:
			c.answerBye(t, msg)
			break loop
		}
	}
	require.False(t, c.ic.cc.GotACK(), "server received unexpected ACK")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the failed call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call never reached the active state")
	require.Contains(t, ended.Error, "no ACK received", "the missing ACK must be reported")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code, "sip status code must be recorded")
}

// TestInboundCallStatusCodeOnPinTooLong covers the SIP status reported for a
// call dropped because the caller entered more pin digits than pinLimit. The
// call was answered with 200 OK to play the pin prompt and is torn down with a
// BYE, so that is the status the record must carry; the bad pin is what
// CallStatus and Error report.
func TestInboundCallStatusCodeOnPinTooLong(t *testing.T) {
	// One digit past the pinLimit in pinPrompt, entered without ever sending '#'.
	const tooManyDigits = 17

	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		return CallDispatch{Result: DispatchRequestPin, Room: RoomConfig{RoomName: testRoomName}}
	}

	// The pin flow answers the call so the caller can hear the prompt.
	call, ic := st.CreateInboundCall(t)

	byeSink := st.TestUA.RegisterSink(call.localTag, "BYE")
	defer st.TestUA.UnregisterSink(call.localTag, "BYE")

	// Feed the digits the way the media port does once DTMF is connected.
	for i := 0; i < tooManyDigits; i++ {
		select {
		case ic.dtmf <- dtmf.Event{Digit: '1', Code: 1}:
		case <-time.After(5 * time.Second):
			require.Fail(t, "timed out entering the pin")
		}
	}

	// The call is established, so it is torn down with a BYE.
	select {
	case msg := <-byeSink:
		require.NotNil(t, msg)
		require.Equal(t, sip.BYE, msg.req.Method)
		require.NoError(t, msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 200, "OK", nil)))
	case <-time.After(10 * time.Second):
		require.Fail(t, "timeout waiting for BYE")
	}

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the dropped call should be reported")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call never got past the pin prompt")
	require.Contains(t, ended.Error, "wrong pin", "the rejected pin must be reported")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code, "sip status code must be recorded")
}

// TestInboundCallStatusCodeOnWrongPin covers the SIP status reported for a call
// dropped because the pin the caller entered matched no dispatch rule. As with
// any pin prompt the call was answered with 200 OK and is torn down with a BYE,
// so that is the status the record must carry; the rejected pin is what
// CallStatus and Error report.
func TestInboundCallStatusCodeOnWrongPin(t *testing.T) {
	const wrongPin = "4321"

	states := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: states.GetStateHandler()})
	var gotPin atomic.Value
	st.Handler.(*TestHandler).DispatchCallFunc = func(ctx context.Context, info *CallInfo) CallDispatch {
		if info.Pin == "" {
			// First evaluation, before any digits: ask for a pin.
			return CallDispatch{Result: DispatchRequestPin, Room: RoomConfig{RoomName: testRoomName}}
		}
		// Second evaluation, once '#' ends the pin: it matches no rule.
		gotPin.Store(info.Pin)
		return CallDispatch{Result: DispatchNoRuleReject}
	}

	// The pin flow answers the call so the caller can hear the prompt.
	call, ic := st.CreateInboundCall(t)

	byeSink := st.TestUA.RegisterSink(call.localTag, "BYE")
	defer st.TestUA.UnregisterSink(call.localTag, "BYE")

	// Enter the pin, then '#' to submit it.
	for _, digit := range wrongPin + "#" {
		select {
		case ic.dtmf <- dtmf.Event{Digit: byte(digit), Code: 1}:
		case <-time.After(5 * time.Second):
			require.Fail(t, "timed out entering the pin")
		}
	}

	// The call is established, so it is torn down with a BYE.
	select {
	case msg := <-byeSink:
		require.NotNil(t, msg)
		require.Equal(t, sip.BYE, msg.req.Method)
		require.NoError(t, msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 200, "OK", nil)))
	case <-time.After(10 * time.Second):
		require.Fail(t, "timeout waiting for BYE")
	}

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the dropped call should be reported")

	// Guard the path: the call must have been dropped on the submitted pin.
	require.Equal(t, wrongPin, gotPin.Load(), "the pin entered should have been dispatched on")

	ended := states.Last()
	require.Equal(t, livekit.SIPCallStatus_SCS_ERROR, ended.CallStatus)
	require.Zero(t, ended.StartedAtNs, "call never got past the pin prompt")
	require.Contains(t, ended.Error, "wrong pin", "the rejected pin must be reported")
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code, "sip status code must be recorded")
}

// failingRetransmitTx wraps an INVITE server transaction and fails every 2xx
// response after the first one, standing in for a transport error that hits a
// 200 OK retransmission while the server waits for the ACK. Provisional
// responses and the teardown status still go out, as they would on a socket
// that only broke for that one write.
type failingRetransmitTx struct {
	sip.ServerTransaction
	sent atomic.Bool
}

func (t *failingRetransmitTx) Respond(res *sip.Response) error {
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return t.ServerTransaction.Respond(res)
	}
	if t.sent.CompareAndSwap(false, true) {
		return t.ServerTransaction.Respond(res)
	}
	return errors.New("write udp: connection refused")
}

// TestInboundCallStatusCodeOnAnswerRetransmitError covers the SIP status
// reported for a call whose 200 OK reached the caller and then failed to be
// retransmitted before the ACK arrived. Over UDP the answer is sent repeatedly
// until the ACK comes back, so a later write failing says nothing about what
// the caller saw: it already has the 200 OK. The record must carry that answer,
// not the internal failure that ended the call afterwards.
func TestInboundCallStatusCodeOnAnswerRetransmitError(t *testing.T) {
	states := &recordingStateHandler{}
	// ringForever holds the call in waitSubscribe, so the accept happens only
	// once this test has wrapped the INVITE transaction.
	st := NewServiceTest(t, &serviceTestConfig{
		GetRoom:         newTestRoomConfig(&testRoomConfig{ringForever: true}),
		GetStateHandler: states.GetStateHandler(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	call.SetLocalSDP(localSDP)

	tx, err := st.TestUA.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	res100 := getResponseOrFailTimeout(t, ctx, tx)
	require.Equal(t, sip.StatusCode(100), res100.StatusCode, "should receive 100 Trying")
	res180 := getResponseOrFailTimeout(t, ctx, tx)
	require.Equal(t, sip.StatusCode(180), res180.StatusCode, "should receive 180 Ringing")
	remoteTag, ok := res180.To().Params.Get("tag")
	require.True(t, ok, "remote tag should be present")
	call.SetRemoteTag(LocalTag(remoteTag))

	st.Server.cmu.Lock()
	ic, ok := st.Server.byLocalTag[call.remoteTag]
	st.Server.cmu.Unlock()
	require.True(t, ok, "call should be registered")

	// Let the answer reach the caller, then break the transport under the
	// retransmission that follows while the server waits for the ACK.
	ic.cc.mu.Lock()
	ic.cc.inviteTx = &failingRetransmitTx{ServerTransaction: ic.cc.inviteTx}
	ic.cc.mu.Unlock()

	// Release the call into the accept path.
	ic.lkRoom.(*testRoom).simulateSubscribed()

	// The caller is answered, and never ACKs, so the server retransmits the
	// 200 OK and that write is the one that fails.
	res200 := getFinalResponseOrFail(t, ctx, tx)
	require.Equal(t, sip.StatusCode(200), res200.StatusCode, "caller should receive the answer")

	require.Eventually(t, func() bool {
		last := states.Last()
		return last != nil && last.EndedAtNs != 0
	}, 5*time.Second, 10*time.Millisecond, "the failed call should be reported")

	require.False(t, ic.cc.GotACK(), "server received unexpected ACK")

	ended := states.Last()
	require.NotNil(t, ended.CallStatusCode, "CallStatusCode must be set")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_OK, ended.CallStatusCode.Code,
		"the caller received the 200 OK, so that is the status the record must carry")
}
