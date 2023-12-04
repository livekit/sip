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
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/ulaw"
)

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
			Realm:     UserAgent,
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

	username, password, err := s.authHandler(from.Address.User, to.Address.User, to.Address.Host, src)
	if err != nil {
		log.Printf("Rejecting inbound call, doesn't match any Trunks %q %q %q %q\n", from.Address.User, to.Address.User, to.Address.Host, src)
		sipErrorResponse(tx, req)
		return
	}
	if !s.handleInviteAuth(req, tx, from.Address.User, username, password) {
		// handleInviteAuth will generate the SIP Response as needed
		return
	}
	call := s.newInboundCall(tag, from, to, src)
	call.handleInvite(req, tx, s.conf)
}

type inboundCall struct {
	s            *Server
	tag          string
	from         *sip.FromHeader
	to           *sip.ToHeader
	src          string
	rtpConn      *MediaConn
	audioHandler atomic.Pointer[rtp.Handler]
	dtmf         chan byte // buffered; DTMF digits as characters
	lkRoom       *Room     // LiveKit room; only active after correct pin is entered
	done         atomic.Bool
}

func (s *Server) newInboundCall(tag string, from *sip.FromHeader, to *sip.ToHeader, src string) *inboundCall {
	c := &inboundCall{
		s:      s,
		tag:    tag,
		from:   from,
		to:     to,
		src:    src,
		dtmf:   make(chan byte, 10),
		lkRoom: NewRoom(), // we need it created earlier so that the audio mixer is available for pin prompts
	}
	return c
}

func (c *inboundCall) handleInvite(req *sip.Request, tx sip.ServerTransaction, conf *config.Config) {
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could even learn that this number is not allowed and reject the call, or ask for pin if required.
	roomName, identity, requirePin, rejectInvite := c.s.dispatchRuleHandler(c.from.Address.User, c.to.Address.User, c.to.Address.Host, c.src, "", false)
	if rejectInvite {
		log.Printf("Rejecting inbound call, doesn't match any Dispatch Rules %q %q %q %q\n", c.from.Address.User, c.to.Address.User, c.to.Address.Host, c.src)
		sipErrorResponse(tx, req)
		return
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMediaConn(req.Body(), conf)

	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	c.s.cmu.Lock()
	c.s.activeCalls[c.tag] = c
	c.s.cmu.Unlock()

	res := sip.NewResponseFromRequest(req, 200, "OK", answerData)
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: c.s.signalingIp, Port: c.s.conf.SIPPort}})
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

func (c *inboundCall) runMediaConn(offerData []byte, conf *config.Config) (answerData []byte, _ error) {
	conn := NewMediaConn()
	conn.OnRTP(c)
	if err := conn.Start(conf.RTPPort.Start, conf.RTPPort.End, "0.0.0.0"); err != nil {
		return nil, err
	}
	c.rtpConn = conn
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(offerData); err != nil {
		return nil, err
	}

	// Encoding pipeline (LK -> SIP)
	// Need to be created earlier to send the pin prompts.
	s := rtp.NewMediaStreamOut[ulaw.Sample](conn, rtpPacketDur)
	c.lkRoom.SetOutput(ulaw.Decode(s))

	return sdpGenerateAnswer(offer, c.s.signalingIp, conn.LocalAddr().Port)
}

func (c *inboundCall) pinPrompt() {
	log.Printf("Requesting Pin for SIP call %q -> %q\n", c.from.Address.User, c.to.Address.User)
	const pinLimit = 16
	c.playAudio(c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case b, ok := <-c.dtmf:
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
				roomName, identity, requirePin, reject := c.s.dispatchRuleHandler(c.from.Address.User, c.to.Address.User, c.to.Address.Host, c.src, pin, noPin)
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

func (c *inboundCall) closeMedia() {
	c.audioHandler.Store(nil)
	if p := c.lkRoom; p != nil {
		p.Close()
		c.lkRoom = nil
	}
	c.rtpConn.Close()
	close(c.dtmf)
}

func (c *inboundCall) HandleRTP(p *rtp.Packet) error {
	if p.Marker && p.PayloadType == 101 {
		c.handleDTMF(p.Payload)
		return nil
	}
	// TODO: Audio data appears to be coming with PayloadType=0, so maybe enforce it?
	if h := c.audioHandler.Load(); h != nil {
		return (*h).HandleRTP(p)
	}
	return nil
}

func (c *inboundCall) createLiveKitParticipant(roomName, participantIdentity string) error {
	err := c.lkRoom.Connect(c.s.conf, roomName, participantIdentity)
	if err != nil {
		return err
	}
	local, err := c.lkRoom.NewParticipant()
	if err != nil {
		_ = c.lkRoom.Close()
		return err
	}

	// Decoding pipeline (SIP -> LK)
	lpcm := media.DecodePCM(local)
	law := ulaw.Encode(lpcm)
	var h rtp.Handler = rtp.NewMediaStreamIn(law)
	c.audioHandler.Store(&h)

	return nil
}

func (c *inboundCall) joinRoom(roomName, identity string) {
	log.Printf("Bridging SIP call %q -> %q to room %q (as %q)\n", c.from.Address.User, c.to.Address.User, roomName, identity)
	c.playAudio(c.s.res.roomJoin)
	if err := c.createLiveKitParticipant(roomName, identity); err != nil {
		log.Println(err)
	}
}

func (c *inboundCall) playAudio(frames []media.PCM16Sample) {
	t := c.lkRoom.NewTrack()
	defer t.Close()
	t.PlayAudio(frames)
}

func (c *inboundCall) handleDTMF(data []byte) { // RFC2833
	if len(data) < 4 {
		return
	}
	ev := data[0]
	b := dtmfEventToChar[ev]
	// We should have enough buffer here.
	select {
	case c.dtmf <- b:
	default:
	}
}
