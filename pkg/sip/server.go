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
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/livekit/protocol/logger"
	"golang.org/x/exp/maps"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	UserAgent   = "LiveKit"
	digestLimit = 500
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type CallInfo struct {
	ID         string
	FromUser   string
	ToUser     string
	ToHost     string
	SrcAddress string
	Pin        string
	NoPin      bool
}

type DispatchResult int

const (
	DispatchAccept = DispatchResult(iota)
	DispatchRequestPin
	DispatchNoRuleReject // reject the call with an error
	DispatchNoRuleDrop   // silently drop the call
)

type CallDispatch struct {
	Result         DispatchResult
	RoomName       string
	Identity       string
	Name           string
	Metadata       string
	Attributes     map[string]string
	WsUrl          string
	Token          string
	TrunkID        string
	DispatchRuleID string
}

type Handler interface {
	GetAuthCredentials(ctx context.Context, fromUser, toUser, toHost, srcAddress string) (username, password string, drop bool, err error)
	DispatchCall(ctx context.Context, info *CallInfo) CallDispatch
}

type Server struct {
	log              logger.Logger
	mon              *stats.Monitor
	sipSrv           *sipgo.Server
	sipConnUDP       *net.UDPConn
	sipConnTCP       *net.TCPListener
	sipUnhandled     sipgo.RequestHandler
	signalingIp      string
	signalingIpLocal string

	inProgressInvites []*inProgressInvite

	cmu         sync.RWMutex
	activeCalls map[string]*inboundCall

	handler Handler
	conf    *config.Config

	res mediaRes
}

type inProgressInvite struct {
	from      string
	challenge digest.Challenge
}

func NewServer(conf *config.Config, log logger.Logger, mon *stats.Monitor) *Server {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &Server{
		log:               log,
		conf:              conf,
		mon:               mon,
		activeCalls:       make(map[string]*inboundCall),
		inProgressInvites: []*inProgressInvite{},
	}
	s.initMediaRes()
	return s
}

func (s *Server) SetHandler(handler Handler) {
	s.handler = handler
}

func getTagValue(req *sip.Request) (string, error) {
	from, ok := req.From()
	if !ok {
		return "", fmt.Errorf("No From on Request")
	}

	tag, ok := from.Params["tag"]
	if !ok {
		return "", fmt.Errorf("No tag on From")
	}

	return tag, nil
}

func sipErrorResponse(tx sip.ServerTransaction, req *sip.Request) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
}

func (s *Server) startUDP() error {
	lis, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: s.conf.SIPPort,
	})
	if err != nil {
		return fmt.Errorf("cannot listen on the UDP signaling port %d: %w", s.conf.SIPPort, err)
	}
	s.sipConnUDP = lis
	s.log.Infow("sip signaling listening on", "local", s.signalingIpLocal, "external", s.signalingIp, "port", s.conf.SIPPort, "proto", "udp")

	go func() {
		if err := s.sipSrv.ServeUDP(lis); err != nil {
			panic(fmt.Errorf("SIP listen UDP error: %w", err))
		}
	}()
	return nil
}

func (s *Server) startTCP() error {
	lis, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: s.conf.SIPPort,
	})
	if err != nil {
		return fmt.Errorf("cannot listen on the TCP signaling port %d: %w", s.conf.SIPPort, err)
	}
	s.sipConnTCP = lis
	s.log.Infow("sip signaling listening on", "local", s.signalingIpLocal, "external", s.signalingIp, "port", s.conf.SIPPort, "proto", "tcp")

	go func() {
		if err := s.sipSrv.ServeTCP(lis); err != nil && !errors.Is(err, net.ErrClosed) {
			panic(fmt.Errorf("SIP listen TCP error: %w", err))
		}
	}()
	return nil
}

func (s *Server) Start(agent *sipgo.UserAgent, unhandled sipgo.RequestHandler) error {
	var err error
	if s.conf.UseExternalIP {
		if s.signalingIp, err = getPublicIP(); err != nil {
			return err
		}
		if s.signalingIpLocal, err = getLocalIP(s.conf.LocalNet); err != nil {
			return err
		}
	} else if s.conf.NAT1To1IP != "" {
		s.signalingIp = s.conf.NAT1To1IP
		s.signalingIpLocal = s.signalingIp
	} else {
		if s.signalingIp, err = getLocalIP(s.conf.LocalNet); err != nil {
			return err
		}
		s.signalingIpLocal = s.signalingIp
	}
	s.log.Infow("server starting", "local", s.signalingIpLocal, "external", s.signalingIp)

	if agent == nil {
		ua, err := sipgo.NewUA(
			sipgo.WithUserAgent(UserAgent),
		)
		if err != nil {
			return err
		}
		agent = ua
	}

	s.sipSrv, err = sipgo.NewServer(agent)
	if err != nil {
		return err
	}

	s.sipSrv.OnInvite(s.onInvite)
	s.sipSrv.OnBye(s.onBye)
	s.sipUnhandled = unhandled

	// Ignore ACKs
	s.sipSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		fromTag, err := getTagValue(req)
		if err != nil {
			sipErrorResponse(tx, req)
			return
		}

		s.cmu.RLock()
		c, found := s.activeCalls[fromTag]
		s.cmu.RUnlock()

		if found {
			if header, ok := req.To(); ok {
				c.to.Params["tag"] = header.Params["tag"]
			}
		}
	})

	if err := s.startUDP(); err != nil {
		return err
	}
	if err := s.startTCP(); err != nil {
		return err
	}

	return nil
}

func (s *Server) Stop() {
	s.cmu.Lock()
	calls := maps.Values(s.activeCalls)
	s.activeCalls = make(map[string]*inboundCall)
	s.cmu.Unlock()
	for _, c := range calls {
		c.Close()
	}
	if s.sipSrv != nil {
		s.sipSrv.Close()
	}
	if s.sipConnUDP != nil {
		s.sipConnUDP.Close()
	}
	if s.sipConnTCP != nil {
		s.sipConnTCP.Close()
	}
}
