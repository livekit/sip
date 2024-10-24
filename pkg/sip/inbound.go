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
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	lksip "github.com/livekit/protocol/sip"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/res"
)

const (
	// audioBridgeMaxDelay delays sending audio for certain time, unless RTP packet is received.
	// This is done because of audio cutoff at the beginning of calls observed in the wild.
	audioBridgeMaxDelay = 1 * time.Second
)

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
	src, err := netip.ParseAddrPort(req.Source())
	if err != nil {
		tx.Terminate()
		s.log.Errorw("cannot parse source IP", err, "fromIP", src)
		return
	}
	callID := lksip.NewCallID()
	log := s.log.WithValues(
		"callID", callID,
		"fromIP", src.Addr(),
		"toIP", req.Destination(),
	)

	tr := transportFromReq(req)
	cc := s.newInbound(LocalTag(callID), s.ContactURI(tr), req, tx)
	log = LoggerWithParams(log, cc)
	log = LoggerWithHeaders(log, cc)
	log.Infow("processing invite")

	if err := cc.ValidateInvite(); err != nil {
		if s.conf.HideInboundPort {
			cc.Drop()
		} else {
			cc.RespondAndDrop(sip.StatusBadRequest, "Bad request")
		}
		return
	}
	ctx, span := tracer.Start(ctx, "Server.onInvite")
	defer span.End()

	from, to := cc.From(), cc.To()

	cmon := s.mon.NewCall(stats.Inbound, from.Host, cc.To().Host)
	cmon.InviteReq()
	defer cmon.SessionDur()()
	joinDur := cmon.JoinDur()

	if !s.conf.HideInboundPort {
		cc.Processing()
	}

	r, err := s.handler.GetAuthCredentials(ctx, callID, from.User, to.User, to.Host, src.Addr())
	if err != nil {
		cmon.InviteErrorShort("auth-error")
		log.Warnw("Rejecting inbound, auth check failed", err)
		cc.RespondAndDrop(sip.StatusServiceUnavailable, "Try again later")
		return
	}
	if r.ProjectID != "" {
		log = log.WithValues("projectID", r.ProjectID)
	}
	if r.TrunkID != "" {
		log = log.WithValues("sipTrunk", r.TrunkID)
	}
	switch r.Result {
	case AuthDrop:
		cmon.InviteErrorShort("flood")
		log.Debugw("Dropping inbound flood")
		cc.Drop()
		return
	case AuthNotFound:
		cmon.InviteErrorShort("no-rule")
		log.Warnw("Rejecting inbound, doesn't match any Trunks", nil)
		cc.RespondAndDrop(sip.StatusNotFound, "Does not match any SIP Trunks")
		return
	case AuthPassword:
		if s.conf.HideInboundPort {
			// We will send password request anyway, so might as well signal that the progress is made.
			cc.Processing()
		}
		if !s.handleInviteAuth(log, req, tx, from.User, r.Username, r.Password) {
			cmon.InviteErrorShort("unauthorized")
			// handleInviteAuth will generate the SIP Response as needed
			return
		}
		fallthrough
	case AuthAccept:
		// ok
	}

	call := s.newInboundCall(log, cmon, cc, src, nil)
	call.joinDur = joinDur
	call.handleInvite(call.ctx, req, r.TrunkID, s.conf)
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getFromTag(req)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
		return
	}

	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.log.Infow("BYE")
		c.cc.AcceptBye(req, tx)
		_ = c.Close()
		return
	}
	ok := false
	if s.sipUnhandled != nil {
		ok = s.sipUnhandled(req, tx)
	}
	if !ok {
		s.log.Infow("BYE for non-existent call", "sipTag", tag)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call does not exist", nil))
	}
}

func (s *Server) onNotify(req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getFromTag(req)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "", nil))
		return
	}

	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.log.Infow("NOTIFY")
		err := c.cc.handleNotify(req, tx)

		code, msg := sipCodeAndMessageFromError(err)

		tx.Respond(sip.NewResponseFromRequest(req, code, msg, nil))

		return
	}
	ok := false
	if s.sipUnhandled != nil {
		ok = s.sipUnhandled(req, tx)
	}
	if !ok {
		s.log.Infow("NOTIFY for non-existent call")
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call does not exist", nil))
	}
}

type inboundCall struct {
	s           *Server
	log         logger.Logger
	cc          *sipInbound
	mon         *stats.CallMonitor
	extraAttrs  map[string]string
	ctx         context.Context
	cancel      func()
	src         netip.AddrPort
	media       *MediaPort
	dtmf        chan dtmf.Event // buffered
	lkRoom      *Room           // LiveKit room; only active after correct pin is entered
	callDur     func() time.Duration
	joinDur     func() time.Duration
	forwardDTMF atomic.Bool
	done        atomic.Bool
}

func (s *Server) newInboundCall(
	log logger.Logger,
	mon *stats.CallMonitor,
	cc *sipInbound,
	src netip.AddrPort,
	extra map[string]string,
) *inboundCall {

	extra = HeadersToAttrs(extra, nil, cc)
	c := &inboundCall{
		s:          s,
		log:        log,
		mon:        mon,
		cc:         cc,
		src:        src,
		extraAttrs: extra,
		dtmf:       make(chan dtmf.Event, 10),
		lkRoom:     NewRoom(log), // we need it created earlier so that the audio mixer is available for pin prompts
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	s.cmu.Lock()
	s.activeCalls[cc.Tag()] = c
	s.byLocal[cc.ID()] = c
	s.cmu.Unlock()
	return c
}

func (c *inboundCall) handleInvite(ctx context.Context, req *sip.Request, trunkID string, conf *config.Config) {
	c.mon.InviteAccept()
	c.mon.CallStart()
	defer c.mon.CallEnd()
	defer c.close(true, callDropped, "other")

	c.cc.StartRinging()
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could even learn that this number is not allowed and reject the call, or ask for pin if required.
	disp := c.s.handler.DispatchCall(ctx, &CallInfo{
		TrunkID:    trunkID,
		ID:         string(c.cc.ID()),
		FromUser:   c.cc.From().User,
		ToUser:     c.cc.To().User,
		ToHost:     c.cc.To().Host,
		SrcAddress: c.src.Addr(),
		Pin:        "",
		NoPin:      false,
	})
	if disp.ProjectID != "" {
		c.log = c.log.WithValues("projectID", disp.ProjectID)
	}
	if disp.TrunkID != "" {
		c.log = c.log.WithValues("sipTrunk", disp.TrunkID)
	}
	if disp.DispatchRuleID != "" {
		c.log = c.log.WithValues("sipRule", disp.DispatchRuleID)
	}
	var pinPrompt bool
	switch disp.Result {
	default:
		c.log.Errorw("Rejecting inbound call", fmt.Errorf("unexpected dispatch result: %v", disp.Result))
		c.cc.RespondAndDrop(sip.StatusNotImplemented, "")
		c.close(true, callDropped, "unexpected-result")
		return
	case DispatchNoRuleDrop:
		c.log.Debugw("Rejecting inbound flood")
		c.cc.Drop()
		c.close(false, callFlood, "flood")
		return
	case DispatchNoRuleReject:
		c.log.Infow("Rejecting inbound call, doesn't match any Dispatch Rules")
		c.cc.RespondAndDrop(sip.StatusNotFound, "Does not match Trunks or Dispatch Rules")
		c.close(false, callDropped, "no-dispatch")
		return
	case DispatchAccept:
		pinPrompt = false
	case DispatchRequestPin:
		pinPrompt = true
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMediaConn(req.Body(), conf, disp.EnabledFeatures)
	if err != nil {
		c.log.Errorw("Cannot start media", err)
		c.cc.RespondAndDrop(sip.StatusInternalServerError, "")
		c.close(true, callDropped, "media-failed")
		return
	}
	acceptCall := func() bool {
		c.log.Infow("Accepting the call", "headers", disp.Headers)
		if err = c.cc.Accept(ctx, answerData, disp.Headers); err != nil {
			c.log.Errorw("Cannot respond to INVITE", err)
			return false
		}
		c.media.EnableTimeout(true)
		if !c.waitMedia(ctx) {
			return false
		}
		return true
	}

	if pinPrompt {
		// Accept the call first on the SIP side, so that we can send audio prompts.
		if !acceptCall() {
			return // already sent a response
		}
		var ok bool
		disp, ok = c.pinPrompt(ctx, trunkID)
		if !ok {
			return // already sent a response
		}
	}
	if len(disp.HeadersToAttributes) != 0 {
		p := &disp.Room.Participant
		if p.Attributes == nil {
			p.Attributes = make(map[string]string)
		}
		headers := c.cc.RemoteHeaders()
		for hdr, attr := range disp.HeadersToAttributes {
			if h := headers.GetHeader(hdr); h != nil {
				p.Attributes[attr] = h.Value()
			}
		}
	}
	if disp.MaxCallDuration <= 0 || disp.MaxCallDuration > maxCallDuration {
		disp.MaxCallDuration = maxCallDuration
	}
	if disp.RingingTimeout <= 0 {
		disp.RingingTimeout = defaultRingingTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, disp.MaxCallDuration)
	defer cancel()
	if !c.joinRoom(ctx, disp.Room) {
		return // already sent a response
	}
	// Publish our own track.
	if err := c.publishTrack(); err != nil {
		c.log.Errorw("Cannot publish track", err)
		c.close(true, callDropped, "publish-failed")
		return
	}
	c.lkRoom.Subscribe()
	if !pinPrompt {
		c.log.Infow("Waiting for track subscription(s)")
		// For dispatches without pin, we first wait for LK participant to become available,
		// and also for at least one track subscription. In the meantime we keep ringing.
		if !c.waitSubscribe(ctx, disp.RingingTimeout) {
			return // already sent a response
		}
		if !acceptCall() {
			return // already sent a response
		}
	}
	// Wait for the caller to terminate the call.
	select {
	case <-ctx.Done():
		c.closeWithHangup()
	case <-c.lkRoom.Closed():
		c.close(false, callDropped, "removed")
	case <-c.media.Timeout():
		c.closeWithTimeout()
	}
}

func (c *inboundCall) runMediaConn(offerData []byte, conf *config.Config, features []rpc.SIPFeature) (answerData []byte, _ error) {
	c.mon.SDPSize(len(offerData), true)
	c.log.Debugw("SDP offer", "sdp", string(offerData))

	mp, err := NewMediaPort(c.log, c.mon, &MediaConfig{
		IP:                  c.s.signalingIp,
		Ports:               conf.RTPPort,
		MediaTimeoutInitial: c.s.conf.MediaTimeoutInitial,
		MediaTimeout:        c.s.conf.MediaTimeout,
	}, RoomSampleRate)
	if err != nil {
		return nil, err
	}
	c.media = mp
	c.media.EnableTimeout(false) // enabled once we accept the call
	c.media.SetDTMFAudio(conf.AudioDTMF)

	answerData, mconf, err := mp.SetOffer(offerData)
	if err != nil {
		return nil, err
	}
	c.mon.SDPSize(len(answerData), false)
	c.log.Debugw("SDP answer", "sdp", string(answerData))

	mconf.Processor = c.s.handler.GetMediaProcessor(features)
	if err = c.media.SetConfig(mconf); err != nil {
		return nil, err
	}
	if mconf.DTMFType != 0 {
		c.media.HandleDTMF(c.handleDTMF)
	}

	// Must be set earlier to send the pin prompts.
	if w := c.lkRoom.SwapOutput(c.media.GetAudioWriter()); w != nil {
		_ = w.Close()
	}
	if mconf.DTMFType != 0 {
		c.lkRoom.SetDTMFOutput(c.media)
	}
	return answerData, nil
}

func (c *inboundCall) waitMedia(ctx context.Context) bool {
	// Wait for either a first RTP packet or a predefined delay.
	//
	// If the delay kicks in earlier than the caller is ready, they might miss some audio packets.
	//
	// On the other hand, if we always wait for RTP, it might be harder to diagnose firewall/routing issues.
	// In that case both sides will hear nothing, instead of only one side having issues.
	//
	// Thus, we wait at most a fixed amount of time before bridging audio.

	delay := time.NewTimer(audioBridgeMaxDelay)
	defer delay.Stop()
	select {
	case <-c.cc.Cancelled():
		c.closeWithCancelled()
		return false
	case <-ctx.Done():
		c.closeWithHangup()
		return false
	case <-c.lkRoom.Closed():
		c.closeWithHangup()
		return false
	case <-c.media.Timeout():
		c.closeWithTimeout()
		return false
	case <-c.media.Received():
	case <-delay.C:
	}
	return true
}

func (c *inboundCall) waitSubscribe(ctx context.Context, timeout time.Duration) bool {
	ctx, span := tracer.Start(ctx, "inboundCall.waitSubscribe")
	defer span.End()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.cc.Cancelled():
		c.closeWithCancelled()
		return false
	case <-ctx.Done():
		c.closeWithHangup()
		return false
	case <-c.lkRoom.Closed():
		c.closeWithHangup()
		return false
	case <-c.media.Timeout():
		c.closeWithTimeout()
		return false
	case <-timer.C:
		c.close(false, callDropped, "cannot-subscribe")
		return false
	case <-c.lkRoom.Subscribed():
		return true
	}
}

func (c *inboundCall) pinPrompt(ctx context.Context, trunkID string) (disp CallDispatch, _ bool) {
	ctx, span := tracer.Start(ctx, "inboundCall.pinPrompt")
	defer span.End()
	c.log.Infow("Requesting Pin for SIP call")
	const pinLimit = 16
	c.playAudio(ctx, c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case <-c.cc.Cancelled():
			c.closeWithCancelled()
			return disp, false
		case <-ctx.Done():
			c.closeWithHangup()
			return disp, false
		case <-c.media.Timeout():
			c.closeWithTimeout()
			return disp, false
		case b, ok := <-c.dtmf:
			if !ok {
				c.Close()
				return disp, false
			}
			if b.Digit == 0 {
				continue // unrecognized
			}
			if b.Digit == '#' {
				// End of the pin
				noPin = pin == ""

				c.log.Infow("Checking Pin for SIP call", "pin", pin, "noPin", noPin)
				disp = c.s.handler.DispatchCall(ctx, &CallInfo{
					TrunkID:    trunkID,
					ID:         string(c.cc.ID()),
					FromUser:   c.cc.From().User,
					ToUser:     c.cc.To().User,
					ToHost:     c.cc.To().Host,
					SrcAddress: c.src.Addr(),
					Pin:        pin,
					NoPin:      noPin,
				})
				if disp.ProjectID != "" {
					c.log = c.log.WithValues("projectID", disp.ProjectID)
				}
				if disp.TrunkID != "" {
					c.log = c.log.WithValues("sipTrunk", disp.TrunkID)
				}
				if disp.DispatchRuleID != "" {
					c.log = c.log.WithValues("sipRule", disp.DispatchRuleID)
				}
				if disp.Result != DispatchAccept || disp.Room.RoomName == "" {
					c.log.Infow("Rejecting call", "pin", pin, "noPin", noPin)
					c.playAudio(ctx, c.s.res.wrongPin)
					c.close(false, callDropped, "wrong-pin")
					return disp, false
				}
				c.playAudio(ctx, c.s.res.roomJoin)
				return disp, true
			}
			// Gather pin numbers
			pin += string(b.Digit)
			if len(pin) > pinLimit {
				c.playAudio(ctx, c.s.res.wrongPin)
				c.close(false, callDropped, "wrong-pin")
				return disp, false
			}
		}
	}
}

// close should only be called from handleInvite.
func (c *inboundCall) close(error bool, status CallStatus, reason string) {
	if !c.done.CompareAndSwap(false, true) {
		return
	}
	c.setStatus(status)
	c.mon.CallTerminate(reason)
	if error {
		c.log.Warnw("Closing inbound call with error", nil, "reason", reason)
	} else {
		c.log.Infow("Closing inbound call", "reason", reason)
	}
	if status != callFlood {
		defer c.log.Infow("Inbound call closed", "reason", reason)
	}

	c.closeMedia()
	c.cc.Close()
	if c.callDur != nil {
		c.callDur()
	}
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.cc.Tag())
	delete(c.s.byLocal, c.cc.ID())
	c.s.cmu.Unlock()

	c.s.DeregisterTransferSIPParticipant(c.cc.ID())

	c.cancel()
}

func (c *inboundCall) closeWithTimeout() {
	c.close(true, callDropped, "media-timeout")
}

func (c *inboundCall) closeWithCancelled() {
	c.close(false, CallHangup, "cancelled")
}

func (c *inboundCall) closeWithHangup() {
	c.close(false, CallHangup, "hangup")
}

func (c *inboundCall) Close() error {
	c.cancel()
	return nil
}

func (c *inboundCall) closeMedia() {
	c.lkRoom.Close()
	if c.media != nil {
		c.media.Close()
	}
}

func (c *inboundCall) setStatus(v CallStatus) {
	attr := v.Attribute()
	if attr == "" {
		return
	}
	if c.lkRoom == nil {
		return
	}
	r := c.lkRoom.Room()
	if r == nil || r.LocalParticipant == nil {
		return
	}

	r.LocalParticipant.SetAttributes(map[string]string{
		livekit.AttrSIPCallStatus: attr,
	})
}

func (c *inboundCall) createLiveKitParticipant(ctx context.Context, rconf RoomConfig) error {
	ctx, span := tracer.Start(ctx, "inboundCall.createLiveKitParticipant")
	defer span.End()
	partConf := &rconf.Participant
	if partConf.Attributes == nil {
		partConf.Attributes = make(map[string]string)
	}
	for k, v := range c.extraAttrs {
		partConf.Attributes[k] = v
	}
	partConf.Attributes[livekit.AttrSIPCallStatus] = CallActive.Attribute()
	c.forwardDTMF.Store(true)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	err := c.s.RegisterTransferSIPParticipant(LocalTag(c.cc.ID()), c)
	if err != nil {
		return err
	}

	err = c.lkRoom.Connect(c.s.conf, rconf)
	if err != nil {
		return err
	}
	return nil
}

func (c *inboundCall) publishTrack() error {
	local, err := c.lkRoom.NewParticipantTrack(RoomSampleRate)
	if err != nil {
		_ = c.lkRoom.Close()
		return err
	}
	c.media.WriteAudioTo(local)
	return nil
}

func (c *inboundCall) joinRoom(ctx context.Context, rconf RoomConfig) bool {
	if c.joinDur != nil {
		c.joinDur()
	}
	c.callDur = c.mon.CallDur()
	c.log = c.log.WithValues(
		"room", rconf.RoomName,
		"participant", rconf.Participant.Identity,
		"participantName", rconf.Participant.Name,
	)
	c.log.Infow("Joining room")
	if err := c.createLiveKitParticipant(ctx, rconf); err != nil {
		c.log.Errorw("Cannot create LiveKit participant", err)
		c.close(true, callDropped, "participant-failed")
		return false
	}
	return true
}

func (c *inboundCall) playAudio(ctx context.Context, frames []media.PCM16Sample) {
	t := c.lkRoom.NewTrack()
	defer t.Close()

	sampleRate := res.SampleRate
	if t.SampleRate() != sampleRate {
		frames = slices.Clone(frames)
		for i := range frames {
			frames[i] = media.Resample(nil, t.SampleRate(), frames[i], sampleRate)
		}
	}
	_ = media.PlayAudio[media.PCM16Sample](ctx, t, rtp.DefFrameDur, frames)
}

func (c *inboundCall) handleDTMF(tone dtmf.Event) {
	if c.forwardDTMF.Load() {
		_ = c.lkRoom.SendData(&livekit.SipDTMF{
			Code:  uint32(tone.Code),
			Digit: string([]byte{tone.Digit}),
		}, lksdk.WithDataPublishReliable(true))
		return
	}
	// We should have enough buffer here.
	select {
	case c.dtmf <- tone:
	default:
	}
}

func (c *inboundCall) transferCall(ctx context.Context, transferTo string) error {
	err := c.cc.TransferCall(ctx, transferTo)
	if err != nil {
		c.log.Infow("inbound call failed to transfer", "error", err, "transferTo", transferTo)
		return err
	}

	c.log.Infow("inbound call tranferred", "transferTo", transferTo)

	// Give time for the peer to hang up first, but hang up ourselves if this doesn't happen within 1 second
	time.AfterFunc(referByeTimeout, func() { c.Close() })

	return nil

}

func (s *Server) newInbound(id LocalTag, contact URI, invite *sip.Request, inviteTx sip.ServerTransaction) *sipInbound {
	c := &sipInbound{
		s:        s,
		id:       id,
		invite:   invite,
		inviteTx: inviteTx,
		contact: &sip.ContactHeader{
			Address: *contact.GetContactURI(),
		},
		cancelled: make(chan struct{}),
		referDone: make(chan error), // Do not buffer the channel to avoid reading a result for an old request
	}
	c.from, _ = invite.From()
	if c.from != nil {
		c.tag, _ = getTagFrom(c.from.Params)
	}
	c.to, _ = invite.To()
	h, _ := invite.CSeq()
	if h != nil {
		c.nextRequestCSeq = h.SeqNo + 1
	}

	return c
}

type sipInbound struct {
	s         *Server
	id        LocalTag
	tag       RemoteTag
	invite    *sip.Request
	inviteTx  sip.ServerTransaction
	contact   *sip.ContactHeader
	cancelled chan struct{}
	from      *sip.FromHeader
	to        *sip.ToHeader
	referDone chan error

	mu              sync.RWMutex
	inviteOk        *sip.Response
	nextRequestCSeq uint32
	referCseq       uint32
	ringing         chan struct{}
}

func (c *sipInbound) ValidateInvite() error {
	if c.from == nil {
		return errors.New("no From header")
	}
	if c.to == nil {
		return errors.New("no To header")
	}
	if c.tag == "" {
		return errors.New("no tag in From")
	}
	return nil
}

func (c *sipInbound) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
}

func (c *sipInbound) drop() {
	c.stopRinging()
	if c.inviteTx != nil {
		c.inviteTx.Terminate()
	}
	c.inviteTx = nil
	c.invite = nil
	c.inviteOk = nil
	c.nextRequestCSeq = 0
}

func (c *sipInbound) respond(status sip.StatusCode, reason string) {
	if c.inviteTx == nil {
		return
	}

	resp := sip.NewResponseFromRequest(c.invite, status, reason, nil)
	resp.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	_ = c.inviteTx.Respond(resp)
}

func (c *sipInbound) RespondAndDrop(status sip.StatusCode, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopRinging()
	c.respond(status, reason)
	c.drop()
}

func (c *sipInbound) From() sip.Uri {
	if c.from == nil {
		return sip.Uri{}
	}
	return c.from.Address
}

func (c *sipInbound) To() sip.Uri {
	if c.to == nil {
		return sip.Uri{}
	}
	return c.to.Address
}

func (c *sipInbound) ID() LocalTag {
	return c.id
}

func (c *sipInbound) Tag() RemoteTag {
	return c.tag
}

func (c *sipInbound) RemoteHeaders() Headers {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.invite == nil {
		return nil
	}
	return c.invite.Headers()
}

func (c *sipInbound) Processing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.respond(sip.StatusTrying, "Processing")
}

func (c *sipInbound) sendRinging() {
	c.respond(sip.StatusRinging, "Ringing")
}

func (c *sipInbound) attachTag() {
	// Set the SIP tag for following requests from us to remote (e.g. BYE).
	c.to.Params["tag"] = string(c.id)
}

func (c *sipInbound) StartRinging() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attachTag()
	c.sendRinging()
	stop := make(chan struct{})
	c.ringing = stop
	tx := c.inviteTx
	cancels := tx.Cancels()
	go func() {
		// TODO: check spec for the exact interval
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case r := <-cancels:
				close(c.cancelled)
				_ = tx.Respond(sip.NewResponseFromRequest(r, sip.StatusOK, "OK", nil))
				c.mu.Lock()
				c.drop()
				c.mu.Unlock()
				return
			case <-ticker.C:
			}
			c.mu.Lock()
			c.sendRinging()
			c.mu.Unlock()
		}
	}()
}

func (c *sipInbound) stopRinging() {
	if c.ringing != nil {
		close(c.ringing)
		c.ringing = nil
	}
}

func (c *sipInbound) Cancelled() <-chan struct{} {
	return c.cancelled
}

func (c *sipInbound) setDestFromVia(r *sip.Response) {
	// When behind LB, the source IP may be incorrect and/or the UDP "session" timeout may expire.
	// This is critical for sending new requests like BYE.
	//
	// Thus, instead of relying on LB, we will contact the source IP directly (should be the first Via).
	// BYE will also copy the same destination address from our response to INVITE.
	if h, ok := c.invite.Via(); ok && h.Host != "" {
		port := 5060
		if h.Port != 0 {
			port = h.Port
		}
		r.SetDestination(fmt.Sprintf("%s:%d", h.Host, port))
	}
}

func (c *sipInbound) Accept(ctx context.Context, sdpData []byte, headers map[string]string) error {
	ctx, span := tracer.Start(ctx, "sipInbound.Accept")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteTx == nil {
		return errors.New("call already rejected")
	}
	r := sip.NewResponseFromRequest(c.invite, 200, "OK", sdpData)

	// This will effectively redirect future SIP requests to this server instance (if host address is not LB).
	r.AppendHeader(c.contact)

	c.setDestFromVia(r)

	r.AppendHeader(&contentTypeHeaderSDP)
	for k, v := range headers {
		r.AppendHeader(sip.NewHeader(k, v))
	}
	c.stopRinging()
	if err := c.inviteTx.Respond(r); err != nil {
		return err
	}
	c.inviteOk = r
	c.inviteTx = nil // accepted
	return nil
}

func (c *sipInbound) AcceptBye(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	c.drop() // mark as closed
}

func (c *sipInbound) swapSrcDst(req *sip.Request) {
	if contact, ok := c.invite.Contact(); ok {
		req.Recipient = &contact.Address
	} else {
		req.Recipient = &c.from.Address
	}
	req.SetSource(c.inviteOk.Source())
	req.SetDestination(c.inviteOk.Destination())
	req.RemoveHeader("From")
	req.AppendHeader((*sip.FromHeader)(c.to))
	req.RemoveHeader("To")
	req.AppendHeader((*sip.ToHeader)(c.from))
	if route, ok := req.RecordRoute(); ok {
		req.RemoveHeader("Record-Route")
		req.AppendHeader(&sip.RouteHeader{Address: route.Address})
	}
}

func (c *sipInbound) setCSeq(req *sip.Request) {
	setCSeq(req, c.nextRequestCSeq)

	c.nextRequestCSeq++
}

func (c *sipInbound) sendBye() {
	if c.inviteOk == nil {
		return // call wasn't established
	}
	if c.invite == nil {
		return // rejected or closed
	}
	ctx := context.Background()
	_, span := tracer.Start(ctx, "sipInbound.sendBye")
	defer span.End()
	// This function is for clients, so we need to swap src and dest
	r := sip.NewByeRequest(c.invite, c.inviteOk, nil)

	c.setCSeq(r)
	c.swapSrcDst(r)
	c.drop()
	sendAndACK(ctx, c, r)
}

func (c *sipInbound) sendRejected() {
	if c.inviteOk != nil {
		return // call already established
	}
	if c.inviteTx == nil {
		return // rejected or closed
	}
	_, span := tracer.Start(context.Background(), "sipInbound.sendRejected")
	defer span.End()

	r := sip.NewResponseFromRequest(c.invite, sip.StatusBusyHere, "Rejected", nil)
	c.setDestFromVia(r)
	_ = c.inviteTx.Respond(r)
	c.drop()
}

func (c *sipInbound) WriteRequest(req *sip.Request) error {
	return c.s.sipSrv.TransportLayer().WriteMsg(req)
}

func (c *sipInbound) Transaction(req *sip.Request) (sip.ClientTransaction, error) {
	return c.s.sipSrv.TransactionLayer().Request(req)
}

func (c *sipInbound) newReferReq(transferTo string) (*sip.Request, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.invite == nil || c.inviteOk == nil {
		return nil, psrpc.NewErrorf(psrpc.FailedPrecondition, "can't transfer non established call") // call wasn't established
	}

	from, _ := c.invite.From()
	if from == nil {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "no From URI in invite")
	}

	// This will effectively redirect future SIP requests to this server instance (if host address is not LB).
	req := NewReferRequest(c.invite, c.inviteOk, c.contact, transferTo)
	c.setCSeq(req)
	c.swapSrcDst(req)

	cseq, _ := req.CSeq()
	if cseq == nil {
		return nil, psrpc.NewErrorf(psrpc.Internal, "missing CSeq header in REFER request")
	}
	c.referCseq = cseq.SeqNo
	return req, nil
}

func (c *sipInbound) TransferCall(ctx context.Context, transferTo string) error {
	req, err := c.newReferReq(transferTo)
	if err != nil {
		return err
	}

	_, err = sendRefer(ctx, c, req, c.s.closing.Watch())
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return psrpc.NewErrorf(psrpc.Canceled, "refer canceled")
	case err := <-c.referDone:
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *sipInbound) handleNotify(req *sip.Request, tx sip.ServerTransaction) error {
	method, cseq, status, err := handleNotify(req)
	if err != nil {
		return err
	}

	switch method {
	default:
		return nil
	case sip.REFER:
		c.mu.RLock()
		defer c.mu.RUnlock()

		if cseq != 0 && cseq != uint32(c.referCseq) {
			// NOTIFY for a different REFER, skip
			return nil
		}

		var result error
		switch {
		case status >= 100 && status < 200:
			// still trying
			return nil
		case status == 200:
			// Success
			result = nil
		default:
			// Failure
			// TODO be more specific in the reported error
			result = psrpc.NewErrorf(psrpc.Canceled, "call transfer failed")
		}
		select {
		case c.referDone <- result:
		case <-time.After(notifyAckTimeout):
		}
		return nil
	}
}

// Close the inbound call cleanly. Depending on the call state it either sends BYE or terminates INVITE with busy status.
func (c *sipInbound) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteOk != nil {
		c.sendBye()
	} else if c.inviteTx != nil {
		c.sendRejected()
	} else {
		c.drop()
	}
}
