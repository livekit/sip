package sip

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/config"
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
	GetAuthCredentialsFunc func(ctx context.Context, fromUser, toUser, toHost, srcAddress string) (username, password string, drop bool, err error)
	DispatchCallFunc       func(ctx context.Context, info *CallInfo) CallDispatch
}

func (h TestHandler) GetAuthCredentials(ctx context.Context, fromUser, toUser, toHost, srcAddress string) (username, password string, drop bool, err error) {
	return h.GetAuthCredentialsFunc(ctx, fromUser, toUser, toHost, srcAddress)
}

func (h TestHandler) DispatchCall(ctx context.Context, info *CallInfo) CallDispatch {
	return h.DispatchCall(ctx, info)
}

func testInvite(t *testing.T, h Handler, from, to string, test func(tx sip.ClientTransaction)) {
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	sipServerAddress := fmt.Sprintf("%s:%d", localIP, sipPort)

	s, err := NewService(&config.Config{
		SIPPort: sipPort,
		RTPPort: rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	})
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(s.Stop)

	s.SetHandler(h)

	require.NoError(t, s.Start())

	sipUserAgent, err := sipgo.NewUA(sipgo.WithUserAgent(from))
	require.NoError(t, err)

	sipClient, err := sipgo.NewClient(sipUserAgent)
	require.NoError(t, err)

	offer, err := sdpGenerateOffer(localIP, 0xB0B)
	require.NoError(t, err)

	inviteRecipent := &sip.Uri{User: to, Host: sipServerAddress}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipent)
	inviteRequest.SetDestination(sipServerAddress)
	inviteRequest.SetBody(offer)
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
		GetAuthCredentialsFunc: func(ctx context.Context, fromUser, toUser, toHost, srcAddress string) (username, password string, drop bool, err error) {
			require.Equal(t, expectedFromUser, fromUser)
			require.Equal(t, expectedToUser, toUser)
			return "", "", false, fmt.Errorf("Auth Failure")
		},
	}
	testInvite(t, h, expectedFromUser, expectedToUser, func(tx sip.ClientTransaction) {
		if !inboundHidePort {
			res := getResponseOrFail(t, tx)
			require.Equal(t, sip.StatusCode(180), res.StatusCode)
		}

		res := getResponseOrFail(t, tx)
		require.Equal(t, sip.StatusCode(400), res.StatusCode)
	})
}

func TestService_AuthDrop(t *testing.T) {
	if !inboundHidePort {
		t.Skip()
	}
	const (
		expectedFromUser = "foo"
		expectedToUser   = "bar"
	)
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, fromUser, toUser, toHost, srcAddress string) (username, password string, drop bool, err error) {
			require.Equal(t, expectedFromUser, fromUser)
			require.Equal(t, expectedToUser, toUser)
			return "", "", true, nil
		},
	}
	testInvite(t, h, expectedFromUser, expectedToUser, func(tx sip.ClientTransaction) {
		expectNoResponse(t, tx)
	})
}
