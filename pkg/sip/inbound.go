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
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/livekit/protocol/logger"
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
	_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))

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

	logger.Infow("INVITE", "tag", tag, "from", from, "to", to)

	username, password, err := s.authHandler(from.Address.User, to.Address.User, to.Address.Host, src)
	if err != nil {
		logger.Warnw("Rejecting inbound call, doesn't match any Trunks", err, "tag", tag, "src", src, "from", from, "to", to, "to-host", to.Address.Host)
		sipErrorResponse(tx, req)
		return
	}
	if !s.handleInviteAuth(req, tx, from.Address.User, username, password) {
		// handleInviteAuth will generate the SIP Response as needed
		return
	}
	call := s.newInboundCall(tag, from, to, src)
	call.handleInvite(call.ctx, req, tx, s.conf)
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getTagValue(req)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	logger.Infow("BYE", "tag", tag)

	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.Close()
	}

	sipSuccessResponse(tx, req, nil)
}

type inboundCall struct {
	s            *Server
	tag          string
	ctx          context.Context
	cancel       func()
	inviteReq    *sip.Request
	inviteResp   *sip.Response
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
	c.ctx, c.cancel = context.WithCancel(context.Background())
	s.cmu.Lock()
	s.activeCalls[tag] = c
	s.cmu.Unlock()
	return c
}

func (c *inboundCall) handleInvite(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, conf *config.Config) {
	defer c.close()
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could even learn that this number is not allowed and reject the call, or ask for pin if required.
	roomName, identity, wsUrl, token, requirePin, rejectInvite := c.s.dispatchRuleHandler(ctx, c.from.Address.User, c.to.Address.User, c.to.Address.Host, c.src, "", false)
	if rejectInvite {
		logger.Infow("Rejecting inbound call, doesn't match any Dispatch Rules", "from", c.from.Address.User, "to", c.to.Address.User, "to-host", c.to.Address.Host, "src", c.src)
		sipErrorResponse(tx, req)
		return
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMediaConn(req.Body(), conf)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}

	res := sip.NewResponseFromRequest(req, 200, "OK", answerData)
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: c.s.signalingIp, Port: c.s.conf.SIPPort}})
	res.AppendHeader(&contentTypeHeaderSDP)
	if err = tx.Respond(res); err != nil {
		logger.Errorw("Cannot respond to INVITE", err)
		return
	}
	c.inviteReq = req
	c.inviteResp = res
	// We own this goroutine, so can freely block.
	if requirePin {
		c.pinPrompt(ctx)
	} else {
		c.joinRoom(ctx, roomName, identity, wsUrl, token)
	}
	// Wait for the caller to terminate the call.
	<-ctx.Done()
}

func (c *inboundCall) sendBye() {
	if c.inviteReq == nil {
		return
	}
	// This function is for clients, so we need to swap src and dest
	bye := sip.NewByeRequest(c.inviteReq, c.inviteResp, nil)
	if contact, ok := c.inviteReq.Contact(); ok {
		bye.Recipient = &contact.Address
	} else {
		bye.Recipient = &c.from.Address
	}
	bye.SetSource(c.inviteResp.Source())
	bye.SetDestination(c.inviteResp.Destination())
	bye.RemoveHeader("From")
	bye.AppendHeader((*sip.FromHeader)(c.to))
	bye.RemoveHeader("To")
	bye.AppendHeader((*sip.ToHeader)(c.from))
	if route, ok := bye.RecordRoute(); ok {
		bye.RemoveHeader("Record-Route")
		bye.AppendHeader(&sip.RouteHeader{Address: route.Address})
	}
	_ = c.s.sipSrv.TransportLayer().WriteMsg(bye)
	c.inviteReq = nil
	c.inviteResp = nil
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
	c.lkRoom.SetOutput(ulaw.Encode(s))

	return sdpGenerateAnswer(offer, c.s.signalingIp, conn.LocalAddr().Port)
}

func (c *inboundCall) pinPrompt(ctx context.Context) {
	logger.Infow("Requesting Pin for SIP call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User)
	const pinLimit = 16
	c.playAudio(ctx, c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case <-ctx.Done():
			return
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

				logger.Infow("Checking Pin for SIP call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "pin", pin, "noPin", noPin)
				roomName, identity, wsUrl, token, requirePin, reject := c.s.dispatchRuleHandler(ctx, c.from.Address.User, c.to.Address.User, c.to.Address.Host, c.src, pin, noPin)
				if reject || requirePin || roomName == "" {
					logger.Infow("Rejecting call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "pin", pin, "noPin", noPin)
					c.playAudio(ctx, c.s.res.wrongPin)
					c.Close()
					return
				}
				c.joinRoom(ctx, roomName, identity, wsUrl, token)
				return
			}
			// Gather pin numbers
			pin += string(b)
			if len(pin) > pinLimit {
				c.playAudio(ctx, c.s.res.wrongPin)
				c.Close()
				return
			}
		}
	}
}

// close should only be called from handleInvite.
func (c *inboundCall) close() {
	if !c.done.CompareAndSwap(false, true) {
		return
	}
	logger.Infow("Closing inbound call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User)
	c.sendBye()
	c.closeMedia()
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.tag)
	c.s.cmu.Unlock()
}

func (c *inboundCall) Close() error {
	c.cancel()
	return nil
}

func (c *inboundCall) closeMedia() {
	c.audioHandler.Store(nil)
	if p := c.lkRoom; p != nil {
		p.Close()
		c.lkRoom = nil
	}
	if c.rtpConn != nil {
		c.rtpConn.Close()
		c.rtpConn = nil
	}
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

func (c *inboundCall) createLiveKitParticipant(ctx context.Context, roomName, participantIdentity, wsUrl, token string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	err := c.lkRoom.Connect(c.s.conf, roomName, participantIdentity, wsUrl, token)
	if err != nil {
		return err
	}
	local, err := c.lkRoom.NewParticipant()
	if err != nil {
		_ = c.lkRoom.Close()
		return err
	}

	// Decoding pipeline (SIP -> LK)
	law := ulaw.Decode(local)
	var h rtp.Handler = rtp.NewMediaStreamIn(law)
	c.audioHandler.Store(&h)

	return nil
}

func (c *inboundCall) joinRoom(ctx context.Context, roomName, identity, wsUrl, token string) {
	logger.Infow("Bridging SIP call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "roomName", roomName, "identity", identity)
	c.playAudio(ctx, c.s.res.roomJoin)
	if err := c.createLiveKitParticipant(ctx, roomName, identity, wsUrl, token); err != nil {
		logger.Errorw("Cannot create LiveKit participant", err, "tag", c.tag)
	}
}

func (c *inboundCall) playAudio(ctx context.Context, frames []media.PCM16Sample) {
	t := c.lkRoom.NewTrack()
	defer t.Close()
	t.PlayAudio(ctx, frames)
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
