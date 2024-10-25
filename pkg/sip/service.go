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
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/version"
)

type ServiceConfig struct {
	SignalingIP      netip.Addr
	SignalingIPLocal netip.Addr
}

type Service struct {
	conf  *config.Config
	sconf *ServiceConfig
	log   logger.Logger
	mon   *stats.Monitor
	cli   *Client
	srv   *Server

	mu               sync.Mutex
	pendingTransfers map[transferKey]chan struct{}
}

type transferKey struct {
	SipCallId  string
	TransferTo string
}

func NewService(conf *config.Config, mon *stats.Monitor, log logger.Logger) (*Service, error) {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &Service{
		conf:             conf,
		log:              log,
		mon:              mon,
		cli:              NewClient(conf, log, mon),
		srv:              NewServer(conf, log, mon),
		pendingTransfers: make(map[transferKey]chan struct{}),
	}
	var err error
	s.sconf, err = GetServiceConfig(s.conf)
	if err != nil {
		return nil, err
	}

	s.conf.SIPHostname = strings.ReplaceAll(
		s.conf.SIPHostname,
		"${IP}",
		strings.NewReplacer(
			".", "-", // IPv4
			"[", "", "]", "", ":", "-", // IPv6
		).Replace(s.sconf.SignalingIP.String()),
	)
	if strings.ContainsAny(s.conf.SIPHostname, "$%{}[]:/| ") {
		return nil, fmt.Errorf("invalid hostname: %q", s.conf.SIPHostname)
	}
	if s.conf.SIPHostname != "" {
		log.Infow("using hostname", "hostname", s.conf.SIPHostname)
	}
	return s, nil
}

func (s *Service) ActiveCalls() int {
	s.cli.cmu.Lock()
	activeClientCalls := len(s.cli.activeCalls)
	s.cli.cmu.Unlock()

	s.srv.cmu.Lock()
	activeServerCalls := len(s.srv.activeCalls)
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
	s.cli.SetHandler(handler)
}

func (s *Service) Start() error {
	s.log.Debugw("starting sip service", "version", version.Version)
	for name, enabled := range s.conf.Codecs {
		if enabled {
			s.log.Warnw("codec enabled", nil, "name", name)
		} else {
			s.log.Warnw("codec disabled", nil, "name", name)
		}
	}
	media.CodecsSetEnabled(s.conf.Codecs)

	if err := s.mon.Start(s.conf); err != nil {
		return err
	}
	// The UA must be shared between the client and the server.
	// Otherwise, the client will have to listen on a random port, which must then be forwarded.
	//
	// Routers are smart, they usually keep the UDP "session" open for a few moments, and may allow INVITE handshake
	// to pass even without forwarding rules on the firewall. ut it will inevitably fail later on follow-up requests like BYE.
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(UserAgent),
	)
	if err != nil {
		return err
	}
	if err := s.cli.Start(ua, s.sconf); err != nil {
		return err
	}
	// Server is responsible for answering all transactions. However, the client may also receive some (e.g. BYE).
	// Thus, all unhandled transactions will be checked by the client.
	if err := s.srv.Start(ua, s.sconf, s.cli.OnRequest); err != nil {
		return err
	}
	s.log.Debugw("sip service ready")
	return nil
}

func (s *Service) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	return s.cli.CreateSIPParticipant(ctx, req)
}

func (s *Service) CreateSIPParticipantAffinity(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) float32 {
	// TODO: scale affinity based on a number or active calls?
	return 0.5
}

func (s *Service) TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*emptypb.Empty, error) {
	s.log.Infow("transfering SIP call", "callID", req.SipCallId, "transferTo", req.TransferTo)

	var transfetResult atomic.Pointer[error]

	s.mu.Lock()
	k := transferKey{
		SipCallId:  req.SipCallId,
		TransferTo: req.TransferTo,
	}
	done, ok := s.pendingTransfers[k]
	if !ok {
		done = make(chan struct{})
		s.pendingTransfers[k] = done

		go func() {
			ctx, cdone := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cdone()

			err := s.processParticipantTransfer(ctx, req.SipCallId, req.TransferTo, req.PlayDialtone)
			transfetResult.Store(&err)
			close(done)

			s.mu.Lock()
			delete(s.pendingTransfers, k)
			s.mu.Unlock()
		}()
	} else {
		s.log.Debugw("repeated request for call transfer", "callID", req.SipCallId, "transferTo", req.TransferTo)
	}
	s.mu.Unlock()

	select {
	case <-done:
		var err error
		errPtr := transfetResult.Load()
		if errPtr != nil {
			err = *errPtr
		}
		return &emptypb.Empty{}, err
	case <-ctx.Done():
		return &emptypb.Empty{}, psrpc.NewError(psrpc.Canceled, ctx.Err())
	}
}

func (s *Service) processParticipantTransfer(ctx context.Context, callID string, transferTo string, dialtone bool) error {
	// Look for call both in client (outbound) and server (inbound)
	s.cli.cmu.Lock()
	out := s.cli.activeCalls[LocalTag(callID)]
	s.cli.cmu.Unlock()

	if out != nil {
		err := out.transferCall(ctx, transferTo, dialtone)
		if err != nil {
			return err
		}

		return nil
	}

	s.srv.cmu.Lock()
	in := s.srv.byLocal[LocalTag(callID)]
	s.srv.cmu.Unlock()

	if in != nil {
		err := in.transferCall(ctx, transferTo, dialtone)
		if err != nil {
			return err
		}

		return nil
	}

	return psrpc.NewErrorf(psrpc.NotFound, "unknown call")
}
