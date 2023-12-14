package sip

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/sip/pkg/config"
	"github.com/stretchr/testify/require"
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

func TestService_AuthFailure(t *testing.T) {
	const (
		expectedFromUser = "foo"
		expectedToUser   = "bar"
	)

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

	s.SetAuthHandler(func(fromUser, toUser, _, _ string) (string, string, error) {
		require.Equal(t, expectedFromUser, fromUser)
		require.Equal(t, expectedToUser, toUser)
		return "", "", fmt.Errorf("Auth Failure")
	})

	require.NoError(t, s.Start())

	sipUserAgent, err := sipgo.NewUA(sipgo.WithUserAgent("foo"))
	require.NoError(t, err)

	sipClient, err := sipgo.NewClient(sipUserAgent)
	require.NoError(t, err)

	offer, err := sdpGenerateOffer(localIP, 0xB0B)
	require.NoError(t, err)

	inviteRecipent := &sip.Uri{User: "bar", Host: sipServerAddress}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipent)
	inviteRequest.SetDestination(sipServerAddress)
	inviteRequest.SetBody(offer)
	inviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	tx, err := sipClient.TransactionRequest(inviteRequest)
	require.NoError(t, err)

	res := getResponseOrFail(t, tx)
	require.Equal(t, sip.StatusCode(180), res.StatusCode)

	res = getResponseOrFail(t, tx)
	require.Equal(t, sip.StatusCode(400), res.StatusCode)

	defer tx.Terminate()

	s.Stop()
}
