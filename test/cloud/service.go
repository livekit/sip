package cloud

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
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

	sipsrv, err := sip.NewService("", conf.Config, mon, log, func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) sip.StateHandler {
		return sip.NewRPCStateHandler(psrpcClient)
	})
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
