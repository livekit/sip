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
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
func (st *serviceTest) inviteWithoutOffer(t *testing.T) *lateOfferCall {
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

	c := st.inviteWithoutOffer(t)
	resp := getFinalResponseOrFail(t, ctx, c.tx)
	// Same rejection as before late offer support: negotiating an empty offer fails.
	require.Equal(t, sip.StatusInternalServerError, resp.StatusCode, "offerless INVITE should be rejected when late offer is disabled")
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

		c := st.inviteWithoutOffer(t)
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

		c := st.inviteWithoutOffer(t)
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

		c := st.inviteWithoutOffer(t)
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

		c := st.inviteWithoutOffer(t)
		c.expectOffer(t, ctx)
		c.ack(t, nil)

		c.expectBye(t, ctx)
		c.expectClosedWithoutMedia(t)
	})

	t.Run("ack_with_invalid_answer", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := st.inviteWithoutOffer(t)
		c.expectOffer(t, ctx)
		c.ack(t, []byte("invalid SDP answer"))

		c.expectBye(t, ctx)
		c.expectClosedWithoutMedia(t)
	})

	t.Run("reinvite_before_ack", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		c := st.inviteWithoutOffer(t)
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
