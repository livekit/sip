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

package sip

import (
	"log"

	"github.com/emiago/sipgo"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/version"
)

type Service struct {
	cli *Client
	srv *Server
}

func NewService(conf *config.Config) (*Service, error) {
	cli := NewClient(conf)
	s := &Service{
		cli: cli,
	}
	s.srv = NewServer(conf)
	return s, nil
}

func (s *Service) Stop(kill bool) {
	if kill {
		if err := s.cli.Stop(); err != nil {
			log.Println(err)
		}
		if err := s.srv.Stop(); err != nil {
			log.Println(err)
		}
	}
}

func (s *Service) SetServerHandler(handler ServerHandler) {
	s.srv.SetHandler(handler)
}

func (s *Service) InternalServerImpl() rpc.SIPInternalServerImpl {
	return s.cli
}

func (s *Service) Start() error {
	logger.Debugw("starting sip service", "version", version.Version)
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(UserAgent),
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
	logger.Debugw("sip service ready")
	return nil
}
