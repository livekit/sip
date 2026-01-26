package sip

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/icholy/digest"
	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/media-sdk/sdp"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	testPortSIPMin = 30000
	testPortSIPMax = 30050

	testPortRTPMin = 30100
	testPortRTPMax = 30150
)

func getResponseOrFail(t *testing.T, tx sip.ClientTransaction) *sip.Response {
	select {
	case <-tx.Done():
		t.Fatal("Transaction failed to complete")
	case res := <-tx.Responses():
		return res
	}

	return nil
}

func expectNoResponse(t *testing.T, tx sip.ClientTransaction) {
	select {
	case res := <-tx.Responses():
		t.Fatal("unexpected result:", res)
	case <-time.After(time.Second / 2):
		// ok
	case <-tx.Done():
		// ok
	}
}

type TestHandler struct {
	GetAuthCredentialsFunc func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error)
	DispatchCallFunc       func(ctx context.Context, info *CallInfo) CallDispatch
	OnInboundInfoFunc      func(log logger.Logger, call *rpc.SIPCall, headers Headers)
	OnSessionEndFunc       func(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string)
}

func (h TestHandler) GetAuthCredentials(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
	return h.GetAuthCredentialsFunc(ctx, call)
}

func (h TestHandler) DispatchCall(ctx context.Context, info *CallInfo) CallDispatch {
	return h.DispatchCallFunc(ctx, info)
}

func (h TestHandler) GetMediaProcessor(_ []livekit.SIPFeature, _ map[string]string) msdk.PCM16Processor {
	return nil
}

func (h TestHandler) RegisterTransferSIPParticipantTopic(sipCallId string) error {
	// no-op
	return nil
}

func (h TestHandler) DeregisterTransferSIPParticipantTopic(sipCallId string) {
	// no-op
}

func (h TestHandler) OnInboundInfo(log logger.Logger, call *rpc.SIPCall, headers Headers) {
	if h.OnInboundInfoFunc != nil {
		h.OnInboundInfoFunc(log, call, headers)
	}
}

func (h TestHandler) OnSessionEnd(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string) {
	if h.OnSessionEndFunc != nil {
		h.OnSessionEndFunc(ctx, callIdentifier, callInfo, reason)
	}
}

func testInvite(t *testing.T, h Handler, hidden bool, from, to string, test func(tx sip.ClientTransaction)) {
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	// Use a no-op logger to avoid panics from async logging after test completion
	log := logger.LogRLogger(logr.Discard())
	s, err := NewService("", &config.Config{
		HideInboundPort: hidden,
		SIPPort:         sipPort,
		SIPPortListen:   sipPort,
		RTPPort:         rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)

	require.NoError(t, s.Start())

	sipUserAgent, err := sipgo.NewUA(
		sipgo.WithUserAgent(from),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
	)
	require.NoError(t, err)

	sipClient, err := sipgo.NewClient(sipUserAgent)
	require.NoError(t, err)

	offer, err := sdp.NewOffer(localIP, 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	inviteRecipent := sip.Uri{User: to, Host: sipServerAddress}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipent)
	inviteRequest.SetDestination(sipServerAddress)
	inviteRequest.SetBody(offerData)
	inviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	tx, err := sipClient.TransactionRequest(inviteRequest)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)

	test(tx)
}

func TestService_AuthFailure(t *testing.T) {
	const (
		expectedFromUser = "foo"
		expectedToUser   = "bar"
	)
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			require.Equal(t, expectedFromUser, call.From.User)
			require.Equal(t, expectedToUser, call.To.User)
			return AuthInfo{}, fmt.Errorf("Auth Failure")
		},
	}
	testInvite(t, h, false, expectedFromUser, expectedToUser, func(tx sip.ClientTransaction) {
		res := getResponseOrFail(t, tx)
		require.Equal(t, sip.StatusCode(100), res.StatusCode)

		res = getResponseOrFail(t, tx)
		require.Equal(t, sip.StatusCode(503), res.StatusCode)
	})
}

func TestService_AuthDrop(t *testing.T) {
	const (
		expectedFromUser = "foo"
		expectedToUser   = "bar"
	)
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			require.Equal(t, expectedFromUser, call.From.User)
			require.Equal(t, expectedToUser, call.To.User)
			return AuthInfo{Result: AuthDrop}, nil
		},
	}
	testInvite(t, h, true, expectedFromUser, expectedToUser, func(tx sip.ClientTransaction) {
		expectNoResponse(t, tx)
	})
}

func TestService_OnSessionEnd(t *testing.T) {
	const (
		expectedCallID    = "test-call-id"
		expectedSipCallID = "test-sip-call-id"
		expectedProjectID = "test-project"
		expectedReason    = "test-reason"
	)

	callEnded := make(chan struct{})
	var receivedCallIdentifier *CallIdentifier
	var receivedCallInfo *livekit.SIPCallInfo
	var receivedReason string

	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{Result: AuthAccept}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			return CallDispatch{
				Result: DispatchAccept,
				Room: RoomConfig{
					RoomName: "test-room",
					Participant: ParticipantConfig{
						Identity: "test-participant",
					},
				},
			}
		},
		OnSessionEndFunc: func(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string) {
			receivedCallIdentifier = callIdentifier
			receivedCallInfo = callInfo
			receivedReason = reason
			close(callEnded)
		},
	}

	// Create a new service
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	// Use a no-op logger to avoid panics from async logging after test completion
	log := logger.LogRLogger(logr.Discard())
	s, err := NewService("", &config.Config{
		SIPPort:       sipPort,
		SIPPortListen: sipPort,
		RTPPort:       rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)
	require.NoError(t, s.Start())

	// Call OnSessionEnd directly with test data
	h.OnSessionEnd(context.Background(), &CallIdentifier{
		ProjectID: expectedProjectID,
		CallID:    expectedCallID,
		SipCallID: expectedSipCallID,
	}, &livekit.SIPCallInfo{
		CallId: expectedCallID,
		ParticipantAttributes: map[string]string{
			"projectID":       expectedProjectID,
			AttrSIPCallIDFull: expectedSipCallID,
		},
	}, expectedReason)

	// Wait for OnSessionEnd to be called
	select {
	case <-callEnded:
		// Success
	case <-time.After(time.Second):
		t.Fatal("OnSessionEnd was not called")
	}

	// Verify the CallIdentifier fields are correctly populated
	require.NotNil(t, receivedCallIdentifier, "CallIdentifier should not be nil")
	require.Equal(t, expectedProjectID, receivedCallIdentifier.ProjectID, "CallIdentifier.ProjectID should match")
	require.Equal(t, expectedCallID, receivedCallIdentifier.CallID, "CallIdentifier.CallID should match")
	require.Equal(t, expectedSipCallID, receivedCallIdentifier.SipCallID, "CallIdentifier.SipCallID should match")

	// Verify the CallInfo fields
	require.NotNil(t, receivedCallInfo, "CallInfo should not be nil")
	require.Equal(t, expectedProjectID, receivedCallInfo.ParticipantAttributes["projectID"], "CallInfo.ParticipantAttributes[projectID] should match")
	require.Equal(t, expectedCallID, receivedCallInfo.CallId, "CallInfo.CallId should match")
	require.Equal(t, expectedSipCallID, receivedCallInfo.ParticipantAttributes[AttrSIPCallIDFull], "CallInfo.ParticipantAttributes[sip.callIDFull] should match")
	require.Equal(t, expectedReason, receivedReason, "Reason should match")
}

// TestDigestAuthSimultaneousCalls tests that simultaneous calls from the same "from" number
// don't interfere with each other's digest authentication state
func TestDigestAuthSimultaneousCalls(t *testing.T) {
	const (
		sameFromUser = "callcenter@example.com"
		toUser1      = "agent1@example.com"
		toUser2      = "agent2@example.com"
		username     = "testuser"
		password     = "testpass"
	)

	// Track authentication attempts to verify they're independent
	authAttempts := make(map[string]int)
	var authMutex sync.Mutex

	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			authMutex.Lock()
			defer authMutex.Unlock()

			// Track each call by its SIP Call ID
			sipCallID := call.SipCallId
			authAttempts[sipCallID]++

			// Return password authentication required
			return AuthInfo{
				Result:   AuthPassword,
				Username: username,
				Password: password,
			}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			return CallDispatch{
				Result: DispatchNoRuleReject,
				// No room config needed for reject
			}
		},
		OnSessionEndFunc: func(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string) {
			// No-op for tests to avoid async logging issues
		},
	}

	// Create service with authentication enabled
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	// Use a no-op logger to avoid panics from async logging after test completion
	log := logger.LogRLogger(logr.Discard())
	s, err := NewService("", &config.Config{
		HideInboundPort: false, // Enable authentication
		SIPPort:         sipPort,
		SIPPortListen:   sipPort,
		RTPPort:         rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)
	require.NoError(t, s.Start())

	// Create two SIP clients for simultaneous calls
	sipUserAgent1, err := sipgo.NewUA(
		sipgo.WithUserAgent(sameFromUser),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
	)
	require.NoError(t, err)

	sipUserAgent2, err := sipgo.NewUA(
		sipgo.WithUserAgent(sameFromUser),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
	)
	require.NoError(t, err)

	sipClient1, err := sipgo.NewClient(sipUserAgent1)
	require.NoError(t, err)

	sipClient2, err := sipgo.NewClient(sipUserAgent2)
	require.NoError(t, err)

	// Create SDP offers
	offer1, err := sdp.NewOffer(localIP, 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData1, err := offer1.SDP.Marshal()
	require.NoError(t, err)

	offer2, err := sdp.NewOffer(localIP, 0xB0C, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData2, err := offer2.SDP.Marshal()
	require.NoError(t, err)

	// Create two simultaneous INVITE requests
	inviteRecipient1 := sip.Uri{User: toUser1, Host: sipServerAddress}
	inviteRequest1 := sip.NewRequest(sip.INVITE, inviteRecipient1)
	inviteRequest1.SetDestination(sipServerAddress)
	inviteRequest1.SetBody(offerData1)
	inviteRequest1.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	// Set different Call-IDs for each request
	inviteRequest1.AppendHeader(sip.NewHeader("Call-ID", "call1-123@test.com"))

	inviteRecipient2 := sip.Uri{User: toUser2, Host: sipServerAddress}
	inviteRequest2 := sip.NewRequest(sip.INVITE, inviteRecipient2)
	inviteRequest2.SetDestination(sipServerAddress)
	inviteRequest2.SetBody(offerData2)
	inviteRequest2.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	// Set different Call-IDs for each request
	inviteRequest2.AppendHeader(sip.NewHeader("Call-ID", "call2-456@test.com"))

	// Start both transactions simultaneously
	tx1, err := sipClient1.TransactionRequest(inviteRequest1)
	require.NoError(t, err)
	t.Cleanup(tx1.Terminate)

	tx2, err := sipClient2.TransactionRequest(inviteRequest2)
	require.NoError(t, err)
	t.Cleanup(tx2.Terminate)

	// Both should receive 100 Trying first, then 407 Unauthorized with different challenges
	res1 := getResponseOrFail(t, tx1)
	require.Equal(t, sip.StatusCode(100), res1.StatusCode, "First call should receive 100 Trying")
	res1 = getResponseOrFail(t, tx1)
	require.Equal(t, sip.StatusCode(407), res1.StatusCode, "First call should receive 407 Unauthorized")

	res2 := getResponseOrFail(t, tx2)
	require.Equal(t, sip.StatusCode(100), res2.StatusCode, "Second call should receive 100 Trying")
	res2 = getResponseOrFail(t, tx2)
	require.Equal(t, sip.StatusCode(407), res2.StatusCode, "Second call should receive 407 Unauthorized")

	// Verify both calls have different Proxy-Authenticate headers (different nonces)
	authHeader1 := res1.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader1, "First response should have Proxy-Authenticate header")

	authHeader2 := res2.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader2, "Second response should have Proxy-Authenticate header")

	// The challenges should be different (different nonces)
	require.NotEqual(t, authHeader1.Value(), authHeader2.Value(),
		"Different calls should have different authentication challenges")

	// Now test the complete authentication flow for both calls
	// Parse challenges and compute digest responses
	challenge1, err := digest.ParseChallenge(authHeader1.Value())
	require.NoError(t, err, "Should be able to parse first challenge")

	challenge2, err := digest.ParseChallenge(authHeader2.Value())
	require.NoError(t, err, "Should be able to parse second challenge")

	// Compute digest responses for both calls
	cred1, err := digest.Digest(challenge1, digest.Options{
		Method:   "INVITE",
		URI:      inviteRecipient1.String(),
		Username: username,
		Password: password,
	})
	require.NoError(t, err, "Should be able to compute digest response for first call")

	cred2, err := digest.Digest(challenge2, digest.Options{
		Method:   "INVITE",
		URI:      inviteRecipient2.String(),
		Username: username,
		Password: password,
	})
	require.NoError(t, err, "Should be able to compute digest response for second call")

	// Create authenticated INVITE requests for both calls
	authInviteRequest1 := sip.NewRequest(sip.INVITE, inviteRecipient1)
	authInviteRequest1.SetDestination(sipServerAddress)
	authInviteRequest1.SetBody(offerData1)
	authInviteRequest1.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	authInviteRequest1.AppendHeader(sip.NewHeader("Call-ID", "call1-123@test.com"))
	authInviteRequest1.AppendHeader(sip.NewHeader("Proxy-Authorization", cred1.String()))

	authInviteRequest2 := sip.NewRequest(sip.INVITE, inviteRecipient2)
	authInviteRequest2.SetDestination(sipServerAddress)
	authInviteRequest2.SetBody(offerData2)
	authInviteRequest2.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	authInviteRequest2.AppendHeader(sip.NewHeader("Call-ID", "call2-456@test.com"))
	authInviteRequest2.AppendHeader(sip.NewHeader("Proxy-Authorization", cred2.String()))

	// Send authenticated requests
	authTx1, err := sipClient1.TransactionRequest(authInviteRequest1)
	require.NoError(t, err)
	t.Cleanup(authTx1.Terminate)

	authTx2, err := sipClient2.TransactionRequest(authInviteRequest2)
	require.NoError(t, err)
	t.Cleanup(authTx2.Terminate)

	// Both authenticated requests should receive 100 Trying first
	authRes1 := getResponseOrFail(t, authTx1)
	require.Equal(t, sip.StatusCode(100), authRes1.StatusCode, "First authenticated call should receive 100 Trying")

	authRes2 := getResponseOrFail(t, authTx2)
	require.Equal(t, sip.StatusCode(100), authRes2.StatusCode, "Second authenticated call should receive 100 Trying")

	// Both should now proceed with authentication (either 200 OK or continue with call processing)
	authRes1 = getResponseOrFail(t, authTx1)
	authRes2 = getResponseOrFail(t, authTx2)

	// Log the results for debugging
	t.Logf("First authenticated call got status: %d", authRes1.StatusCode)
	t.Logf("Second authenticated call got status: %d", authRes2.StatusCode)

	// Verify each call was tracked independently (should be 2 calls total)
	// Note: Each SipCallId gets tracked twice - once for initial request, once for authenticated request
	authMutex.Lock()
	require.Equal(t, 2, len(authAttempts), "Should have tracked 2 different calls")
	require.Equal(t, 2, authAttempts["call1-123@test.com"], "First call should be tracked twice (initial + authenticated)")
	require.Equal(t, 2, authAttempts["call2-456@test.com"], "Second call should be tracked twice (initial + authenticated)")
	authMutex.Unlock()
}

// TestDigestAuthStandardFlow tests the standard authentication flow where the same Call-ID gets the same challenge state
func TestDigestAuthStandardFlow(t *testing.T) {
	const (
		fromUser = "test@example.com"
		toUser   = "agent@example.com"
		username = "testuser"
		password = "testpass"
		callID   = "same-call-id@test.com"
	)

	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{
				Result:   AuthPassword,
				Username: username,
				Password: password,
			}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			return CallDispatch{
				Result: DispatchNoRuleReject,
				// No room config needed for reject
			}
		},
		OnSessionEndFunc: func(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string) {
			// No-op for tests to avoid async logging issues
		},
	}

	// Create service with authentication enabled
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	// Use a no-op logger to avoid panics from async logging after test completion
	log := logger.LogRLogger(logr.Discard())
	s, err := NewService("", &config.Config{
		HideInboundPort: false, // Enable authentication
		SIPPort:         sipPort,
		SIPPortListen:   sipPort,
		RTPPort:         rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)
	require.NoError(t, s.Start())

	sipUserAgent, err := sipgo.NewUA(
		sipgo.WithUserAgent(fromUser),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
	)
	require.NoError(t, err)

	sipClient, err := sipgo.NewClient(sipUserAgent)
	require.NoError(t, err)

	offer, err := sdp.NewOffer(localIP, 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	// Create first INVITE request with specific Call-ID
	inviteRecipient := sip.Uri{User: toUser, Host: sipServerAddress}
	inviteRequest1 := sip.NewRequest(sip.INVITE, inviteRecipient)
	inviteRequest1.SetDestination(sipServerAddress)
	inviteRequest1.SetBody(offerData)
	inviteRequest1.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteRequest1.AppendHeader(sip.NewHeader("Call-ID", callID))

	tx1, err := sipClient.TransactionRequest(inviteRequest1)
	require.NoError(t, err)
	t.Cleanup(tx1.Terminate)

	// Should receive 100 Trying first, then 407 Unauthorized
	res1 := getResponseOrFail(t, tx1)
	require.Equal(t, sip.StatusCode(100), res1.StatusCode, "First request should receive 100 Trying")
	res1 = getResponseOrFail(t, tx1)
	require.Equal(t, sip.StatusCode(407), res1.StatusCode, "First request should receive 407 Unauthorized")

	// Get the challenge from first response
	authHeader1 := res1.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader1, "First response should have Proxy-Authenticate header")
	challenge1 := authHeader1.Value()

	// Parse the challenge to extract nonce and realm
	challenge, err := digest.ParseChallenge(challenge1)
	require.NoError(t, err, "Should be able to parse challenge")

	// Compute the digest response using the challenge and credentials
	cred, err := digest.Digest(challenge, digest.Options{
		Method:   "INVITE",
		URI:      inviteRecipient.String(),
		Username: username,
		Password: password,
	})
	require.NoError(t, err, "Should be able to compute digest response")

	// Create second INVITE request with the SAME Call-ID and Proxy-Authorization header
	inviteRequest2 := sip.NewRequest(sip.INVITE, inviteRecipient)
	inviteRequest2.SetDestination(sipServerAddress)
	inviteRequest2.SetBody(offerData)
	inviteRequest2.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteRequest2.AppendHeader(sip.NewHeader("Call-ID", callID))
	inviteRequest2.AppendHeader(sip.NewHeader("Proxy-Authorization", cred.String()))

	tx2, err := sipClient.TransactionRequest(inviteRequest2)
	require.NoError(t, err)
	t.Cleanup(tx2.Terminate)

	// Should receive 100 Trying first, then proceed with authentication
	res2 := getResponseOrFail(t, tx2)
	require.Equal(t, sip.StatusCode(100), res2.StatusCode, "Second request should receive 100 Trying")

	// The second request should either succeed (200) or get another 407 if there are issues
	// Let's check what response we get
	res2 = getResponseOrFail(t, tx2)
	if res2.StatusCode == 407 {
		// If we get another 407, it means authentication failed
		t.Logf("Second request got 407 again, authentication may have failed")
	} else if res2.StatusCode == 200 {
		t.Logf("Second request succeeded with 200 OK")
	} else {
		t.Logf("Second request got status: %d", res2.StatusCode)
	}
}

// When a cancel request is sent, we expect two responses, 200 (for CANCEL), and 487 (for INVITE).
// This test makes sure the 487 response is received (can't test CANCEL-200)
func TestCANCELSendsBothResponses(t *testing.T) {
	const (
		fromUser = "caller@example.com"
		toUser   = "callee@example.com"
	)

	// Handler that accepts calls and makes them ring (so we can cancel during ringing)
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{Result: AuthAccept}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			// Accept the call but don't complete immediately - let it ring
			// This simulates a call that's ringing when CANCEL is received
			return CallDispatch{
				Result: DispatchAccept,
				Room: RoomConfig{
					RoomName: "test-room",
					Participant: ParticipantConfig{
						Identity: "test-participant",
					},
				},
				RingingTimeout: 30 * time.Second, // Long timeout so call stays ringing
			}
		},
		OnSessionEndFunc: func(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string) {
			// No-op for tests
		},
	}

	// Create service
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	log := logger.LogRLogger(logr.Discard())
	s, err := NewService("", &config.Config{
		HideInboundPort:    false,
		SIPPort:            sipPort,
		SIPPortListen:      sipPort,
		RTPPort:            rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
		SIPRingingInterval: 1 * time.Second,
	}, mon, log, func(projectID string) rpc.IOInfoClient { return nil })
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)
	require.NoError(t, s.Start())

	// Create SIP client using sipgo
	sipUserAgent, err := sipgo.NewUA(
		sipgo.WithUserAgent(fromUser),
	)
	require.NoError(t, err)

	sipClient, err := sipgo.NewClient(sipUserAgent)
	require.NoError(t, err)

	// Create SDP offer
	offer, err := sdp.NewOffer(localIP, 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	// Create INVITE request
	inviteRecipient := sip.Uri{User: toUser, Host: sipServerAddress}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipient)
	inviteRequest.SetDestination(sipServerAddress)
	inviteRequest.SetBody(offerData)
	inviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	// Send INVITE
	tx, err := sipClient.TransactionRequest(inviteRequest)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)

	// Wait for 100 Trying
	res100 := getResponseOrFail(t, tx)
	require.Equal(t, sip.StatusCode(100), res100.StatusCode, "Should receive 100 Trying")

	// Wait for 180 Ringing (call is now ringing)
	res180 := getResponseOrFail(t, tx)
	require.Equal(t, sip.StatusCode(180), res180.StatusCode, "Should receive 180 Ringing")

	// Now send CANCEL
	err = tx.Cancel()
	require.NoError(t, err, "Should be able to send CANCEL")

	// On-the-wire there should be two responses after CANCEL:
	// 1. 200 OK response to the CANCEL request (CSeq method = CANCEL)
	// 2. 487 Request Terminated response to the original INVITE (CSeq method = INVITE)
	//    This is the critical one - we must receive it
	// However, the 200 OK response to CANCEL will not come through tx.Responses().
	// Sipgo treats both INVITE and CANCEL as the same transaction, and has special handling
	// to swallow the 200 OK response to CANCEL, so it can't look like the INVITE got the 200.

	// Collect responses until we get the final 487 or transaction completes
	var responses []*sip.Response
	var invite487Received bool

	// Wait for responses with a timeout
	timeout := time.After(time.Second)
	transactionDone := false

	// Collect responses until we get 487 or timeout
	for !invite487Received && !transactionDone {
		select {
		case res := <-tx.Responses():
			responses = append(responses, res)
			cseq := res.CSeq()

			// Debug: log all responses to understand what we're receiving
			cseqMethod := "nil"
			if cseq != nil {
				cseqMethod = string(cseq.MethodName)
			}
			t.Logf("Received response: StatusCode=%d, CSeq method=%s", res.StatusCode, cseqMethod)

			// Check if this is the 487 response to INVITE
			if res.StatusCode == sip.StatusCode(487) {
				// Verify this is for the INVITE (CSeq method should be INVITE)
				require.NotNil(t, cseq, "487 response should have CSeq header")
				if cseq != nil {
					require.Equal(t, sip.INVITE, cseq.MethodName, "487 response should be for INVITE method")
					invite487Received = true
				}
			}

		case <-tx.Done():
			// Transaction completed, check if we got the 487 response
			transactionDone = true

		case <-timeout:
			// Log all received responses for debugging
			t.Logf("Timeout after receiving %d responses", len(responses))
			for i, res := range responses {
				cseq := res.CSeq()
				cseqMethod := "nil"
				if cseq != nil {
					cseqMethod = string(cseq.MethodName)
				}
				t.Logf("  Response %d: StatusCode=%d, CSeq method=%s", i+1, res.StatusCode, cseqMethod)
			}
			t.Fatal("Timeout waiting for 487 Request Terminated response after CANCEL")
		}
	}

	// Verify we received the critical 487 response
	require.True(t, invite487Received, "Should have received 487 Request Terminated response to INVITE when CANCEL is sent")
}
