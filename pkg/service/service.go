// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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

	psrpcClient rpc.IOInfoClient
	bus         psrpc.MessageBus

	shutdown core.Fuse
}

func NewService(conf *config.Config, psrpcClient rpc.IOInfoClient, bus psrpc.MessageBus) *Service {
	s := &Service{
		conf:        conf,
		psrpcClient: psrpcClient,
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

	logger.Debugw("service ready")

	for {
		select {
		case <-s.shutdown.Watch():
			logger.Infow("shutting down")

			return nil
		}
	}
}

func (s *Service) HandleInvite(from, to, srcAddress string) (joinRoom string, requestPin bool, rejectInvite bool) {
	resp, err := s.psrpcClient.EvaluateSIPDispatchRules(context.TODO(), &rpc.EvaluateSIPDispatchRulesRequest{
		From:       from,
		To:         to,
		SrcAddress: srcAddress,
	})

	if err != nil {
		log.Println(err)
		return "", false, true
	}

	return resp.RoomName, false, false
}

func (s *Service) HandlePIN() {
}
