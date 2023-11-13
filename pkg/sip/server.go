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
	"github.com/pion/sdp/v2"
)

const (
	userAgent = "LiveKit"
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type (
	onInviteFunc func(from, to, srcAddress string) (joinRoom string, pinRequired bool, hangup bool)
	onPinFunc    func()

	Server struct {
		sipSrv *sipgo.Server

		activeInvites map[string]activeInvite
		onInvite      onInviteFunc
		onPin         onPinFunc
		conf          *config.Config
	}

	activeInvite struct {
		udpConn *net.UDPConn
	}
)

func NewServer(conf *config.Config, onInvite onInviteFunc, onPin onPinFunc) *Server {
	return &Server{
		conf:          conf,
		activeInvites: map[string]activeInvite{},
		onInvite:      onInvite,
		onPin:         onPin,
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

func sipErrorResponse(tx sip.ServerTransaction, req *sip.Request) {
	if err := tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil)); err != nil {
		log.Println(err)
	}
}

func sipSuccessResponse(tx sip.ServerTransaction, req *sip.Request, body []byte) {
	if err := tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", body)); err != nil {
		log.Println(err)
	}
}

func (s *Server) Start() error {
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
			sipErrorResponse(tx, req)
			return
		}

		from, ok := req.From()
		if !ok {
			sipErrorResponse(tx, req)
			return
		}

		_, _, rejectInvite := s.onInvite("", "", "")
		if rejectInvite {
			sipErrorResponse(tx, req)
			return
		}

		udpConn, err := createMediaSession(s.conf, from.Address.User)
		if err != nil {
			sipErrorResponse(tx, req)
			return
		}

		offer := sdp.SessionDescription{}
		if err := offer.Unmarshal(req.Body()); err != nil {
			sipErrorResponse(tx, req)
			return
		}

		s.activeInvites[tag] = activeInvite{udpConn: udpConn}

		answer, err := generateAnswer(offer, publicIp, udpConn.LocalAddr().(*net.UDPAddr).Port)
		if err != nil {
			sipErrorResponse(tx, req)
			return
		}

		res := sip.NewResponseFromRequest(req, 200, "OK", answer)
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: publicIp, Port: s.conf.SIPPort}})
		res.AppendHeader(&contentTypeHeaderSDP)
		if err = tx.Respond(res); err != nil {
			log.Println(err)
		}
	})

	s.sipSrv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		tag, err := getTagValue(req)
		if err != nil {
			sipErrorResponse(tx, req)
			return
		}

		if activeInvite, ok := s.activeInvites[tag]; ok {
			if activeInvite.udpConn != nil {
				fmt.Println("closing")
				activeInvite.udpConn.Close()
			}
			delete(s.activeInvites, tag)
		}

		sipSuccessResponse(tx, req, nil)
	})

	s.sipSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		sipSuccessResponse(tx, req, nil)
	})

	go func() {
		panic(s.sipSrv.ListenAndServe(context.TODO(), "udp", fmt.Sprintf("0.0.0.0:%d", s.conf.SIPPort)))
	}()

	return nil
}

func (s *Server) Stop() error {
	if s.sipSrv != nil {
		return s.sipSrv.Close()
	}

	return nil
}
