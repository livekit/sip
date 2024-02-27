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
	"net"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
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
	Result   DispatchResult
	RoomName string
	Identity string
	WsUrl    string
	Token    string
}

type Handler interface {
	GetAuthCredentials(ctx context.Context, fromUser, toUser, toHost, srcAddress string) (username, password string, drop bool, err error)
	DispatchCall(ctx context.Context, info *CallInfo) CallDispatch
}

type Server struct {
	mon         *stats.Monitor
	sipSrv      *sipgo.Server
	sipConn     *net.UDPConn
	signalingIp string

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

func NewServer(conf *config.Config, mon *stats.Monitor) *Server {
	s := &Server{
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

func (s *Server) Start() error {
	var err error
	if s.conf.UseExternalIP {
		if s.signalingIp, err = getPublicIP(); err != nil {
			return err
		}
	} else if s.conf.NAT1To1IP != "" {
		s.signalingIp = s.conf.NAT1To1IP
	} else {
		if s.signalingIp, err = getLocalIP(); err != nil {
			return err
		}
	}

	agent, err := sipgo.NewUA(
		sipgo.WithUserAgent(UserAgent),
	)
	if err != nil {
		return err
	}

	s.sipSrv, err = sipgo.NewServer(agent)
	if err != nil {
		return err
	}

	s.sipSrv.OnInvite(s.onInvite)
	s.sipSrv.OnBye(s.onBye)

	// Ignore ACKs
	s.sipSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})

	lis, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: s.conf.SIPPort,
	})
	if err != nil {
		return fmt.Errorf("cannot listen on the signaling port %d: %w", s.conf.SIPPort, err)
	}
	s.sipConn = lis

	go func() {
		if err := s.sipSrv.ServeUDP(lis); err != nil {
			panic(fmt.Errorf("SIP listen UDP error: %w", err))
		}
	}()

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
	if s.sipConn != nil {
		s.sipConn.Close()
	}
}
