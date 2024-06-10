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
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksip "github.com/livekit/protocol/sip"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/stats"
)

const (
	// audioBridgeMaxDelay delays sending audio for certain time, unless RTP packet is received.
	// This is done because of audio cutoff at the beginning of calls observed in the wild.
	audioBridgeMaxDelay = 1 * time.Second
)

var headerToLog = map[string]string{
	"X-Twilio-AccountSid": "twilioAccSID",
	"X-Twilio-CallSid":    "twilioCallSID",
}

func (s *Server) sipErrorOrDrop(tx sip.ServerTransaction, req *sip.Request) {
	if s.conf.HideInboundPort {
		tx.Terminate()
	} else {
		sipErrorResponse(tx, req)
	}
}

func (s *Server) handleInviteAuth(log logger.Logger, req *sip.Request, tx sip.ServerTransaction, from, username, password string) (ok bool) {
	if username == "" || password == "" {
		return true
	}
	if s.conf.HideInboundPort {
		// We will send password request anyway, so might as well signal that the progress is made.
		_ = tx.Respond(sip.NewResponseFromRequest(req, 100, "Processing", nil))
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
		log.Infow("Requesting inbound auth")
		inviteState.challenge = digest.Challenge{
			Realm:     UserAgent,
			Nonce:     fmt.Sprintf("%d", time.Now().UnixMicro()),
			Algorithm: "MD5",
		}

		res := sip.NewResponseFromRequest(req, 407, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("Proxy-Authenticate", inviteState.challenge.String()))
		_ = tx.Respond(res)
		return false
	}

	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	digCred, err := digest.Digest(&inviteState.challenge, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: cred.Username,
		Password: password,
	})

	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	if cred.Response != digCred.Response {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil))
		return false
	}

	return true
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	ctx := context.Background()
	s.mon.InviteReqRaw(stats.Inbound)
	callID := lksip.NewCallID()
	src := req.Source()
	log := s.log.WithValues(
		"callID", callID,
		"fromIP", src,
		"toIP", req.Destination(),
	)
	for hdr, name := range headerToLog {
		if h := req.GetHeader(hdr); h != nil {
			log = log.WithValues(name, h.Value())
		}
	}
	log.Debugw("invite received")

	if !s.conf.HideInboundPort {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 100, "Processing", nil))
	}
	tag, err := getTagValue(req)
	if err != nil {
		s.sipErrorOrDrop(tx, req)
		return
	}
	log = log.WithValues(
		"sipTag", tag,
	)

	from, ok := req.From()
	if !ok {
		s.sipErrorOrDrop(tx, req)
		return
	}

	to, ok := req.To()
	if !ok {
		s.sipErrorOrDrop(tx, req)
		return
	}

	cmon := s.mon.NewCall(stats.Inbound, from.Address.String(), to.Address.String())

	cmon.InviteReq()
	defer cmon.SessionDur()()
	joinDur := cmon.JoinDur()
	log = log.WithValues(
		"fromHost", from.Address.Host, "fromUser", from.Address.User,
		"toHost", to.Address.Host, "toUser", to.Address.User,
	)
	log.Infow("processing invite")

	username, password, drop, err := s.handler.GetAuthCredentials(ctx, from.Address.User, to.Address.User, to.Address.Host, src)
	if err != nil {
		cmon.InviteErrorShort("no-rule")
		log.Warnw("Rejecting inbound, doesn't match any Trunks", err)
		sipErrorResponse(tx, req)
		return
	} else if drop {
		cmon.InviteErrorShort("flood")
		log.Debugw("Dropping inbound flood")
		tx.Terminate()
		return
	}
	if !s.handleInviteAuth(log, req, tx, from.Address.User, username, password) {
		cmon.InviteErrorShort("unauthorized")
		// handleInviteAuth will generate the SIP Response as needed
		return
	}
	cmon.InviteAccept()
	if !s.conf.HideInboundPort {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))
	}

	call := s.newInboundCall(log, cmon, callID, tag, from, to, src)
	call.joinDur = joinDur
	call.handleInvite(call.ctx, req, tx, s.conf)
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getTagValue(req)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}

	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		c.log.Infow("BYE")
		c.Close()
	} else {
		s.log.Infow("BYE for non-existent call", "sipTag", tag)
		if s.sipUnhandled != nil {
			s.sipUnhandled(req, tx)
		}
	}
}

type inboundCall struct {
	s             *Server
	log           logger.Logger
	mon           *stats.CallMonitor
	id            string
	tag           string
	ctx           context.Context
	cancel        func()
	inviteReq     *sip.Request
	inviteResp    *sip.Response
	from          *sip.FromHeader
	to            *sip.ToHeader
	src           string
	rtpConn       *rtp.Conn
	audioCodec    rtp.AudioCodec
	audioHandler  atomic.Pointer[rtp.Handler]
	audioReceived atomic.Bool
	audioRecvChan chan struct{}
	audioType     byte
	dtmf          chan dtmf.Event // buffered
	lkRoom        *Room           // LiveKit room; only active after correct pin is entered
	callDur       func() time.Duration
	joinDur       func() time.Duration
	forwardDTMF   atomic.Bool
	done          atomic.Bool
}

func (s *Server) newInboundCall(log logger.Logger, mon *stats.CallMonitor, id, tag string, from *sip.FromHeader, to *sip.ToHeader, src string) *inboundCall {
	c := &inboundCall{
		s:             s,
		log:           log,
		mon:           mon,
		id:            id,
		tag:           tag,
		from:          from,
		to:            to,
		src:           src,
		audioRecvChan: make(chan struct{}),
		dtmf:          make(chan dtmf.Event, 10),
		lkRoom:        NewRoom(log), // we need it created earlier so that the audio mixer is available for pin prompts
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	s.cmu.Lock()
	s.activeCalls[tag] = c
	s.cmu.Unlock()
	return c
}

func (c *inboundCall) handleInvite(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, conf *config.Config) {
	c.mon.CallStart()
	defer c.mon.CallEnd()
	defer c.close(true, "other")
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could even learn that this number is not allowed and reject the call, or ask for pin if required.
	disp := c.s.handler.DispatchCall(ctx, &CallInfo{
		ID:         c.id,
		FromUser:   c.from.Address.User,
		ToUser:     c.to.Address.User,
		ToHost:     c.to.Address.Host,
		SrcAddress: c.src,
		Pin:        "",
		NoPin:      false,
	})
	if disp.TrunkID != "" {
		c.log = c.log.WithValues("sipTrunk", disp.TrunkID)
	}
	if disp.DispatchRuleID != "" {
		c.log = c.log.WithValues("sipRule", disp.DispatchRuleID)
	}
	switch disp.Result {
	default:
		c.log.Errorw("Rejecting inbound call", fmt.Errorf("unexpected dispatch result: %v", disp.Result))
		sipErrorResponse(tx, req)
		c.close(true, "unexpected-result")
		return
	case DispatchNoRuleDrop:
		c.log.Debugw("Rejecting inbound flood")
		tx.Terminate()
		c.close(false, "flood")
		return
	case DispatchNoRuleReject:
		c.log.Infow("Rejecting inbound call, doesn't match any Dispatch Rules")
		sipErrorResponse(tx, req)
		c.close(false, "no-dispatch")
		return
	case DispatchAccept, DispatchRequestPin:
		// continue
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMediaConn(req.Body(), conf)
	if err != nil {
		c.log.Errorw("Cannot start media", err)
		sipErrorResponse(tx, req)
		c.close(true, "media-failed")
		return
	}

	res := sip.NewResponseFromRequest(req, 200, "OK", answerData)

	// This will effectively redirect future SIP requests to this server instance (if signalingIp is not LB).
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: c.s.signalingIp, Port: c.s.conf.SIPPort}})

	// When behind LB, the source IP may be incorrect and/or the UDP "session" timeout may expire.
	// This is critical for sending new requests like BYE.
	//
	// Thus, instead of relying on LB, we will contact the source IP directly (should be the first Via).
	// BYE will also copy the same destination address from our response to INVITE.
	if h, ok := req.Via(); ok && h.Host != "" {
		port := 5060
		if h.Port != 0 {
			port = h.Port
		}
		res.SetDestination(fmt.Sprintf("%s:%d", h.Host, port))
	}

	res.AppendHeader(&contentTypeHeaderSDP)
	if err = tx.Respond(res); err != nil {
		c.log.Errorw("Cannot respond to INVITE", err)
		return
	}
	c.inviteReq = req
	c.inviteResp = res

	// Wait for either a first RTP packet or a predefined delay.
	//
	// If the delay kicks in earlier than the caller is ready, they might miss some audio packets.
	//
	// On the other hand, if we always wait for RTP, it might be harder to diagnose firewall/routing issues.
	// In that case both sides will hear nothing, instead of only one side having issues.
	//
	// Thus, we wait at most a fixed amount of time before bridging audio.

	// We own this goroutine, so can freely block.
	delay := time.NewTimer(audioBridgeMaxDelay)
	select {
	case <-ctx.Done():
		delay.Stop()
		c.close(false, "hangup")
		return
	case <-c.audioRecvChan:
		delay.Stop()
	case <-delay.C:
	}
	switch disp.Result {
	default:
		c.log.Errorw("Rejecting inbound call", fmt.Errorf("unreachable dispatch result path: %v", disp.Result))
		sipErrorResponse(tx, req)
		c.close(true, "unreachable-path")
		return
	case DispatchRequestPin:
		c.pinPrompt(ctx)
	case DispatchAccept:
		c.joinRoom(ctx, disp.RoomName, disp.Identity, disp.Name, disp.Metadata, disp.WsUrl, disp.Token)
	}
	// Wait for the caller to terminate the call.
	select {
	case <-ctx.Done():
		c.close(false, "hangup")
	case <-c.lkRoom.Closed():
		c.close(false, "removed")
	}
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
	c.inviteReq = nil
	c.inviteResp = nil
	tx, err := c.s.sipSrv.TransactionLayer().Request(bye)
	if err != nil {
		return
	}
	defer tx.Terminate()
	r, err := sipResponse(tx)
	if err != nil {
		return
	}
	if r.StatusCode == 200 {
		_ = c.s.sipSrv.TransportLayer().WriteMsg(sip.NewAckRequest(bye, r, nil))
	}
}

func (c *inboundCall) runMediaConn(offerData []byte, conf *config.Config) (answerData []byte, _ error) {
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(offerData); err != nil {
		return nil, err
	}
	res, err := sdpGetAudioCodec(offer)
	if err != nil {
		return nil, err
	}
	c.log.Infow("Using codecs",
		"audioCodec", res.Audio.Info().SDPName, "audioRTP", res.AudioType,
		"dtmfRTP", res.DTMFType,
	)

	conn := rtp.NewConn(func() {
		c.close(true, "media-timeout")
	})
	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(c.mon, "", nil))
	mux.Register(res.AudioType, newRTPStatsHandler(c.mon, res.Audio.Info().SDPName, rtp.HandlerFunc(c.handleAudio)))
	if res.DTMFType != 0 {
		mux.Register(res.DTMFType, newRTPStatsHandler(c.mon, dtmf.SDPName, rtp.HandlerFunc(c.handleDTMF)))
	}
	conn.OnRTP(mux)
	if dst := sdpGetAudioDest(offer); dst != nil {
		conn.SetDestAddr(dst)
	}
	if err := conn.ListenAndServe(conf.RTPPort.Start, conf.RTPPort.End, "0.0.0.0"); err != nil {
		return nil, err
	}
	c.log.Debugw("begin listening on UDP", "port", conn.LocalAddr().Port)
	c.rtpConn = conn
	c.audioCodec = res.Audio
	c.audioType = res.AudioType

	// Encoding pipeline (LK -> SIP)
	// Need to be created earlier to send the pin prompts.
	s := rtp.NewSeqWriter(newRTPStatsWriter(c.mon, "audio", conn))
	sa := s.NewStream(c.audioType)
	audio := c.audioCodec.EncodeRTP(sa)
	c.lkRoom.SetOutput(audio)

	return sdpGenerateAnswer(offer, c.s.signalingIp, conn.LocalAddr().Port, res)
}

func (c *inboundCall) pinPrompt(ctx context.Context) {
	c.log.Infow("Requesting Pin for SIP call")
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
			if b.Digit == 0 {
				continue // unrecognized
			}
			if b.Digit == '#' {
				// End of the pin
				noPin = pin == ""

				c.log.Infow("Checking Pin for SIP call", "pin", pin, "noPin", noPin)
				disp := c.s.handler.DispatchCall(ctx, &CallInfo{
					ID:         c.id,
					FromUser:   c.from.Address.User,
					ToUser:     c.to.Address.User,
					ToHost:     c.to.Address.Host,
					SrcAddress: c.src,
					Pin:        pin,
					NoPin:      noPin,
				})
				if disp.TrunkID != "" {
					c.log = c.log.WithValues("sipTrunk", disp.TrunkID)
				}
				if disp.DispatchRuleID != "" {
					c.log = c.log.WithValues("sipRule", disp.DispatchRuleID)
				}
				if disp.Result != DispatchAccept || disp.RoomName == "" {
					c.log.Infow("Rejecting call", "pin", pin, "noPin", noPin)
					c.playAudio(ctx, c.s.res.wrongPin)
					c.close(false, "wrong-pin")
					return
				}
				c.playAudio(ctx, c.s.res.roomJoin)
				c.joinRoom(ctx, disp.RoomName, disp.Identity, disp.Name, disp.Metadata, disp.WsUrl, disp.Token)
				return
			}
			// Gather pin numbers
			pin += string(b.Digit)
			if len(pin) > pinLimit {
				c.playAudio(ctx, c.s.res.wrongPin)
				c.close(false, "wrong-pin")
				return
			}
		}
	}
}

// close should only be called from handleInvite.
func (c *inboundCall) close(error bool, reason string) {
	if !c.done.CompareAndSwap(false, true) {
		return
	}
	c.mon.CallTerminate(reason)
	if error {
		c.log.Warnw("Closing inbound call with error", nil, "reason", reason)
	} else {
		c.log.Infow("Closing inbound call", "reason", reason)
	}
	c.closeMedia()
	c.sendBye()
	if c.callDur != nil {
		c.callDur()
	}
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.tag)
	c.s.cmu.Unlock()
	c.cancel()
}

func (c *inboundCall) Close() error {
	c.cancel()
	return nil
}

func (c *inboundCall) closeMedia() {
	c.audioHandler.Store(nil)
	c.lkRoom.Close()
	if c.rtpConn != nil {
		c.rtpConn.Close()
		c.rtpConn = nil
	}
}

func (c *inboundCall) handleAudio(p *rtp.Packet) error {
	if c.audioReceived.CompareAndSwap(false, true) {
		close(c.audioRecvChan)
	}
	if h := c.audioHandler.Load(); h != nil {
		return (*h).HandleRTP(p)
	}
	return nil
}

func (c *inboundCall) createLiveKitParticipant(ctx context.Context, roomName, parIdentity, parName, parMeta, wsUrl, token string) error {
	c.forwardDTMF.Store(true)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	err := c.lkRoom.Connect(c.s.conf, roomName, parIdentity, parName, parMeta, wsUrl, token)
	if err != nil {
		return err
	}
	local, err := c.lkRoom.NewParticipantTrack()
	if err != nil {
		_ = c.lkRoom.Close()
		return err
	}

	// Decoding pipeline (SIP -> LK)
	var h rtp.Handler = c.audioCodec.DecodeRTP(local, c.audioType)
	c.audioHandler.Store(&h)

	return nil
}

func (c *inboundCall) joinRoom(ctx context.Context, roomName, identity, name, meta, wsUrl, token string) {
	if c.joinDur != nil {
		c.joinDur()
	}
	c.callDur = c.mon.CallDur()
	c.log = c.log.WithValues("roomName", roomName, "identity", identity, "name", name)
	c.log.Infow("Bridging SIP call")
	if err := c.createLiveKitParticipant(ctx, roomName, identity, name, meta, wsUrl, token); err != nil {
		c.log.Errorw("Cannot create LiveKit participant", err)
		c.close(true, "participant-failed")
	}
}

func (c *inboundCall) playAudio(ctx context.Context, frames []media.PCM16Sample) {
	t := c.lkRoom.NewTrack()
	defer t.Close()
	t.PlayAudio(ctx, frames)
}

func (c *inboundCall) handleDTMF(p *rtp.Packet) error {
	tone, ok := dtmf.DecodeRTP(p)
	if !ok {
		return nil
	}
	if c.forwardDTMF.Load() {
		_ = c.lkRoom.SendData(&livekit.SipDTMF{
			Code:  uint32(tone.Code),
			Digit: string([]byte{tone.Digit}),
		}, lksdk.WithDataPublishReliable(true))
		return nil
	}
	// We should have enough buffer here.
	select {
	case c.dtmf <- tone:
	default:
	}
	return nil
}
