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
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/version"
)

type ServiceConfig struct {
	SignalingIP      netip.Addr
	SignalingIPLocal netip.Addr
	MediaIP          netip.Addr
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

type GetIOInfoClient func(projectID string) rpc.IOInfoClient

func NewService(region string, conf *config.Config, mon *stats.Monitor, log logger.Logger, getIOClient GetIOInfoClient) (*Service, error) {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &Service{
		conf:             conf,
		log:              log,
		mon:              mon,
		cli:              NewClient(region, conf, log, mon, getIOClient),
		srv:              NewServer(region, conf, log, mon, getIOClient),
		pendingTransfers: make(map[transferKey]chan struct{}),
	}
	var err error
	s.sconf, err = GetServiceConfig(s.conf)
	if err != nil {
		return nil, err
	}

	const placeholder = "${IP}"
	if strings.Contains(s.conf.SIPHostname, placeholder) {
		s.conf.SIPHostname = strings.ReplaceAll(
			s.conf.SIPHostname,
			placeholder,
			strings.NewReplacer(
				".", "-", // IPv4
				"[", "", "]", "", ":", "-", // IPv6
			).Replace(s.sconf.SignalingIP.String()),
		)
		addr, err := net.ResolveTCPAddr("tcp4", s.conf.SIPHostname)
		if err != nil {
			log.Errorw("cannot resolve node hostname", err, "hostname", s.conf.SIPHostname)
		} else {
			log.Infow("resolved node hostname", "hostname", s.conf.SIPHostname, "ip", addr.IP.String())
		}
	}
	if strings.ContainsAny(s.conf.SIPHostname, "$%{}[]:/| ") {
		return nil, fmt.Errorf("invalid hostname: %q", s.conf.SIPHostname)
	}
	if s.conf.SIPHostname != "" {
		log.Infow("using hostname", "hostname", s.conf.SIPHostname)
	}
	if s.conf.SIPRingingInterval < 1*time.Second || s.conf.SIPRingingInterval > 60*time.Second {
		s.conf.SIPRingingInterval = 1 * time.Second
		log.Infow("ringing interval", "seconds", s.conf.SIPRingingInterval)
	}
	return s, nil
}

type ActiveCalls struct {
	Inbound   int
	Outbound  int
	SampleIDs []string
}

func (st ActiveCalls) Total() int {
	return st.Outbound + st.Inbound
}

func sampleMap[K comparable, V any](limit int, m map[K]V, sample func(v V) string) ([]string, int) {
	total := len(m)
	var out []string
	for _, v := range m {
		if s := sample(v); s != "" {
			out = append(out, s)
		}
		limit--
		if limit <= 0 {
			break
		}
	}
	return out, total
}

func (s *Service) ActiveCalls() ActiveCalls {
	st := ActiveCalls{}

	s.cli.cmu.Lock()
	samples, total := sampleMap(5, s.cli.activeCalls, func(v *outboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	st.Outbound = total
	st.SampleIDs = append(st.SampleIDs, samples...)
	s.cli.cmu.Unlock()

	s.srv.cmu.Lock()
	samples, total = sampleMap(5, s.srv.activeCalls, func(v *inboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	st.Inbound = total
	st.SampleIDs = append(st.SampleIDs, samples...)
	s.srv.cmu.Unlock()

	return st
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
	msdk.CodecsSetEnabled(s.conf.Codecs)

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
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
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

	var transferResult atomic.Pointer[error]

	s.mu.Lock()
	k := transferKey{
		SipCallId:  req.SipCallId,
		TransferTo: req.TransferTo,
	}
	done, ok := s.pendingTransfers[k]
	if !ok {
		done = make(chan struct{})
		s.pendingTransfers[k] = done

		timeout := req.RingingTimeout.AsDuration()
		if timeout <= 0 {
			timeout = 80 * time.Second
		}

		go func() {
			ctx, cdone := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cdone()

			err := s.processParticipantTransfer(ctx, req.SipCallId, req.TransferTo, req.Headers, req.PlayDialtone)
			transferResult.Store(&err)
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
		errPtr := transferResult.Load()
		if errPtr != nil {
			err = *errPtr
		}
		return &emptypb.Empty{}, err
	case <-ctx.Done():
		return &emptypb.Empty{}, psrpc.NewError(psrpc.Canceled, ctx.Err())
	}
}

func (s *Service) processParticipantTransfer(ctx context.Context, callID string, transferTo string, headers map[string]string, dialtone bool) error {
	// Look for call both in client (outbound) and server (inbound)
	s.cli.cmu.Lock()
	out := s.cli.activeCalls[LocalTag(callID)]
	s.cli.cmu.Unlock()

	if out != nil {
		err := out.transferCall(ctx, transferTo, headers, dialtone)
		if err != nil {
			return err
		}

		return nil
	}

	s.srv.cmu.Lock()
	in := s.srv.byLocal[LocalTag(callID)]
	s.srv.cmu.Unlock()

	if in != nil {
		err := in.transferCall(ctx, transferTo, headers, dialtone)
		if err != nil {
			return err
		}

		return nil
	}

	return psrpc.NewErrorf(psrpc.NotFound, "unknown call")
}
