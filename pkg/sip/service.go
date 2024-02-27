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
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"golang.org/x/exp/maps"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/version"
)

type Service struct {
	conf *config.Config
	mon  *stats.Monitor
	cli  *Client
	srv  *Server
}

func NewService(conf *config.Config) (*Service, error) {
	mon := stats.NewMonitor()
	cli := NewClient(conf, mon)
	s := &Service{
		conf: conf,
		mon:  mon,
		cli:  cli,
	}
	s.srv = NewServer(conf, mon)
	return s, nil
}

func (s *Service) ActiveCalls() int {
	s.cli.cmu.Lock()
	activeClientCalls := len(maps.Values(s.cli.activeCalls))
	s.cli.cmu.Unlock()

	s.srv.cmu.Lock()
	activeServerCalls := len(maps.Values(s.srv.activeCalls))
	s.srv.cmu.Unlock()

	return activeClientCalls + activeServerCalls
}

func (s *Service) Stop() {
	s.cli.Stop()
	s.srv.Stop()
	s.mon.Stop()
}

func (s *Service) SetHandler(handler Handler) {
	s.srv.SetHandler(handler)
}

func (s *Service) InternalServerImpl() rpc.SIPInternalServerImpl {
	return s.cli
}

func (s *Service) Start() error {
	logger.Debugw("starting sip service", "version", version.Version)

	if err := s.mon.Start(s.conf); err != nil {
		return err
	}

	if err := s.cli.Start(); err != nil {
		return err
	}
	if err := s.srv.Start(); err != nil {
		return err
	}
	logger.Debugw("sip service ready")
	return nil
}
