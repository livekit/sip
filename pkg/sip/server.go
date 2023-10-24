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

package sip

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/livekit/sip/pkg/config"
)

const (
	userAgent = "LiveKit"
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type activeInvite struct {
	udpConn *net.UDPConn
}

type Server struct {
	sipSrv *sipgo.Server

	activeInvites map[string]activeInvite
}

func NewServer() *Server {
	return &Server{
		activeInvites: map[string]activeInvite{},
	}
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

func (s *Server) Start(conf *config.Config) error {
	publicIp := getPublicIP()

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(userAgent),
	)
	if err != nil {
		log.Fatal(err)
	}

	s.sipSrv, err = sipgo.NewServer(ua)
	if err != nil {
		log.Fatal(err)
	}

	s.sipSrv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		tag, err := getTagValue(req)
		if err != nil {
			tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
			return
		}

		from, ok := req.From()
		if !ok {
			tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
			return
		}

		udpConn, err := createRTPListener(conf, from.Address.User)
		if err != nil {
			tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
			return
		}

		s.activeInvites[tag] = activeInvite{udpConn: udpConn}

		answer, err := generateAnswer(req.Body(), publicIp, udpConn.LocalAddr().(*net.UDPAddr).Port)
		if err != nil {
			tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
			return
		}

		res := sip.NewResponseFromRequest(req, 200, "OK", answer)
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: publicIp, Port: conf.SIPPort}})
		res.AppendHeader(&contentTypeHeaderSDP)
		tx.Respond(res)
	})

	s.sipSrv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		tag, err := getTagValue(req)
		if err != nil {
			tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
			return
		}

		if activeInvite, ok := s.activeInvites[tag]; ok {
			if activeInvite.udpConn != nil {
				activeInvite.udpConn.Close()
			}
			delete(s.activeInvites, tag)
		}

		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	s.sipSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	go func() {
		panic(s.sipSrv.ListenAndServe(context.TODO(), "udp", fmt.Sprintf("0.0.0.0:%d", conf.SIPPort)))
	}()

	return nil
}

func (s *Server) Stop() error {
	if s.sipSrv != nil {
		return s.sipSrv.Close()
	}

	return nil
}
