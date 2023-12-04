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
	"log"

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/version"
)

type Service struct {
	conf *config.Config

	psrpcServer rpc.SIPInternalServerImpl
	psrpcClient rpc.IOInfoClient
	bus         psrpc.MessageBus

	shutdown core.Fuse
}

func NewService(conf *config.Config, srv rpc.SIPInternalServerImpl, cli rpc.IOInfoClient, bus psrpc.MessageBus) *Service {
	s := &Service{
		conf:        conf,
		psrpcServer: srv,
		psrpcClient: cli,
		bus:         bus,
		shutdown:    core.NewFuse(),
	}
	return s
}

func (s *Service) Stop(kill bool) {
	s.shutdown.Break()
}

func (s *Service) Run() error {
	logger.Debugw("starting service", "version", version.Version)

	srv, err := rpc.NewSIPInternalServer(s.psrpcServer, s.bus)
	if err != nil {
		return err
	}
	defer srv.Shutdown()

	logger.Debugw("service ready")

	for { //nolint: gosimple
		select {
		case <-s.shutdown.Watch():
			logger.Infow("shutting down")

			return nil
		}
	}
}

func (s *Service) HandleTrunkAuthentication(from, to, toHost, srcAddress string) (username, password string, err error) {
	resp, err := s.psrpcClient.GetSIPTrunkAuthentication(context.TODO(), &rpc.GetSIPTrunkAuthenticationRequest{
		From:       from,
		To:         to,
		ToHost:     toHost,
		SrcAddress: srcAddress,
	})

	if err != nil {
		return "", "", err
	}

	return resp.Username, resp.Password, nil
}

func (s *Service) HandleDispatchRules(callingNumber, calledNumber, calledHost, srcAddress string, pin string, noPin bool) (joinRoom, identity string, requestPin, rejectInvite bool) {
	resp, err := s.psrpcClient.EvaluateSIPDispatchRules(context.TODO(), &rpc.EvaluateSIPDispatchRulesRequest{
		CallingNumber: callingNumber,
		CalledNumber:  calledNumber,
		CalledHost:    calledHost,
		SrcAddress:    srcAddress,
		Pin:           pin,
		NoPin:         noPin,
	})

	if err != nil {
		log.Println(err)
		return "", "", false, true
	}

	return resp.RoomName, resp.ParticipantIdentity, resp.RequestPin, false
}

func (s *Service) CanAccept() bool {
	return true
}
