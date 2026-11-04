package cloud

import (
	"crypto/rand"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/sealer"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/pkg/stats"
)

func NewService(t testing.TB, conf *IntegrationConfig, bus psrpc.MessageBus) (*service.Service, error) {
	psrpcClient := NewIOTestClient(conf)
	log := logger.NewTestLogger(t)

	mon, err := stats.NewMonitor(conf.Config)
	if err != nil {
		return nil, err
	}

	nonces, err := sealer.NewAESGCM([]string{rand.Text()})
	if err != nil {
		return nil, err
	}
	manager := sip.NewCallManager("", conf.Config, log, mon, func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) sip.StateHandler {
	sipsrv, err := sip.NewService("", conf.Config, mon, log, nonces, manager)
	if err != nil {
		return nil, err
	}
	svc := service.NewService(conf.Config, log, sipsrv, sipsrv.Stop, sipsrv.ActiveCalls, psrpcClient, bus, mon)
	sipsrv.SetHandler(svc)

	if err = sipsrv.Start(); err != nil {
		return nil, err
	}

	return svc, nil
}
