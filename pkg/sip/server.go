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
	"log"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"golang.org/x/exp/maps"

	"github.com/livekit/sip/pkg/config"
)

const (
	UserAgent   = "LiveKit"
	digestLimit = 500
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type (
	AuthHandlerFunc         func(from, to, srcAddress string) (username, password string, err error)
	DispatchRuleHandlerFunc func(callingNumber, calledNumber, srcAddress string, pin string, noPin bool) (joinRoom, identity string, requestPin, rejectInvite bool)
	Server                  struct {
		sipSrv      *sipgo.Server
		signalingIp string

		inProgressInvites []*inProgressInvite

		cmu         sync.RWMutex
		activeCalls map[string]*inboundCall

		authHandler         AuthHandlerFunc
		dispatchRuleHandler DispatchRuleHandlerFunc
		conf                *config.Config

		res mediaRes
	}

	inProgressInvite struct {
		from      string
		challenge digest.Challenge
	}
)

func NewServer(conf *config.Config) *Server {
	s := &Server{
		conf:              conf,
		activeCalls:       make(map[string]*inboundCall),
		inProgressInvites: []*inProgressInvite{},
	}
	s.initMediaRes()
	return s
}

func (s *Server) SetAuthHandler(handler AuthHandlerFunc) {
	s.authHandler = handler
}

func (s *Server) SetDispatchRuleHandlerFunc(handler DispatchRuleHandlerFunc) {
	s.dispatchRuleHandler = handler
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
	logOnError(tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil)))
}

func sipSuccessResponse(tx sip.ServerTransaction, req *sip.Request, body []byte) {
	logOnError(tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", body)))
}

func logOnError(err error) {
	if err != nil {
		log.Println(err)
	}
}

func (s *Server) Start(agent *sipgo.UserAgent) error {
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
	s.sipSrv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		tag, err := getTagValue(req)
		if err != nil {
			sipErrorResponse(tx, req)
			return
		}

		s.cmu.RLock()
		c := s.activeCalls[tag]
		s.cmu.RUnlock()
		if c != nil {
			c.Close()
		}

		sipSuccessResponse(tx, req, nil)
	})

	// Ignore ACKs
	s.sipSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})

	go func() {
		panic(s.sipSrv.ListenAndServe(context.TODO(), "udp", fmt.Sprintf("0.0.0.0:%d", s.conf.SIPPort)))
	}()

	return nil
}

func (s *Server) Stop() error {
	s.cmu.Lock()
	calls := maps.Values(s.activeCalls)
	s.activeCalls = make(map[string]*inboundCall)
	s.cmu.Unlock()
	for _, c := range calls {
		c.Close()
	}
	if s.sipSrv != nil {
		return s.sipSrv.Close()
	}

	return nil
}
