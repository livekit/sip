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
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/livekit/sip/pkg/config"
	"github.com/pion/sdp/v2"
)

const (
	userAgent   = "LiveKit"
	digestLimit = 500
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type (
	authenticationHandlerFunc func(from, to, srcAddress string) (username, password string, err error)
	dispatchRuleHandlerFunc   func(callingNumber, calledNumber, srcAddress string) (joinRoom string, pinRequired bool, hangup bool)

	Server struct {
		sipSrv   *sipgo.Server
		publicIp string

		inProgressInvites []*inProgressInvite
		activeInvites     map[string]activeInvite

		authenticationHandler authenticationHandlerFunc
		dispatchRuleHandler   dispatchRuleHandlerFunc
		conf                  *config.Config
	}

	inProgressInvite struct {
		from      string
		challenge digest.Challenge
	}

	activeInvite struct {
		udpConn *net.UDPConn
	}
)

func NewServer(conf *config.Config, authenticationHandler authenticationHandlerFunc, dispatchRuleHandler dispatchRuleHandlerFunc) *Server {
	return &Server{
		conf:                  conf,
		publicIp:              getPublicIP(),
		activeInvites:         map[string]activeInvite{},
		inProgressInvites:     []*inProgressInvite{},
		authenticationHandler: authenticationHandler,
		dispatchRuleHandler:   dispatchRuleHandler,
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

func (s *Server) handleInviteAuth(req *sip.Request, tx sip.ServerTransaction, from, username, password string) (ok bool) {
	if username == "" || password == "" {
		return true
	}

	var inviteState *inProgressInvite
	for i := range s.inProgressInvites {
		if s.inProgressInvites[i].from == from {
			inviteState = s.inProgressInvites[i]
		}
	}

	if inviteState == nil {
		if len(s.inProgressInvites) >= digestLimit {
			s.inProgressInvites = s.inProgressInvites[1:]
		}

		inviteState = &inProgressInvite{from: from}
		s.inProgressInvites = append(s.inProgressInvites, inviteState)
	}

	h := req.GetHeader("Authorization")
	if h == nil {
		inviteState.challenge = digest.Challenge{
			Realm:     userAgent,
			Nonce:     fmt.Sprintf("%d", time.Now().UnixMicro()),
			Algorithm: "MD5",
		}

		res := sip.NewResponseFromRequest(req, 401, "Unathorized", nil)
		res.AppendHeader(sip.NewHeader("WWW-Authenticate", inviteState.challenge.String()))
		logOnError(tx.Respond(res))
		return false
	}

	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		logOnError(tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil)))
		return false
	}

	digCred, err := digest.Digest(&inviteState.challenge, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: cred.Username,
		Password: password,
	})

	if err != nil {
		logOnError(tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil)))
		return false
	}

	if cred.Response != digCred.Response {
		// 	fmt.Println(cred.Response, digCred.Response)
		logOnError(tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)))
		return false
	}

	return true
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
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

	to, ok := req.To()
	if !ok {
		sipErrorResponse(tx, req)
		return
	}

	username, password, err := s.authenticationHandler(from.Address.User, to.Address.User, req.Source())
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}

	if !s.handleInviteAuth(req, tx, from.Address.User, username, password) {
		// handleInviteAuth will generate the SIP Response as needed
		return
	}

	roomName, _, rejectInvite := s.dispatchRuleHandler(from.Address.User, to.Address.User, req.Source())
	if rejectInvite {
		sipErrorResponse(tx, req)
		return
	}

	udpConn, err := createMediaSession(s.conf, roomName, from.Address.User)
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

	answer, err := generateAnswer(offer, s.publicIp, udpConn.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}

	res := sip.NewResponseFromRequest(req, 200, "OK", answer)
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: s.publicIp, Port: s.conf.SIPPort}})
	res.AppendHeader(&contentTypeHeaderSDP)
	if err = tx.Respond(res); err != nil {
		log.Println(err)
	}

}

func (s *Server) Start() error {
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

	s.sipSrv.OnInvite(s.onInvite)

	s.sipSrv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		tag, err := getTagValue(req)
		if err != nil {
			sipErrorResponse(tx, req)
			return
		}

		if activeInvite, ok := s.activeInvites[tag]; ok {
			if activeInvite.udpConn != nil {
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
