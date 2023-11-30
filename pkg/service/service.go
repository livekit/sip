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

	"github.com/emiago/sipgo"
	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/version"
)

type Service struct {
	conf *config.Config
	cli  *sip.Client
	srv  *sip.Server

	psrpcClient rpc.IOInfoClient
	bus         psrpc.MessageBus

	shutdown core.Fuse
}

func NewService(conf *config.Config, bus psrpc.MessageBus) (*Service, error) {
	psrpcClient, err := rpc.NewIOInfoClient(bus)
	if err != nil {
		return nil, err
	}
	cli := sip.NewClient(conf)
	s := &Service{
		conf:        conf,
		cli:         cli,
		psrpcClient: psrpcClient,
		bus:         bus,
		shutdown:    core.NewFuse(),
	}
	s.srv = sip.NewServer(conf, s.HandleTrunkAuthentication, s.HandleDispatchRules)
	return s, nil
}

func (s *Service) Stop(kill bool) {
	s.shutdown.Break()
	if kill {
		if err := s.cli.Stop(); err != nil {
			log.Println(err)
		}
		if err := s.srv.Stop(); err != nil {
			log.Println(err)
		}
	}
}

func (s *Service) Run() error {
	logger.Debugw("starting service", "version", version.Version)
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(sip.UserAgent),
	)
	if err != nil {
		return err
	}
	if err = s.cli.Start(ua); err != nil {
		return err
	}
	if err = s.srv.Start(ua); err != nil {
		return err
	}

	srv, err := rpc.NewSIPInternalServer(s.cli, s.bus)
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

func (s *Service) HandleTrunkAuthentication(from, to, srcAddress string) (username, password string, err error) {
	resp, err := s.psrpcClient.GetSIPTrunkAuthentication(context.TODO(), &rpc.GetSIPTrunkAuthenticationRequest{
		From:       from,
		To:         to,
		SrcAddress: srcAddress,
	})

	if err != nil {
		return "", "", err
	}

	return resp.Username, resp.Password, nil
}

func (s *Service) HandleDispatchRules(callingNumber, calledNumber, srcAddress string, pin string, noPin bool) (joinRoom, identity string, requestPin, rejectInvite bool) {
	resp, err := s.psrpcClient.EvaluateSIPDispatchRules(context.TODO(), &rpc.EvaluateSIPDispatchRulesRequest{
		CallingNumber: callingNumber,
		CalledNumber:  calledNumber,
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
