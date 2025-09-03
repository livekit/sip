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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/rpc"

	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"github.com/pkg/errors"

	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksip "github.com/livekit/protocol/sip"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/res"
)

const (
	// audioBridgeMaxDelay delays sending audio for certain time, unless RTP packet is received.
	// This is done because of audio cutoff at the beginning of calls observed in the wild.
	audioBridgeMaxDelay = 1 * time.Second

	inviteOkAckTimeout = 5 * time.Second
)

// hashPassword creates a SHA256 hash of the password for logging purposes
func hashPassword(password string) string {
	if password == "" {
		return "<empty>"
	}
	hash := sha256.Sum256([]byte(password))
	return hex.EncodeToString(hash[:8]) // Use first 8 bytes for shorter hash
}

func (s *Server) getInvite(sipCallID string) *inProgressInvite {
	s.imu.Lock()
	defer s.imu.Unlock()
	for i := range s.inProgressInvites {
		if s.inProgressInvites[i].sipCallID == sipCallID {
			return s.inProgressInvites[i]
		}
	}
	if len(s.inProgressInvites) >= digestLimit {
		s.inProgressInvites = s.inProgressInvites[1:]
	}
	is := &inProgressInvite{sipCallID: sipCallID}
	s.inProgressInvites = append(s.inProgressInvites, is)
	return is
}

func (s *Server) handleInviteAuth(log logger.Logger, req *sip.Request, tx sip.ServerTransaction, from, username, password string) (ok bool) {
	log = log.WithValues(
		"username", username,
		"passwordHash", hashPassword(password),
		"method", req.Method.String(),
		"uri", req.Recipient.String(),
	)

	log.Infow("Starting SIP invite authentication")

	if username == "" || password == "" {
		log.Debugw("Skipping authentication - no credentials provided")
		return true
	}

	if s.conf.HideInboundPort {
		// We will send password request anyway, so might as well signal that the progress is made.
		log.Debugw("Sending processing response due to HideInboundPort config")
		_ = tx.Respond(sip.NewResponseFromRequest(req, 100, "Processing", nil))
	}

	// Extract SIP Call ID for tracking in-progress invites
	sipCallID := ""
	if h := req.CallID(); h != nil {
		sipCallID = h.Value()
	}
	inviteState := s.getInvite(sipCallID)
	log = log.WithValues("inviteStateSipCallID", sipCallID)

	h := req.GetHeader("Proxy-Authorization")
	if h == nil {
		inviteState.challenge = digest.Challenge{
			Realm:     UserAgent,
			Nonce:     fmt.Sprintf("%d", time.Now().UnixMicro()),
			Algorithm: "MD5",
		}

		log.Debugw("Created digest challenge",
			"realm", inviteState.challenge.Realm,
			"nonce", inviteState.challenge.Nonce,
			"algorithm", inviteState.challenge.Algorithm,
		)

		res := sip.NewResponseFromRequest(req, 407, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("Proxy-Authenticate", inviteState.challenge.String()))
		_ = tx.Respond(res)
		log.Infow("No Proxy header found. Sending 407 Unauthorized response with Proxy-Authenticate header")
		return false
	}

	log.Debugw("Found Proxy-Authorization header, parsing credentials")
	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		log.Warnw("Failed to parse Proxy-Authorization credentials", err,
			"headerValue", h.Value(),
		)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	// Set credURI and credUsername in logger early to avoid repetitive logging
	log = log.WithValues("credURI", cred.URI, "credUsername", cred.Username)

	log.Debugw("Parsed credentials successfully", "cred", cred)

	// Check if we have a valid challenge state
	if inviteState.challenge.Realm == "" {
		log.Warnw("No challenge state found for authentication attempt", errors.New("missing challenge state"),
			"sipCallID", sipCallID,
			"expectedRealm", UserAgent,
		)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	log.Debugw("Computing digest response",
		"challengeRealm", inviteState.challenge.Realm,
		"challengeNonce", inviteState.challenge.Nonce,
		"challengeAlgorithm", inviteState.challenge.Algorithm,
	)

	digCred, err := digest.Digest(&inviteState.challenge, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: cred.Username,
		Password: password,
	})

	if err != nil {
		log.Warnw("Failed to compute digest response", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	log.Debugw("Digest computation completed",
		"expectedResponse", digCred.Response,
		"receivedResponse", cred.Response,
		"responsesMatch", cred.Response == digCred.Response,
	)

	if cred.Response != digCred.Response {
		log.Warnw("Authentication failed - response mismatch", errors.New("response mismatch"),
			"expectedResponse", digCred.Response,
			"receivedResponse", cred.Response,
		)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil))
		return false
	}

	log.Infow("SIP invite authentication successful")
	return true
}

func (s *Server) onInvite(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	// Error processed in defer
	_ = s.processInvite(req, tx)
}

func (s *Server) processInvite(req *sip.Request, tx sip.ServerTransaction) (retErr error) {
	var state *CallState
	ctx := context.Background()
	defer func() {
		if state == nil {
			return
		}
		state.Update(ctx, func(info *livekit.SIPCallInfo) {
			if err := retErr; err != nil && info.Error == "" {
				info.CallStatus = livekit.SIPCallStatus_SCS_ERROR
				info.Error = err.Error()
			} else {
				info.CallStatus = livekit.SIPCallStatus_SCS_DISCONNECTED
			}
			info.EndedAtNs = time.Now().UnixNano()
		})
	}()
	s.mon.InviteReqRaw(stats.Inbound)
	src, err := netip.ParseAddrPort(req.Source())
	if err != nil {
		tx.Terminate()
		s.log.Errorw("cannot parse source IP", err, "fromIP", src)
		return psrpc.NewError(psrpc.MalformedRequest, errors.Wrap(err, "cannot parse source IP"))
	}
	callID := lksip.NewCallID()
	log := s.log.WithValues(
		"callID", callID,
		"fromIP", src.Addr(),
		"toIP", req.Destination(),
	)

	var call *inboundCall

	tr := transportFromReq(req)
	cc := s.newInbound(LocalTag(callID), s.ContactURI(tr), req, tx, func(headers map[string]string) map[string]string {
		c := call
		if c == nil || len(c.attrsToHdr) == 0 {
			return headers
		}
		r := c.lkRoom.Room()
		if r == nil {
			return headers
		}
		return AttrsToHeaders(r.LocalParticipant.Attributes(), c.attrsToHdr, headers)
	})
	log = LoggerWithParams(log, cc)
	log = LoggerWithHeaders(log, cc)
	log.Infow("processing invite")

	if err := cc.ValidateInvite(); err != nil {
		if s.conf.HideInboundPort {
			cc.Drop()
		} else {
			cc.RespondAndDrop(sip.StatusBadRequest, "Bad request")
		}
		return psrpc.NewError(psrpc.InvalidArgument, errors.Wrap(err, "invite validation failed"))
	}
	ctx, span := tracer.Start(ctx, "Server.onInvite")
	defer span.End()

	from, to := cc.From(), cc.To()

	cmon := s.mon.NewCall(stats.Inbound, from.Host, to.Host)
	cmon.InviteReq()
	defer cmon.SessionDur()()
	joinDur := cmon.JoinDur()

	if !s.conf.HideInboundPort {
		cc.Processing()
	}

	// Extract SIP Call ID directly from the request
	sipCallID := ""
	if h := req.CallID(); h != nil {
		sipCallID = h.Value()
	}

	callInfo := &rpc.SIPCall{
		LkCallId:  callID,
		SipCallId: sipCallID,
		SourceIp:  src.Addr().String(),
		Address:   ToSIPUri("", cc.Address()),
		From:      ToSIPUri("", from),
		To:        ToSIPUri("", to),
	}
	for _, h := range cc.RemoteHeaders() {
		switch h := h.(type) {
		case *sip.ViaHeader:
			callInfo.Via = append(callInfo.Via, &livekit.SIPUri{
				Host:      h.Host,
				Port:      uint32(h.Port),
				Transport: SIPTransportFrom(Transport(h.Transport)),
			})
		}
	}

	r, err := s.handler.GetAuthCredentials(ctx, callInfo)
	if err != nil {
		cmon.InviteErrorShort("auth-error")
		log.Warnw("Rejecting inbound, auth check failed", err)
		cc.RespondAndDrop(sip.StatusServiceUnavailable, "Try again later")
		return psrpc.NewError(psrpc.PermissionDenied, errors.Wrap(err, "rejecting inbound, auth check failed"))
	}
	if r.ProjectID != "" {
		log = log.WithValues("projectID", r.ProjectID)
	}
	if r.TrunkID != "" {
		log = log.WithValues("sipTrunk", r.TrunkID)
	}

	state = NewCallState(s.getIOClient(r.ProjectID), &livekit.SIPCallInfo{
		CallId:        string(cc.ID()),
		Region:        s.region,
		FromUri:       CreateURIFromUserAndAddress(cc.From().User, src.String(), tr).ToSIPUri(),
		ToUri:         CreateURIFromUserAndAddress(cc.To().User, cc.To().Host, tr).ToSIPUri(),
		CallStatus:    livekit.SIPCallStatus_SCS_CALL_INCOMING,
		CallDirection: livekit.SIPCallDirection_SCD_INBOUND,
		CreatedAtNs:   time.Now().UnixNano(),
		TrunkId:       r.TrunkID,
	})
	state.Flush(ctx)

	switch r.Result {
	case AuthDrop:
		cmon.InviteErrorShort("flood")
		log.Debugw("Dropping inbound flood")
		cc.Drop()
		return psrpc.NewErrorf(psrpc.PermissionDenied, "call was not authorized by trunk configuration")
	case AuthNotFound:
		cmon.InviteErrorShort("no-rule")
		log.Warnw("Rejecting inbound, doesn't match any Trunks", nil)
		cc.RespondAndDrop(sip.StatusNotFound, "Does not match any SIP Trunks")
		return psrpc.NewErrorf(psrpc.NotFound, "no trunk configuration for call")
	case AuthPassword:
		if s.conf.HideInboundPort {
			// We will send password request anyway, so might as well signal that the progress is made.
			cc.Processing()
		}
		if !s.handleInviteAuth(log, req, tx, from.User, r.Username, r.Password) {
			cmon.InviteErrorShort("unauthorized")
			// handleInviteAuth will generate the SIP Response as needed
			return psrpc.NewErrorf(psrpc.PermissionDenied, "invalid credentials were provided")
		}
		fallthrough
	case AuthAccept:
		// ok
	}

	call = s.newInboundCall(log, cmon, cc, callInfo, state, nil)
	call.joinDur = joinDur
	return call.handleInvite(call.ctx, req, r.TrunkID, s.conf)
}

func (s *Server) onOptions(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
}

func (s *Server) onAck(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getFromTag(req)
	if err != nil {
		return
	}
	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c == nil {
		return
	}
	c.log.Infow("ACK from remote")
	c.cc.AcceptAck(req, tx)
}

func (s *Server) onBye(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getFromTag(req)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "", nil))
		return
	}

	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.log.Infow("BYE from remote")
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

func (s *Server) OnNoRoute(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	from := ""
	if h := req.From(); h != nil {
		from = h.Address.String()
	}
	to := ""
	if h := req.To(); h != nil {
		to = h.Address.String()
	}
	s.log.Infow("Inbound SIP request not handled",
		"method", req.Method.String(),
		"callID", callID,
		"from", from,
		"to", to)
	tx.Respond(sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil))
}

func (s *Server) onNotify(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getFromTag(req)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "", nil))
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
	state       *CallState
	extraAttrs  map[string]string
	attrsToHdr  map[string]string
	ctx         context.Context
	cancel      func()
	call        *rpc.SIPCall
	media       *MediaPort
	dtmf        chan dtmf.Event // buffered
	lkRoom      *Room           // LiveKit room; only active after correct pin is entered
	callDur     func() time.Duration
	joinDur     func() time.Duration
	forwardDTMF atomic.Bool
	done        atomic.Bool
	started     core.Fuse
	stats       Stats
	jitterBuf   bool
	projectID   string
}

func (s *Server) newInboundCall(
	log logger.Logger,
	mon *stats.CallMonitor,
	cc *sipInbound,
	call *rpc.SIPCall,
	state *CallState,
	extra map[string]string,
) *inboundCall {
	// Map known headers immediately on join. The rest of the mapping will be available later.
	extra = HeadersToAttrs(extra, nil, 0, cc, nil)
	c := &inboundCall{
		s:          s,
		log:        log,
		mon:        mon,
		cc:         cc,
		call:       call,
		state:      state,
		extraAttrs: extra,
		dtmf:       make(chan dtmf.Event, 10),
		jitterBuf:  SelectValueBool(s.conf.EnableJitterBuffer, s.conf.EnableJitterBufferProb),
		projectID:  "", // Will be set in handleInvite when available
	}
	// we need it created earlier so that the audio mixer is available for pin prompts
	c.lkRoom = NewRoom(log, &c.stats.Room)
	c.log = c.log.WithValues("jitterBuf", c.jitterBuf)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	s.cmu.Lock()
	s.activeCalls[cc.Tag()] = c
	s.byLocal[cc.ID()] = c
	s.cmu.Unlock()
	return c
}

func (c *inboundCall) handleInvite(ctx context.Context, req *sip.Request, trunkID string, conf *config.Config) error {
	c.mon.InviteAccept()
	c.mon.CallStart()
	defer c.mon.CallEnd()
	defer c.close(true, callDropped, "other")

	// Extract and store the SIP call ID from the request
	if h := req.CallID(); h != nil {
		c.call.SipCallId = h.Value()
	}

	c.cc.StartRinging()
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could even learn that this number is not allowed and reject the call, or ask for pin if required.
	disp := c.s.handler.DispatchCall(ctx, &CallInfo{
		TrunkID: trunkID,
		Call:    c.call,
		Pin:     "",
		NoPin:   false,
	})
	if disp.ProjectID != "" {
		c.log = c.log.WithValues("projectID", disp.ProjectID)
		c.projectID = disp.ProjectID
	}
	if disp.TrunkID != "" {
		c.log = c.log.WithValues("sipTrunk", disp.TrunkID)
	}
	if disp.DispatchRuleID != "" {
		c.log = c.log.WithValues("sipRule", disp.DispatchRuleID)
	}

	c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
		info.TrunkId = disp.TrunkID
		info.DispatchRuleId = disp.DispatchRuleID
		info.RoomName = disp.Room.RoomName
		info.ParticipantIdentity = disp.Room.Participant.Identity
		info.ParticipantAttributes = disp.Room.Participant.Attributes
	})

	var pinPrompt bool
	switch disp.Result {
	default:
		err := fmt.Errorf("unexpected dispatch result: %v", disp.Result)
		c.log.Errorw("Rejecting inbound call", err)
		c.cc.RespondAndDrop(sip.StatusNotImplemented, "")
		c.close(true, callDropped, "unexpected-result")
		return psrpc.NewError(psrpc.Unimplemented, err)
	case DispatchNoRuleDrop:
		c.log.Debugw("Rejecting inbound flood")
		c.cc.Drop()
		c.close(false, callFlood, "flood")
		return psrpc.NewErrorf(psrpc.PermissionDenied, "call was not authorized by trunk configuration")
	case DispatchNoRuleReject:
		c.log.Infow("Rejecting inbound call, doesn't match any Dispatch Rules")
		c.cc.RespondAndDrop(sip.StatusNotFound, "Does not match Trunks or Dispatch Rules")
		c.close(false, callDropped, "no-dispatch")
		return psrpc.NewErrorf(psrpc.NotFound, "no trunk configuration for call")
	case DispatchAccept:
		pinPrompt = false
	case DispatchRequestPin:
		pinPrompt = true
	}

	runMedia := func(enc livekit.SIPMediaEncryption) ([]byte, error) {
		answerData, err := c.runMediaConn(req.Body(), enc, conf, disp.EnabledFeatures)
		if err != nil {
			isError := true
			status, reason := callDropped, "media-failed"
			if errors.Is(err, sdp.ErrNoCommonMedia) {
				status, reason = callMediaFailed, "no-common-codec"
				isError = false
			} else if errors.Is(err, sdp.ErrNoCommonCrypto) {
				status, reason = callMediaFailed, "no-common-crypto"
				isError = false
			}
			if isError {
				c.log.Errorw("Cannot start media", err)
			} else {
				c.log.Warnw("Cannot start media", err)
			}
			c.cc.RespondAndDrop(sip.StatusInternalServerError, "")
			c.close(true, status, reason)
			return nil, err
		}
		return answerData, nil
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	acceptCall := func(answerData []byte) (bool, error) {
		headers := disp.Headers
		c.attrsToHdr = disp.AttributesToHeaders
		if r := c.lkRoom.Room(); r != nil {
			headers = AttrsToHeaders(r.LocalParticipant.Attributes(), c.attrsToHdr, headers)
		}
		c.log.Infow("Accepting the call", "headers", headers)
		if err := c.cc.Accept(ctx, answerData, headers); err != nil {
			c.log.Errorw("Cannot accept the call", err)
			return false, err
		}
		c.media.EnableTimeout(true)
		c.media.EnableOut()
		if ok, err := c.waitMedia(ctx); !ok {
			return false, err
		}
		c.setStatus(CallActive)
		return true, nil
	}

	ok := false
	var answerData []byte
	if pinPrompt {
		var err error
		// Accept the call first on the SIP side, so that we can send audio prompts.
		// This also means we have to pick encryption setting early, before room is selected.
		// Backend must explicitly enable encryption for pin prompts.
		answerData, err = runMedia(disp.MediaEncryption)
		if err != nil {
			return err // already sent a response
		}
		if ok, err = acceptCall(answerData); !ok {
			return err // could be success if the caller hung up
		}
		disp, ok, err = c.pinPrompt(ctx, trunkID)
		if !ok {
			return err // already sent a response. Could be success if user hung up
		}
	} else {
		// Start media with given encryption settings.
		var err error
		answerData, err = runMedia(disp.MediaEncryption)
		if err != nil {
			return err // already sent a response
		}
	}
	p := &disp.Room.Participant
	p.Attributes = HeadersToAttrs(p.Attributes, disp.HeadersToAttributes, disp.IncludeHeaders, c.cc, nil)
	if disp.MaxCallDuration <= 0 || disp.MaxCallDuration > maxCallDuration {
		disp.MaxCallDuration = maxCallDuration
	}
	if disp.RingingTimeout <= 0 {
		disp.RingingTimeout = defaultRingingTimeout
	}
	disp.Room.JitterBuf = c.jitterBuf
	ctx, cancel := context.WithTimeout(ctx, disp.MaxCallDuration)
	defer cancel()
	status := CallRinging
	if pinPrompt {
		status = CallActive
	}
	if err := c.joinRoom(ctx, disp.Room, status); err != nil {
		return errors.Wrap(err, "failed joining room")
	}
	// Publish our own track.
	if err := c.publishTrack(); err != nil {
		c.log.Errorw("Cannot publish track", err)
		c.close(true, callDropped, "publish-failed")
		return errors.Wrap(err, "publishing track to room failed")
	}
	c.lkRoom.Subscribe()
	if !pinPrompt {
		c.log.Infow("Waiting for track subscription(s)")
		// For dispatches without pin, we first wait for LK participant to become available,
		// and also for at least one track subscription. In the meantime we keep ringing.
		if ok, err := c.waitSubscribe(ctx, disp.RingingTimeout); !ok {
			return err // already sent a response. Could be success if caller hung up
		}
		if ok, err := acceptCall(answerData); !ok {
			return err // already sent a response. Could be success if caller hung up
		}
	}

	c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
		info.StartedAtNs = time.Now().UnixNano()
		info.CallStatus = livekit.SIPCallStatus_SCS_ACTIVE
		if r := c.lkRoom.Room(); r != nil {
			info.RoomId = r.SID()
			info.RoomName = r.Name()
			info.ParticipantAttributes = r.LocalParticipant.Attributes()
		}
	})

	c.started.Break()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	// Wait for the caller to terminate the call. Send regular keep alives
	for {
		select {
		case <-ticker.C:
			c.log.Debugw("sending keep-alive")
			c.state.ForceFlush(ctx)
		case <-ctx.Done():
			c.closeWithHangup()
			return nil
		case <-c.lkRoom.Closed():
			c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
				info.DisconnectReason = livekit.DisconnectReason_CLIENT_INITIATED
			})
			c.close(false, callDropped, "removed")
			return nil
		case <-c.media.Timeout():
			c.closeWithTimeout()
			return psrpc.NewErrorf(psrpc.DeadlineExceeded, "media timeout")
		}
	}
}

func (c *inboundCall) runMediaConn(offerData []byte, enc livekit.SIPMediaEncryption, conf *config.Config, features []livekit.SIPFeature) (answerData []byte, _ error) {
	c.mon.SDPSize(len(offerData), true)
	c.log.Debugw("SDP offer", "sdp", string(offerData))
	e, err := sdpEncryption(enc)
	if err != nil {
		c.log.Errorw("Cannot parse encryption", err)
		return nil, err
	}

	mp, err := NewMediaPort(c.log, c.mon, &MediaOptions{
		IP:                  c.s.sconf.MediaIP,
		Ports:               conf.RTPPort,
		MediaTimeoutInitial: c.s.conf.MediaTimeoutInitial,
		MediaTimeout:        c.s.conf.MediaTimeout,
		EnableJitterBuffer:  c.jitterBuf,
		Stats:               &c.stats.Port,
	}, RoomSampleRate)
	if err != nil {
		return nil, err
	}
	c.media = mp
	c.media.EnableTimeout(false) // enabled once we accept the call
	c.media.DisableOut()         // disabled until we send 200
	c.media.SetDTMFAudio(conf.AudioDTMF)

	answer, mconf, err := mp.SetOffer(offerData, e)
	if err != nil {
		return nil, err
	}
	answerData, err = answer.SDP.Marshal()
	if err != nil {
		return nil, err
	}
	c.mon.SDPSize(len(answerData), false)
	c.log.Debugw("SDP answer", "sdp", string(answerData))

	mconf.Processor = c.s.handler.GetMediaProcessor(features)
	if err = c.media.SetConfig(mconf); err != nil {
		return nil, err
	}
	if mconf.Audio.DTMFType != 0 {
		c.media.HandleDTMF(c.handleDTMF)
	}

	// Must be set earlier to send the pin prompts.
	if w := c.lkRoom.SwapOutput(c.media.GetAudioWriter()); w != nil {
		_ = w.Close()
	}
	if mconf.Audio.DTMFType != 0 {
		c.lkRoom.SetDTMFOutput(c.media)
	}
	c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
		info.AudioCodec = mconf.Audio.Codec.Info().SDPName
	})
	return answerData, nil
}

func (c *inboundCall) waitMedia(ctx context.Context) (bool, error) {
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
		return false, nil // caller hung up
	case <-ctx.Done():
		c.closeWithHangup()
		return false, nil // caller hung up
	case <-c.lkRoom.Closed():
		c.closeWithHangup()
		return false, psrpc.NewErrorf(psrpc.Canceled, "room closed")
	case <-c.media.Timeout():
		c.closeWithTimeout()
		return false, psrpc.NewErrorf(psrpc.DeadlineExceeded, "media timed out")
	case <-c.media.Received():
	case <-delay.C:
	}
	return true, nil
}

func (c *inboundCall) waitSubscribe(ctx context.Context, timeout time.Duration) (bool, error) {
	ctx, span := tracer.Start(ctx, "inboundCall.waitSubscribe")
	defer span.End()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.cc.Cancelled():
		c.closeWithCancelled()
		return false, nil
	case <-ctx.Done():
		c.closeWithHangup()
		return false, nil
	case <-c.lkRoom.Closed():
		c.closeWithHangup()
		return false, psrpc.NewErrorf(psrpc.Canceled, "room closed")
	case <-c.media.Timeout():
		c.closeWithTimeout()
		return false, psrpc.NewErrorf(psrpc.DeadlineExceeded, "media timed out")
	case <-timer.C:
		c.close(false, callDropped, "cannot-subscribe")
		return false, psrpc.NewErrorf(psrpc.DeadlineExceeded, "room subscription timed out")
	case <-c.lkRoom.Subscribed():
		return true, nil
	}
}

func (c *inboundCall) pinPrompt(ctx context.Context, trunkID string) (disp CallDispatch, _ bool, _ error) {
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
			return disp, false, nil
		case <-ctx.Done():
			c.closeWithHangup()
			return disp, false, nil
		case <-c.media.Timeout():
			c.closeWithTimeout()
			return disp, false, psrpc.NewErrorf(psrpc.DeadlineExceeded, "media timeout")
		case b, ok := <-c.dtmf:
			if !ok {
				c.Close()
				return disp, false, psrpc.NewErrorf(psrpc.Canceled, "failed reading DTMF event")
			}
			if b.Digit == 0 {
				continue // unrecognized
			}
			if b.Digit == '#' {
				// End of the pin
				noPin = pin == ""

				c.log.Infow("Checking Pin for SIP call", "pin", pin, "noPin", noPin)
				disp = c.s.handler.DispatchCall(ctx, &CallInfo{
					TrunkID: trunkID,
					Call:    c.call,
					Pin:     pin,
					NoPin:   noPin,
				})
				if disp.ProjectID != "" {
					c.log = c.log.WithValues("projectID", disp.ProjectID)
					c.projectID = disp.ProjectID
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
					return disp, false, psrpc.NewErrorf(psrpc.PermissionDenied, "wrong pin")
				}
				c.playAudio(ctx, c.s.res.roomJoin)
				return disp, true, nil
			}
			// Gather pin numbers
			pin += string(b.Digit)
			if len(pin) > pinLimit {
				c.playAudio(ctx, c.s.res.wrongPin)
				c.close(false, callDropped, "wrong-pin")
				return disp, false, psrpc.NewErrorf(psrpc.PermissionDenied, "wrong pin")
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
	sipCode, sipStatus := status.SIPStatus()
	log := c.log.WithValues("status", sipCode, "reason", reason)
	defer func() {
		log.Infow("call statistics", "stats", c.stats.Load())
	}()
	if error {
		log.Warnw("Closing inbound call with error", nil)
	} else {
		log.Infow("Closing inbound call")
	}
	if status != callFlood {
		defer log.Infow("Inbound call closed")
	}

	c.closeMedia()
	c.cc.CloseWithStatus(sipCode, sipStatus)
	if c.callDur != nil {
		c.callDur()
	}
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.cc.Tag())
	delete(c.s.byLocal, c.cc.ID())
	c.s.cmu.Unlock()

	c.s.DeregisterTransferSIPParticipant(c.cc.ID())

	// Call the handler asynchronously to avoid blocking
	if c.s.handler != nil {
		go c.s.handler.OnSessionEnd(context.Background(), &CallIdentifier{
			ProjectID: c.projectID,
			CallID:    c.call.LkCallId,
			SipCallID: c.call.SipCallId,
		}, c.state.callInfo, reason)
	}

	c.cancel()
}

func (c *inboundCall) closeWithTimeout() {
	c.close(true, callDropped, "media-timeout")
}

func (c *inboundCall) closeWithCancelled() {
	c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
		info.DisconnectReason = livekit.DisconnectReason_CLIENT_INITIATED
	})
	c.close(false, CallHangup, "cancelled")
}

func (c *inboundCall) closeWithHangup() {
	c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
		info.DisconnectReason = livekit.DisconnectReason_CLIENT_INITIATED
	})
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

func (c *inboundCall) createLiveKitParticipant(ctx context.Context, rconf RoomConfig, status CallStatus) error {
	ctx, span := tracer.Start(ctx, "inboundCall.createLiveKitParticipant")
	defer span.End()
	partConf := &rconf.Participant
	if partConf.Attributes == nil {
		partConf.Attributes = make(map[string]string)
	}
	for k, v := range c.extraAttrs {
		partConf.Attributes[k] = v
	}
	partConf.Attributes[livekit.AttrSIPCallStatus] = status.Attribute()
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

func (c *inboundCall) joinRoom(ctx context.Context, rconf RoomConfig, status CallStatus) error {
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
	if err := c.createLiveKitParticipant(ctx, rconf, status); err != nil {
		c.log.Errorw("Cannot create LiveKit participant", err)
		c.close(true, callDropped, "participant-failed")
		return errors.Wrap(err, "cannot create LiveKit participant")
	}
	return nil
}

func (c *inboundCall) playAudio(ctx context.Context, frames []msdk.PCM16Sample) {
	t := c.lkRoom.NewTrack()
	if t == nil {
		return // closed
	}
	defer t.Close()

	sampleRate := res.SampleRate
	if t.SampleRate() != sampleRate {
		frames = slices.Clone(frames)
		for i := range frames {
			frames[i] = msdk.Resample(nil, t.SampleRate(), frames[i], sampleRate)
		}
	}
	_ = msdk.PlayAudio[msdk.PCM16Sample](ctx, t, rtp.DefFrameDur, frames)
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

func (c *inboundCall) transferCall(ctx context.Context, transferTo string, headers map[string]string, dialtone bool) (retErr error) {
	var err error

	tID := c.state.StartTransfer(ctx, transferTo)
	defer func() {
		c.state.EndTransfer(ctx, tID, retErr)
	}()

	if dialtone && c.started.IsBroken() && !c.done.Load() {
		const ringVolume = math.MaxInt16 / 2
		rctx, rcancel := context.WithCancel(ctx)
		defer rcancel()

		// mute the room audio to the SIP participant
		w := c.lkRoom.SwapOutput(nil)

		defer func() {
			if retErr != nil && !c.done.Load() {
				c.lkRoom.SwapOutput(w)
			} else if w != nil {
				w.Close()
			}
		}()

		go func() {
			aw := c.media.GetAudioWriter()

			err := tones.Play(rctx, aw, ringVolume, tones.ETSIRinging)
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				c.log.Infow("cannot play dial tone", "error", err)
			}
		}()
	}

	err = c.cc.TransferCall(ctx, transferTo, headers)
	if err != nil {
		c.log.Infow("inbound call failed to transfer", "error", err, "transferTo", transferTo)
		return err
	}

	c.log.Infow("inbound call transferred", "transferTo", transferTo)

	// Give time for the peer to hang up first, but hang up ourselves if this doesn't happen within 1 second
	time.AfterFunc(referByeTimeout, func() { c.Close() })

	return nil

}

func (s *Server) newInbound(id LocalTag, contact URI, invite *sip.Request, inviteTx sip.ServerTransaction, getHeaders setHeadersFunc) *sipInbound {
	c := &sipInbound{
		s:        s,
		id:       id,
		invite:   invite,
		inviteTx: inviteTx,
		contact: &sip.ContactHeader{
			Address: *contact.GetContactURI(),
		},
		cancelled:  make(chan struct{}),
		referDone:  make(chan error), // Do not buffer the channel to avoid reading a result for an old request
		setHeaders: getHeaders,
	}
	c.from = invite.From()
	if c.from != nil {
		c.tag, _ = getTagFrom(c.from.Params)
	}
	c.to = invite.To()
	if h := invite.CSeq(); h != nil {
		c.nextRequestCSeq = h.SeqNo + 1
	}
	if callID := invite.CallID(); callID != nil {
		c.callID = callID.Value()
	}
	return c
}

type sipInbound struct {
	s         *Server
	id        LocalTag
	tag       RemoteTag
	callID    string
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
	acked           core.Fuse
	setHeaders      setHeadersFunc
}

func (c *sipInbound) ValidateInvite() error {
	if c.callID == "" {
		return errors.New("no Call-ID header in INVITE")
	}
	if c.from == nil {
		return errors.New("no From header in INVITE")
	}
	if c.to == nil {
		return errors.New("no To header in INVITE")
	}
	if c.tag == "" {
		return errors.New("no tag in From in INVITE")
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

	r := sip.NewResponseFromRequest(c.invite, status, reason, nil)
	r.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))
	c.addExtraHeaders(r)
	_ = c.inviteTx.Respond(r)
}

func (c *sipInbound) RespondAndDrop(status sip.StatusCode, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopRinging()
	c.respond(status, reason)
	c.drop()
}

func (c *sipInbound) Address() sip.Uri {
	if c.invite == nil {
		return sip.Uri{}
	}
	return c.invite.Recipient
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

func (c *sipInbound) CallID() string {
	return c.callID
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
		ticker := time.NewTicker(c.s.conf.SIPRingingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case r := <-cancels:
				close(c.cancelled)
				_ = tx.Respond(sip.NewResponseFromRequest(r, sip.StatusOK, "OK", nil))
				c.RespondAndDrop(sip.StatusRequestTerminated, "Request Terminated")
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

func (c *sipInbound) addExtraHeaders(r *sip.Response) {
	if c.s.conf.AddRecordRoute {
		// Other in-dialog requests should be sent to this instance as well.
		recordRoute := c.contact.Address.Clone()
		if recordRoute.UriParams == nil {
			recordRoute.UriParams = sip.HeaderParams{}
		}
		recordRoute.UriParams.Add("lr", "")
		r.PrependHeader(&sip.RecordRouteHeader{
			Address: *recordRoute,
		})
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
	r := sip.NewResponseFromRequest(c.invite, sip.StatusOK, "OK", sdpData)

	// This will effectively redirect future SIP requests to this server instance (if host address is not LB).
	r.AppendHeader(c.contact)

	c.addExtraHeaders(r)

	r.AppendHeader(&contentTypeHeaderSDP)
	for k, v := range headers {
		r.AppendHeader(sip.NewHeader(k, v))
	}
	c.stopRinging()
	if err := c.inviteTx.Respond(r); err != nil {
		return err
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, inviteOkAckTimeout)
	defer ackCancel()
	select {
	case <-ackCtx.Done():
		return errors.New("no ACK received for 200 OK")
	case <-c.inviteTx.Acks():
	case <-c.acked.Watch():
	}
	c.inviteOk = r
	c.inviteTx = nil // accepted
	return nil
}

func (c *sipInbound) AcceptAck(req *sip.Request, tx sip.ServerTransaction) {
	c.acked.Break()
}

func (c *sipInbound) AcceptBye(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop() // mark as closed
}

func (c *sipInbound) swapSrcDst(req *sip.Request) {
	dest := c.inviteOk.Destination()
	if contact := c.invite.Contact(); contact != nil {
		req.Recipient = contact.Address
		dest = ConvertURI(&contact.Address).GetDest()
	} else {
		req.Recipient = c.from.Address
	}
	if route := c.invite.RecordRoute(); route != nil {
		dest = ConvertURI(&route.Address).GetDest()
	}
	req.SetSource(c.inviteOk.Source())
	req.SetDestination(dest)
	req.RemoveHeader("From")
	req.AppendHeader((*sip.FromHeader)(c.to))
	req.RemoveHeader("To")
	req.AppendHeader((*sip.ToHeader)(c.from))
	// Remove all Via headers
	for req.RemoveHeader("Via") {
	}
	req.PrependHeader(c.generateViaHeader(req))

	rrHdrs := req.GetHeaders("Record-Route")
	for _, hdr := range rrHdrs {
		req.PrependHeader(&sip.RouteHeader{Address: hdr.(*sip.RecordRouteHeader).Address})
	}
	// Remove all Record-Route headers
	for req.RemoveHeader("Record-Route") {
	}
}

func (c *sipInbound) generateViaHeader(req *sip.Request) *sip.ViaHeader {
	newvia := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       req.Transport(),
		Host:            c.s.sconf.SignalingIP.String(), // This can be rewritten by transport layer
		Port:            c.s.conf.SIPPort,               // This can be rewritten by transport layer
		Params:          sip.NewParams(),
	}
	// NOTE: Consider lenght of branch configurable
	newvia.Params.Add("branch", sip.GenerateBranchN(16))

	return newvia
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
	if c.setHeaders != nil {
		for k, v := range c.setHeaders(nil) {
			r.AppendHeader(sip.NewHeader(k, v))
		}
	}

	c.setCSeq(r)
	c.swapSrcDst(r)
	c.drop()
	sendAndACK(ctx, c, r)
}

func (c *sipInbound) sendStatus(code sip.StatusCode, status string) {
	if c.inviteOk != nil {
		return // call already established
	}
	if c.inviteTx == nil {
		return // rejected or closed
	}
	_, span := tracer.Start(context.Background(), "sipInbound.sendStatus")
	defer span.End()

	if status == "" {
		status = sipStatus(code)
	}
	r := sip.NewResponseFromRequest(c.invite, code, status, nil)
	if c.setHeaders != nil {
		for k, v := range c.setHeaders(nil) {
			r.AppendHeader(sip.NewHeader(k, v))
		}
	}
	_ = c.inviteTx.Respond(r)
	c.drop()
}

func (c *sipInbound) WriteRequest(req *sip.Request) error {
	return c.s.sipSrv.TransportLayer().WriteMsg(req)
}

func (c *sipInbound) Transaction(req *sip.Request) (sip.ClientTransaction, error) {
	return c.s.sipSrv.TransactionLayer().Request(req)
}

func (c *sipInbound) newReferReq(transferTo string, headers map[string]string) (*sip.Request, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.invite == nil || c.inviteOk == nil {
		return nil, psrpc.NewErrorf(psrpc.FailedPrecondition, "can't transfer non established call") // call wasn't established
	}

	from := c.invite.From()
	if from == nil {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "no From URI in invite")
	}
	if c.setHeaders != nil {
		headers = c.setHeaders(headers)
	}

	// This will effectively redirect future SIP requests to this server instance (if host address is not LB).
	req := NewReferRequest(c.invite, c.inviteOk, c.contact, transferTo, headers)
	c.setCSeq(req)
	c.swapSrcDst(req)

	cseq := req.CSeq()
	if cseq == nil {
		return nil, psrpc.NewErrorf(psrpc.Internal, "missing CSeq header in REFER request")
	}
	c.referCseq = cseq.SeqNo
	return req, nil
}

func (c *sipInbound) TransferCall(ctx context.Context, transferTo string, headers map[string]string) error {
	req, err := c.newReferReq(transferTo, headers)
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
	c.CloseWithStatus(sip.StatusBusyHere, "Rejected")
}

// CloseWithStatus the inbound call cleanly. Depending on the call state it either sends BYE or terminates INVITE with a specified status.
func (c *sipInbound) CloseWithStatus(code sip.StatusCode, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteOk != nil {
		c.sendBye()
	} else if c.inviteTx != nil {
		c.sendStatus(code, status)
	} else {
		c.drop()
	}
}
