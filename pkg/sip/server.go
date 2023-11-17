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
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/sdp/v2"
	"golang.org/x/exp/maps"

	"github.com/livekit/sip/pkg/config"
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
	dispatchRuleHandlerFunc   func(callingNumber, calledNumber, srcAddress, pin string, skipPin bool) (joinRoom, identity string, pinRequired, hangup bool)

	Server struct {
		sipSrv   *sipgo.Server
		publicIp string

		inProgressInvites []*inProgressInvite

		cmu         sync.RWMutex
		activeCalls map[string]*inboundCall

		authenticationHandler authenticationHandlerFunc
		dispatchRuleHandler   dispatchRuleHandlerFunc
		conf                  *config.Config

		res mediaRes
	}

	inProgressInvite struct {
		from      string
		challenge digest.Challenge
	}
)

func NewServer(conf *config.Config, authenticationHandler authenticationHandlerFunc, dispatchRuleHandler dispatchRuleHandlerFunc) *Server {
	s := &Server{
		conf:                  conf,
		publicIp:              getPublicIP(),
		activeCalls:           make(map[string]*inboundCall),
		inProgressInvites:     []*inProgressInvite{},
		authenticationHandler: authenticationHandler,
		dispatchRuleHandler:   dispatchRuleHandler,
	}
	s.initMediaRes()
	return s
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

	h := req.GetHeader("Proxy-Authorization")
	if h == nil {
		inviteState.challenge = digest.Challenge{
			Realm:     userAgent,
			Nonce:     fmt.Sprintf("%d", time.Now().UnixMicro()),
			Algorithm: "MD5",
		}

		res := sip.NewResponseFromRequest(req, 407, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("Proxy-Authenticate", inviteState.challenge.String()))
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
	src := req.Source()

	username, password, err := s.authenticationHandler(from.Address.User, to.Address.User, src)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	if !s.handleInviteAuth(req, tx, from.Address.User, username, password) {
		// handleInviteAuth will generate the SIP Response as needed
		return
	}
	call := s.newCall(tag, from, to, src)
	call.handleInvite(req, tx)
}

type inboundCall struct {
	s     *Server
	tag   string
	from  *sip.FromHeader
	to    *sip.ToHeader
	src   string
	media mediaData
	done  atomic.Bool
}

func (s *Server) newCall(tag string, from *sip.FromHeader, to *sip.ToHeader, src string) *inboundCall {
	c := &inboundCall{
		s:    s,
		tag:  tag,
		from: from,
		to:   to,
		src:  src,
	}
	c.initMedia()
	return c
}

func (c *inboundCall) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could ever learn that this number is not allowed and reject the call, or ask for pin if required.
	roomName, identity, requirePin, rejectInvite := c.s.dispatchRuleHandler(c.from.Address.User, c.to.Address.User, c.src, "", false)
	if rejectInvite {
		sipErrorResponse(tx, req)
		return
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMedia(req.Body())
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	c.s.cmu.Lock()
	c.s.activeCalls[c.tag] = c
	c.s.cmu.Unlock()

	res := sip.NewResponseFromRequest(req, 200, "OK", answerData)
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: c.s.publicIp, Port: c.s.conf.SIPPort}})
	res.AppendHeader(&contentTypeHeaderSDP)
	if err = tx.Respond(res); err != nil {
		log.Println(err)
		// TODO: should we close the call in this case?
		return
	}
	// We own this goroutine, so can freely block.
	if requirePin {
		c.pinPrompt()
	} else {
		c.joinRoom(roomName, identity)
	}
}

func (c *inboundCall) runMedia(offerData []byte) ([]byte, error) {
	addr, err := c.createMediaSession()
	if err != nil {
		return nil, err
	}
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(offerData); err != nil {
		return nil, err
	}
	return generateAnswer(offer, c.s.publicIp, addr.Port)
}

func (c *inboundCall) pinPrompt() {
	log.Printf("Requesting Pin for SIP call %q -> %q\n", c.from.Address.User, c.to.Address.User)
	const pinLimit = 16
	c.playAudio(c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case b, ok := <-c.media.dtmf:
			if !ok {
				c.Close()
				return
			}
			if b == 0 {
				continue // unrecognized
			}
			if b == '#' {
				// End of the pin
				noPin = pin == ""

				log.Printf("Checking Pin for SIP call %q -> %q = %q (noPin = %v)\n", c.from.Address.User, c.to.Address.User, pin, noPin)
				roomName, identity, requirePin, reject := c.s.dispatchRuleHandler(c.from.Address.User, c.to.Address.User, c.src, pin, noPin)
				if reject || requirePin || roomName == "" {
					c.playAudio(c.s.res.wrongPin)
					c.Close()
					return
				}
				c.joinRoom(roomName, identity)
				return
			}
			// Gather pin numbers
			pin += string(b)
			if len(pin) > pinLimit {
				c.playAudio(c.s.res.wrongPin)
				c.Close()
				return
			}
		}
	}
}

func (c *inboundCall) Close() error {
	if c.done.CompareAndSwap(false, true) {
		return nil
	}
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.tag)
	c.s.cmu.Unlock()
	c.closeMedia()
	// FIXME: drop the actual call
	return nil
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
