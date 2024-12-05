// Copyright 2023 LiveKit, Inc.
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

package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/media"

	"github.com/livekit/sip/pkg/stats"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/version"
)

type sipServiceStopFunc func()
type sipServiceActiveCallsFunc func() int

type Service struct {
	conf *config.Config
	log  logger.Logger

	psrpcServer rpc.SIPInternalServerImpl
	psrpcClient rpc.IOInfoClient
	bus         psrpc.MessageBus

	promServer   *http.Server
	pprofServer  *http.Server
	rpcSIPServer rpc.SIPInternalServer

	sipServiceStop        sipServiceStopFunc
	sipServiceActiveCalls sipServiceActiveCallsFunc

	mon      *stats.Monitor
	shutdown core.Fuse
	killed   atomic.Bool
}

func NewService(
	conf *config.Config, log logger.Logger, srv rpc.SIPInternalServerImpl, sipServiceStop sipServiceStopFunc,
	sipServiceActiveCalls sipServiceActiveCallsFunc, cli rpc.IOInfoClient, bus psrpc.MessageBus, mon *stats.Monitor,
) *Service {
	s := &Service{
		conf: conf,
		log:  log,

		psrpcServer: srv,
		psrpcClient: cli,
		bus:         bus,

		sipServiceStop:        sipServiceStop,
		sipServiceActiveCalls: sipServiceActiveCalls,

		mon: mon,
	}
	if conf.PrometheusPort > 0 {
		s.promServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", conf.PrometheusPort),
			Handler: promhttp.Handler(),
		}
	}
	if conf.PProfPort > 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		s.pprofServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", conf.PProfPort),
			Handler: mux,
		}
	}
	return s
}

func (s *Service) Stop(kill bool) {
	s.mon.Shutdown()
	s.shutdown.Break()
	s.killed.Store(kill)
}

func (s *Service) Run() error {
	s.log.Debugw("starting service", "version", version.Version)

	if srv := s.promServer; srv != nil {
		l, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		defer l.Close()
		go func() {
			_ = srv.Serve(l)
		}()
	}

	if srv := s.pprofServer; srv != nil {
		l, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		defer l.Close()
		go func() {
			_ = srv.Serve(l)
		}()
	}

	var err error
	if s.rpcSIPServer, err = rpc.NewSIPInternalServer(s.psrpcServer, s.bus); err != nil {
		return err
	}
	defer s.rpcSIPServer.Shutdown()

	if err := s.RegisterCreateSIPParticipantTopic(); err != nil {
		return err
	}

	s.log.Debugw("service ready")

	for { // nolint: gosimple
		select {
		case <-s.shutdown.Watch():
			s.log.Infow("shutting down")
			s.DeregisterCreateSIPParticipantTopic()

			if !s.killed.Load() {
				shutdownTicker := time.NewTicker(5 * time.Second)
				defer shutdownTicker.Stop()
				var activeCalls int

				for !s.killed.Load() {
					activeCalls = s.sipServiceActiveCalls()
					if activeCalls == 0 {
						break
					}
					s.log.Infow("waiting for calls to finish", "calls", activeCalls)
					<-shutdownTicker.C
				}
			}

			s.sipServiceStop()
			return nil
		}
	}
}

func (s *Service) GetAuthCredentials(ctx context.Context, callID, from, to, toHost string, srcAddress netip.Addr) (sip.AuthInfo, error) {
	return GetAuthCredentials(ctx, s.psrpcClient, callID, from, to, toHost, srcAddress)
}

func (s *Service) DispatchCall(ctx context.Context, info *sip.CallInfo) sip.CallDispatch {
	return DispatchCall(ctx, s.psrpcClient, s.log, info)
}

func (s *Service) GetMediaProcessor(_ []livekit.SIPFeature) media.PCM16Processor {
	return nil
}

func (s *Service) CanAccept() bool {
	return s.mon.CanAccept()
}

func (s *Service) RegisterCreateSIPParticipantTopic() error {
	if s.rpcSIPServer != nil {
		return s.rpcSIPServer.RegisterCreateSIPParticipantTopic(s.conf.ClusterID)
	}

	return nil
}

func (s *Service) DeregisterCreateSIPParticipantTopic() {
	if s.rpcSIPServer != nil {
		s.rpcSIPServer.DeregisterCreateSIPParticipantTopic(s.conf.ClusterID)
	}
}

func (s *Service) RegisterTransferSIPParticipantTopic(sipCallId string) error {
	if s.rpcSIPServer != nil {
		return s.rpcSIPServer.RegisterTransferSIPParticipantTopic(sipCallId)
	}

	return psrpc.NewErrorf(psrpc.Internal, "RPC server not started")
}

func (s *Service) DeregisterTransferSIPParticipantTopic(sipCallId string) {
	if s.rpcSIPServer != nil {
		s.rpcSIPServer.DeregisterTransferSIPParticipantTopic(sipCallId)
	}
}
