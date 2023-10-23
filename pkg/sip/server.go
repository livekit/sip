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

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/livekit/sip/pkg/config"
)

const (
	userAgent = "LiveKit"
	sipPort   = 5060
	httpPort  = 8080
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type Server struct {
}

func NewServer() *Server {
	return &Server{}
}

func (s *Server) Start(conf *config.Config) error {
	publicIp := getPublicIP()

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(userAgent),
	)
	if err != nil {
		log.Fatal(err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Fatal(err)
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		rtpListenerPort := createRTPListener()

		res := sip.NewResponseFromRequest(req, 200, "OK", generateAnswer(req.Body(), publicIp, rtpListenerPort))
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: publicIp, Port: sipPort}})
		res.AppendHeader(&contentTypeHeaderSDP)
		tx.Respond(res)
	})

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	go func() {
		panic(srv.ListenAndServe(context.TODO(), "udp", fmt.Sprintf("0.0.0.0:%d", sipPort)))
	}()

	return nil
}

func (s *Server) Stop() error {
	return nil
}
