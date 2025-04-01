package cloud

import (
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/pkg/stats"
)

func NewService(conf *IntegrationConfig, bus psrpc.MessageBus) (*service.Service, error) {
	psrpcClient := NewIOTestClient(conf)

	mon, err := stats.NewMonitor(conf.Config)
	if err != nil {
		return nil, err
	}

	sipsrv, err := sip.NewService("", conf.Config, mon, logger.GetLogger(), func(projectID string) rpc.IOInfoClient { return psrpcClient })
	if err != nil {
		return nil, err
	}
	svc := service.NewService(conf.Config, logger.GetLogger(), sipsrv, sipsrv.Stop, sipsrv.ActiveCalls, psrpcClient, bus, mon)
	sipsrv.SetHandler(svc)

	if err = sipsrv.Start(); err != nil {
		return nil, err
	}

	return svc, nil
}
