package sip

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/media-sdk/sdp"

	"sync"

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
	OnSessionEndFunc       func(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string)
}

func (h TestHandler) GetAuthCredentials(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
	return h.GetAuthCredentialsFunc(ctx, call)
}

func (h TestHandler) DispatchCall(ctx context.Context, info *CallInfo) CallDispatch {
	return h.DispatchCallFunc(ctx, info)
}

func (h TestHandler) GetMediaProcessor(_ []livekit.SIPFeature) msdk.PCM16Processor {
	return nil
}

func (h TestHandler) RegisterTransferSIPParticipantTopic(sipCallId string) error {
	// no-op
	return nil
}

func (h TestHandler) DeregisterTransferSIPParticipantTopic(sipCallId string) {
	// no-op
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

	log := logger.NewTestLogger(t)
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

	log := logger.NewTestLogger(t)
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
	}

	// Create service with authentication enabled
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	log := logger.NewTestLogger(t)
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

	// Both should receive 407 Unauthorized with different challenges
	res1 := getResponseOrFail(t, tx1)
	require.Equal(t, sip.StatusCode(407), res1.StatusCode, "First call should receive 407 Unauthorized")

	res2 := getResponseOrFail(t, tx2)
	require.Equal(t, sip.StatusCode(407), res2.StatusCode, "Second call should receive 407 Unauthorized")

	// Verify both calls have different Proxy-Authenticate headers (different nonces)
	authHeader1 := res1.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader1, "First response should have Proxy-Authenticate header")

	authHeader2 := res2.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader2, "Second response should have Proxy-Authenticate header")

	// The challenges should be different (different nonces)
	require.NotEqual(t, authHeader1.Value(), authHeader2.Value(),
		"Different calls should have different authentication challenges")

	// Verify each call was tracked independently
	authMutex.Lock()
	require.Equal(t, 2, len(authAttempts), "Should have tracked 2 different calls")
	require.Equal(t, 1, authAttempts["call1-123@test.com"], "First call should be tracked once")
	require.Equal(t, 1, authAttempts["call2-456@test.com"], "Second call should be tracked once")
	authMutex.Unlock()
}

// TestDigestAuthMissingCallID tests that requests without Call-ID header are properly rejected
func TestDigestAuthMissingCallID(t *testing.T) {
	const (
		fromUser = "test@example.com"
		toUser   = "agent@example.com"
		username = "testuser"
		password = "testpass"
	)

	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{
				Result:   AuthPassword,
				Username: username,
				Password: password,
			}, nil
		},
	}

	// Create service with authentication enabled
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	log := logger.NewTestLogger(t)
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

	// Create INVITE request WITHOUT Call-ID header
	inviteRecipient := sip.Uri{User: toUser, Host: sipServerAddress}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipient)
	inviteRequest.SetDestination(sipServerAddress)
	inviteRequest.SetBody(offerData)
	inviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	// Deliberately NOT adding Call-ID header

	tx, err := sipClient.TransactionRequest(inviteRequest)
	require.NoError(t, err)
	t.Cleanup(tx.Terminate)

	// Should receive 400 Bad Request due to missing Call-ID
	res := getResponseOrFail(t, tx)
	require.Equal(t, sip.StatusCode(400), res.StatusCode, "Should receive 400 Bad Request for missing Call-ID")
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
	}

	// Create service with authentication enabled
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)

	log := logger.NewTestLogger(t)
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

	// Should receive 407 Unauthorized
	res1 := getResponseOrFail(t, tx1)
	require.Equal(t, sip.StatusCode(407), res1.StatusCode, "First request should receive 407 Unauthorized")

	// Get the challenge from first response
	authHeader1 := res1.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader1, "First response should have Proxy-Authenticate header")
	challenge1 := authHeader1.Value()

	// Create second INVITE request with the SAME Call-ID
	inviteRequest2 := sip.NewRequest(sip.INVITE, inviteRecipient)
	inviteRequest2.SetDestination(sipServerAddress)
	inviteRequest2.SetBody(offerData)
	inviteRequest2.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteRequest2.AppendHeader(sip.NewHeader("Call-ID", callID))

	tx2, err := sipClient.TransactionRequest(inviteRequest2)
	require.NoError(t, err)
	t.Cleanup(tx2.Terminate)

	// Should receive 407 Unauthorized with the SAME challenge
	res2 := getResponseOrFail(t, tx2)
	require.Equal(t, sip.StatusCode(407), res2.StatusCode, "Second request should receive 407 Unauthorized")

	authHeader2 := res2.GetHeader("Proxy-Authenticate")
	require.NotNil(t, authHeader2, "Second response should have Proxy-Authenticate header")
	challenge2 := authHeader2.Value()

	// The challenges should be the same for the same Call-ID
	require.Equal(t, challenge1, challenge2, "Same Call-ID should get the same authentication challenge")
}
