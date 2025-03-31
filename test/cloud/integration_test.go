package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
)

func TestSIP(t *testing.T) {
	logger.InitFromConfig(&logger.Config{
		JSON:  false,
		Level: "debug",
	}, "sip")

	conf, err := NewIntegrationConfig()
	require.NoError(t, err)

	if conf.ApiKey == "" || conf.ApiSecret == "" || conf.WsUrl == "" {
		t.Skip("missing env vars")
	}

	bus := psrpc.NewLocalMessageBus()
	svc, err := NewService(conf, bus)
	require.NoError(t, err)
	defer svc.Stop(true)

	go func() {
		_ = svc.Run()
	}()

	a, err := NewPhoneClient(false)
	require.NoError(t, err)
	defer a.Close()

	b, err := NewPhoneClient(true)
	require.NoError(t, err)
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.SendSilence(ctx)

	go a.SendAudio("audio.mkv")

	time.Sleep(time.Second * 5)
	_ = a.SendDTMF("2345")
	time.Sleep(time.Second * 5)
}
