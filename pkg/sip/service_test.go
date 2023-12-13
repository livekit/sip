package sip

import (
	"fmt"
	"math/rand"
	"testing"

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

// Test service E2E that
func TestService(t *testing.T) {
	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin

	s, err := NewService(&config.Config{
		SIPPort: sipPort,
		RTPPort: rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	})
	require.NoError(t, err)
	require.NotNil(t, s)

	s.SetAuthHandler(func(_, _, _, _ string) (string, string, error) {
		return "", "", fmt.Errorf("Auth Failure")
	})

	require.NoError(t, s.Start())

	s.Stop()
}
