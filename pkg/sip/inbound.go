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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"github.com/pkg/errors"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	lksip "github.com/livekit/protocol/sip"
	"github.com/livekit/protocol/utils/traceid"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/res"
)

const (
	statsInterval = time.Minute

	stateUpdateTick = 10 * time.Minute

	// audioBridgeMaxDelay delays sending audio for certain time, unless RTP packet is received.
	// This is done because of audio cutoff at the beginning of calls observed in the wild.
	audioBridgeMaxDelay = 1 * time.Second

	inviteOkRetryInterval      = 250 * time.Millisecond // 1/2 of T1 for faster recovery
	inviteOkRetryIntervalMax   = 3 * time.Second
	inviteOKRetryAttempts      = 5
	inviteOKRetryAttemptsNoACK = 2
	inviteOkAckLateTimeout     = inviteOkRetryIntervalMax
)

var errNoACK = errors.New("no ACK received for 200 OK")

// hashPassword creates a SHA256 hash of the password for logging purposes
func hashPassword(password string) string {
	if password == "" {
		return "<empty>"
	}
	hash := sha256.Sum256([]byte(password))
	return hex.EncodeToString(hash[:8]) // Use first 8 bytes for shorter hash
}

type inboundCallInfo struct {
	sync.Mutex
	cseq        uint32
	cseqAuth    uint32
	invites     uint32
	invitesAuth uint32
}

func inviteHasAuth(r *sip.Request) bool {
	return r.GetHeader("Proxy-Authorization") != nil ||
		r.GetHeader("Authorization") != nil
}

func (c *inboundCallInfo) countInvite(log logger.Logger, req *sip.Request) {
	hasAuth := inviteHasAuth(req)
	cseq := req.CSeq()
	if cseq == nil {
		return
	}
	c.Lock()
	defer c.Unlock()
	cseqPtr := &c.cseq
	countPtr := &c.invites
	name := "invite"
	if hasAuth {
		cseqPtr = &c.cseqAuth
		countPtr = &c.invitesAuth
		name = "invite with auth"
	}
	if *cseqPtr == 0 {
		*cseqPtr = cseq.SeqNo
	}
	if cseq.SeqNo > *cseqPtr {
		return // reinvite
	}
	*countPtr++
	if *countPtr > 1 {
		log.Warnw("remote appears to be retrying an "+name, nil, "invites", *countPtr, "cseq", *cseqPtr)
	}
}

func (s *Server) getCallInfo(id string) *inboundCallInfo {
	c, _ := s.infos.byCallID.Get(id)
	if c != nil {
		return c
	}
	s.infos.Lock()
	defer s.infos.Unlock()
	c, _ = s.infos.byCallID.Get(id)
	if c != nil {
		return c
	}
	c = &inboundCallInfo{}
	s.infos.byCallID.Add(id, c)
	return c
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

func (s *Server) handleInviteAuth(tid traceid.ID, log logger.Logger, req *sip.Request, tx sip.ServerTransaction, from, username, password string) (ok bool) {
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

	// Validate that the username in the request matches the expected username
	if cred.Username != username {
		log.Warnw("Authentication failed - username mismatch", errors.New("username mismatch"),
			"expectedUsername", username,
			"receivedUsername", cred.Username,
		)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil))
		return false
	}

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
	start := time.Now()
	var state *CallState
	ctx := context.Background()
	ctx, span := Tracer.Start(ctx, "sip.Server.processInvite")
	defer span.End()
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
	tid := traceid.FromGUID(callID)
	tr := callTransportFromReq(req)
	legTr := legTransportFromReq(req)
	log := s.log.WithValues(
		"callID", callID,
		"traceID", tid.String(),
		"fromIP", src.Addr(),
		"toIP", req.Destination(),
		"transport", tr,
	)

	var call *inboundCall
	cc := s.newInbound(log, LocalTag(callID), s.ContactURI(legTr), req, tx, func(headers map[string]string) map[string]string {
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
	cc.log = log
	log.Infow("processing invite")

	if err := cc.ValidateInvite(); err != nil {
		if s.conf.HideInboundPort {
			cc.Drop()
		} else {
			cc.RespondAndDrop(sip.StatusBadRequest, "Bad request")
		}
		return psrpc.NewError(psrpc.InvalidArgument, errors.Wrap(err, "invite validation failed"))
	}

	s.cmu.RLock()
	existing := s.byCallID[cc.SIPCallID()]
	s.cmu.RUnlock()
	if existing != nil && existing.cc.InviteCSeq() < cc.InviteCSeq() {
		log.Infow("accepting reinvite", "sipCallID", existing.cc.ID(), "content-type", req.ContentType(), "content-length", req.ContentLength())
		existing.log().Infow("reinvite", "content-type", req.ContentType(), "content-length", req.ContentLength(), "cseq", cc.InviteCSeq())
		cc.AcceptAsKeepAlive(existing.cc.OwnSDP())
		return nil
	}

	from, to := cc.From(), cc.To()

	cmon := s.mon.NewCall(stats.Inbound, from.Host, to.Host)
	cmon.InviteReq()
	defer cmon.SessionDur()()
	var checkDurOnce sync.Once
	checkDur := cmon.CheckDur()
	checked := func() {
		checkDurOnce.Do(func() {
			checkDur.Observe(time.Since(start).Seconds())
		})
	}
	defer checked()
	joinDur := cmon.JoinDur()

	if !s.conf.HideInboundPort {
		cc.Processing()
	}

	callInfo := &rpc.SIPCall{
		LkCallId:  callID,
		SipCallId: cc.SIPCallID(),
		SourceIp:  src.Addr().String(),
		Address:   ToSIPUri("", cc.Address()),
		From:      ToSIPUri("", from),
		To:        ToSIPUri("", to),
	}
	rheaders := cc.RemoteHeaders()
	s.handler.OnInboundInfo(log, callInfo, rheaders)
	for _, h := range rheaders {
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
	checked()
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
		ProviderInfo:  r.ProviderInfo,
		SipCallId:     cc.SIPCallID(),
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
	case AuthQuotaExceeded:
		cmon.InviteErrorShort("quota-exceeded")
		log.Warnw("Rejecting inbound, quota exceeded", nil)
		cc.RespondAndDrop(sip.StatusServiceUnavailable, "Service temporarily unavailable")
		return psrpc.NewErrorf(psrpc.ResourceExhausted, "quota limit exceeded")
	case AuthNoTrunkFound:
		cmon.InviteErrorShort("no-trunk")
		log.Warnw("Rejecting inbound, no trunk found", nil)
		cc.RespondAndDrop(sip.StatusNotFound, "No trunk found")
		return psrpc.NewErrorf(psrpc.NotFound, "no trunk found for call")
	case AuthPassword:
		if s.conf.HideInboundPort {
			// We will send password request anyway, so might as well signal that the progress is made.
			cc.Processing()
		}
		s.getCallInfo(cc.SIPCallID()).countInvite(log, req)
		if !s.handleInviteAuth(tid, log, req, tx, from.User, r.Username, r.Password) {
			cmon.InviteErrorShort("unauthorized")
			// handleInviteAuth will generate the SIP Response as needed
			return psrpc.NewErrorf(psrpc.PermissionDenied, "invalid credentials were provided")
		}
		// ok
	case AuthAccept:
		s.getCallInfo(cc.SIPCallID()).countInvite(log, req)
		// ok
	}

	call = s.newInboundCall(ctx, tid, log, cmon, cc, callInfo, state, start, nil)
	call.joinDur = joinDur
	return call.handleInvite(call.ctx, tid, req, r.TrunkID, s.conf)
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
	c := s.byRemoteTag[tag]
	s.cmu.RUnlock()
	if c == nil {
		return
	}
	c.log().Infow("ACK from remote")
	c.cc.AcceptAck(req, tx)
}

func (s *Server) onBye(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getFromTag(req)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "", nil))
		return
	}

	s.cmu.RLock()
	c := s.byRemoteTag[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.cc.AcceptBye(req, tx)
		var (
			reason    ReasonHeader
			rawReason string
		)
		if h := req.GetHeader("Reason"); h != nil {
			rawReason = h.Value()
			reason, err = ParseReasonHeader(rawReason)
			if err != nil {
				c.log().Warnw("cannot parse reason header", err, "reason-raw", rawReason)
			}
		}
		c.log().Infow("BYE from remote",
			"reason-type", reason.Type,
			"reason-cause", reason.Cause,
			"reason-text", reason.Text,
			"reason-raw", rawReason,
		)
		c.Bye(reason)
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
		"sipCallID", callID,
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
	c := s.byRemoteTag[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.log().Infow("NOTIFY")
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
	tid         traceid.ID
	logPtr      atomic.Pointer[logger.Logger]
	cc          *sipInbound
	mon         *stats.CallMonitor
	state       *CallState
	callStart   time.Time
	extraAttrs  map[string]string
	attrsToHdr  map[string]string
	ctx         context.Context
	cancel      func()
	closeReason atomic.Pointer[ReasonHeader]
	call        *rpc.SIPCall
	media       *MediaPort
	dtmf        chan dtmf.Event // buffered
	lkRoom      RoomInterface   // LiveKit room; only active after correct pin is entered
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
	ctx context.Context,
	tid traceid.ID,
	log logger.Logger,
	mon *stats.CallMonitor,
	cc *sipInbound,
	call *rpc.SIPCall,
	state *CallState,
	callStart time.Time,
	extra map[string]string,
) *inboundCall {
	ctx = context.WithoutCancel(ctx)
	// Map known headers immediately on join. The rest of the mapping will be available later.
	extra = HeadersToAttrs(extra, nil, 0, cc, nil)
	c := &inboundCall{
		s:          s,
		tid:        tid,
		callStart:  callStart,
		mon:        mon,
		cc:         cc,
		call:       call,
		state:      state,
		extraAttrs: extra,
		dtmf:       make(chan dtmf.Event, 10),
		jitterBuf:  SelectValueBool(s.conf.EnableJitterBuffer, s.conf.EnableJitterBufferProb),
		projectID:  "", // Will be set in handleInvite when available
	}
	c.stats.Update()
	c.setLog(log.WithValues("jitterBuf", c.jitterBuf))
	// we need it created earlier so that the audio mixer is available for pin prompts
	c.lkRoom = s.getRoom(c.log(), &c.stats.Room)
	c.ctx, c.cancel = context.WithCancel(ctx)
	s.cmu.Lock()
	s.byRemoteTag[cc.Tag()] = c
	s.byLocalTag[cc.ID()] = c
	s.byCallID[cc.SIPCallID()] = c
	s.cmu.Unlock()
	return c
}

func (c *inboundCall) setLog(log logger.Logger) {
	c.logPtr.Store(&log)
}

func (c *inboundCall) log() logger.Logger {
	ptr := c.logPtr.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

func (c *inboundCall) appendLogValues(kvs ...any) {
	c.setLog(c.log().WithValues(kvs...))
}

func (c *inboundCall) mediaTimeout(ctx context.Context) error {
	if c.cc == nil {
		c.closeWithTimeout(ctx, true)
		return psrpc.NewErrorf(psrpc.DeadlineExceeded, "media timeout")
	}
	if !c.cc.GotACK() {
		c.log().Warnw("Media timeout after missing ACK", errNoACK)
		c.closeWithNoACK(ctx)
		return psrpc.NewError(psrpc.DeadlineExceeded, errNoACK)
	}
	c.closeWithTimeout(ctx, false)
	return nil // logged as a warning in close
}

func (c *inboundCall) handleInvite(ctx context.Context, tid traceid.ID, req *sip.Request, trunkID string, conf *config.Config) error {
	ctx, span := Tracer.Start(ctx, "sip.inbound.handleInvite")
	defer span.End()
	c.mon.InviteAccept()
	c.mon.CallStart()
	defer c.mon.CallEnd()
	defer c.close(ctx, true, callDropped, "other")

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
		c.appendLogValues("projectID", disp.ProjectID)
		c.projectID = disp.ProjectID
	}
	if disp.TrunkID != "" {
		c.appendLogValues("sipTrunk", disp.TrunkID)
	}
	if disp.DispatchRuleID != "" {
		c.appendLogValues("sipRule", disp.DispatchRuleID)
	}

	c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
		info.TrunkId = disp.TrunkID
		info.DispatchRuleId = disp.DispatchRuleID
		info.RoomName = disp.Room.RoomName
		info.ParticipantIdentity = disp.Room.Participant.Identity
		info.ParticipantAttributes = disp.Room.Participant.Attributes
		// Set callidfull in participant attributes for backwards compatibility
		if c.call.SipCallId != "" {
			if info.ParticipantAttributes == nil {
				info.ParticipantAttributes = make(map[string]string)
			}
			info.ParticipantAttributes[AttrSIPCallIDFull] = c.call.SipCallId
		}
	})

	var pinPrompt bool
	switch disp.Result {
	default:
		err := fmt.Errorf("unexpected dispatch result: %v", disp.Result)
		c.log().Errorw("Rejecting inbound call", err)
		c.cc.RespondAndDrop(sip.StatusNotImplemented, "")
		c.close(ctx, true, callDropped, "unexpected-result")
		return psrpc.NewError(psrpc.Unimplemented, err)
	case DispatchNoRuleDrop:
		c.log().Debugw("Rejecting inbound flood")
		c.cc.Drop()
		c.close(ctx, false, callFlood, "flood")
		return psrpc.NewErrorf(psrpc.PermissionDenied, "call was not authorized by trunk configuration")
	case DispatchNoRuleReject:
		c.log().Infow("Rejecting inbound call, doesn't match any Dispatch Rules")
		c.cc.RespondAndDrop(sip.StatusNotFound, "Does not match Trunks or Dispatch Rules")
		c.close(ctx, false, callDropped, "no-dispatch")
		return psrpc.NewErrorf(psrpc.NotFound, "no trunk configuration for call")
	case DispatchAccept:
		pinPrompt = false
	case DispatchRequestPin:
		pinPrompt = true
	}

	runMedia := func(enc livekit.SIPMediaEncryption) ([]byte, error) {
		log := c.log()
		if h := req.ContentLength(); h != nil {
			log = log.WithValues("contentLength", int(*h))
		}
		if h := req.ContentType(); h != nil {
			log = log.WithValues("contentType", h.Value())
			switch h.Value() {
			default:
				log.Infow("unsupported offer type")
			case "application/sdp":
			}
		} else {
			log.Infow("no offer type specified")
		}
		rawSDP := req.Body()
		answerData, err := c.runMediaConn(tid, rawSDP, enc, conf, disp.EnabledFeatures, disp.FeatureFlags)
		if err != nil {
			log = log.WithValues("sdp", string(rawSDP))
			isError := true
			status, reason := callDropped, "media-failed"
			if errors.Is(err, sdp.ErrNoCommonMedia) {
				status, reason = callMediaFailed, "no-common-codec"
				isError = false
			} else if errors.Is(err, sdp.ErrNoCommonCrypto) {
				status, reason = callMediaFailed, "no-common-crypto"
				isError = false
			} else if e := (SDPError{}); errors.As(err, &e) {
				status, reason = callMediaFailed, "sdp-error"
				isError = false
			}
			if isError {
				log.Errorw("Cannot start media", err)
			} else {
				log.Warnw("Cannot start media", err)
			}
			c.cc.RespondAndDrop(sip.StatusInternalServerError, "")
			c.close(ctx, true, status, reason)
			return nil, err
		}
		return answerData, nil
	}

	// If we do not wait for ACK during Accept, we could wait for it later.
	// Otherwise, leave channels nil, so that they never trigger.
	var (
		ackReceived <-chan struct{}
		ackTimeout  <-chan time.Time
	)

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	acceptCall := func(answerData []byte) (bool, error) {
		headers := disp.Headers
		c.attrsToHdr = disp.AttributesToHeaders
		if r := c.lkRoom.Room(); r != nil {
			headers = AttrsToHeaders(r.LocalParticipant.Attributes(), c.attrsToHdr, headers)
		}
		c.log().Infow("Accepting the call", "headers", headers)
		err := c.cc.Accept(ctx, answerData, headers)
		if errors.Is(err, errNoACK) {
			c.log().Errorw("Call accepted, but no ACK received", err)
			c.closeWithNoACK(ctx)
			return false, err
		} else if err != nil {
			c.log().Errorw("Cannot accept the call", err)
			c.close(ctx, true, callAcceptFailed, "accept-failed")
			return false, err
		}
		if !c.s.conf.Experimental.InboundWaitACK {
			ackReceived = c.cc.InviteACK()
			// Start this timer right after the Accept.
			ackTimeout = time.After(inviteOkAckLateTimeout)
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
		c.log().Errorw("Cannot publish track", err)
		c.close(ctx, true, callDropped, "publish-failed")
		return errors.Wrap(err, "publishing track to room failed")
	}
	c.lkRoom.Subscribe()
	if !pinPrompt {
		c.log().Infow("Waiting for track subscription(s)")
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

	return c.waitForCallEnd(ctx, ackReceived, ackTimeout)
}

func (c *inboundCall) waitForCallEnd(ctx context.Context, ackReceived <-chan struct{}, ackTimeout <-chan time.Time) error {
	ctx, span := Tracer.Start(ctx, "sip.inbound.waitForCallEnd")
	defer span.End()
	// Wait for the caller to terminate the call. Send regular keep alives.
	ticker := time.NewTicker(stateUpdateTick)
	defer ticker.Stop()

	statsTicker := time.NewTicker(statsInterval)
	defer statsTicker.Stop()
	for {
		select {
		case <-statsTicker.C:
			c.stats.Update()
			c.printStats(c.log())
		case <-ticker.C:
			c.log().Debugw("sending keep-alive")
			c.state.ForceFlush(ctx)
		case <-ctx.Done():
			c.closeWithHangup(ctx)
			return nil
		case <-c.lkRoom.Closed():
			c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
				info.DisconnectReason = livekit.DisconnectReason_CLIENT_INITIATED
			})
			c.close(ctx, false, callDropped, "removed")
			return nil
		case <-c.media.Timeout():
			return c.mediaTimeout(ctx)
		case <-ackReceived:
			ackTimeout = nil // all good, disable timeout
			ackReceived = nil
		case <-ackTimeout:
			// Only warn, the other side still thinks the call is active, media may be flowing.
			c.log().Warnw("Call accepted, but no ACK received", errNoACK)
			// We don't need to wait for a full media timeout initially, we already know something is not quite right.
			c.media.SetTimeout(min(inviteOkAckLateTimeout, c.s.conf.MediaTimeoutInitial), c.s.conf.MediaTimeout)
		}
	}
}

func (c *inboundCall) runMediaConn(tid traceid.ID, offerData []byte, enc livekit.SIPMediaEncryption, conf *config.Config, features []livekit.SIPFeature, featureFlags map[string]string) (answerData []byte, _ error) {
	c.mon.SDPSize(len(offerData), true)
	c.log().Debugw("SDP offer", "sdp", string(offerData))
	e, err := sdpEncryption(enc)
	if err != nil {
		c.log().Errorw("Cannot parse encryption", err)
		return nil, err
	}

	mp, err := NewMediaPort(tid, c.log(), c.mon, &MediaOptions{
		IP:                  c.s.sconf.MediaIP,
		Ports:               conf.RTPPort,
		MediaTimeoutInitial: c.s.conf.MediaTimeoutInitial,
		MediaTimeout:        c.s.conf.MediaTimeout,
		EnableJitterBuffer:  c.jitterBuf,
		Stats:               &c.stats.Port,
		NoInputResample:     !RoomResample,
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
		return nil, SDPError{Err: err}
	}
	answerData, err = answer.SDP.Marshal()
	if err != nil {
		return nil, err
	}
	c.mon.SDPSize(len(answerData), false)
	c.log().Debugw("SDP answer", "sdp", string(answerData))

	mconf.Processor = c.s.handler.GetMediaProcessor(features, featureFlags)
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
	ctx, span := Tracer.Start(ctx, "sip.inbound.waitMedia")
	defer span.End()
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
		c.closeWithCancelled(ctx)
		return false, nil // caller hung up
	case <-ctx.Done():
		c.closeWithHangup(ctx)
		return false, nil // caller hung up
	case <-c.lkRoom.Closed():
		c.closeWithHangup(ctx)
		return false, psrpc.NewErrorf(psrpc.Canceled, "room closed")
	case <-c.media.Timeout():
		return false, c.mediaTimeout(ctx)
	case <-c.media.Received():
	case <-delay.C:
	}
	return true, nil
}

func (c *inboundCall) waitSubscribe(ctx context.Context, timeout time.Duration) (bool, error) {
	ctx, span := Tracer.Start(ctx, "sip.inbound.waitSubscribe")
	defer span.End()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.cc.Cancelled():
		c.closeWithCancelled(ctx)
		return false, nil
	case <-ctx.Done():
		c.closeWithHangup(ctx)
		return false, nil
	case <-c.lkRoom.Closed():
		c.closeWithHangup(ctx)
		return false, psrpc.NewErrorf(psrpc.Canceled, "room closed")
	case <-c.media.Timeout():
		return false, c.mediaTimeout(ctx)
	case <-timer.C:
		c.close(ctx, false, callDropped, "cannot-subscribe")
		return false, psrpc.NewErrorf(psrpc.DeadlineExceeded, "room subscription timed out")
	case <-c.lkRoom.Subscribed():
		return true, nil
	}
}

func (c *inboundCall) pinPrompt(ctx context.Context, trunkID string) (disp CallDispatch, _ bool, _ error) {
	ctx, span := Tracer.Start(ctx, "sip.inbound.pinPrompt")
	defer span.End()
	c.log().Infow("Requesting Pin for SIP call")
	const pinLimit = 16
	c.playAudio(ctx, c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case <-c.cc.Cancelled():
			c.closeWithCancelled(ctx)
			return disp, false, nil
		case <-ctx.Done():
			c.closeWithHangup(ctx)
			return disp, false, nil
		case <-c.media.Timeout():
			return disp, false, c.mediaTimeout(ctx)
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

				c.log().Infow("Checking Pin for SIP call", "pin", pin, "noPin", noPin)
				disp = c.s.handler.DispatchCall(ctx, &CallInfo{
					TrunkID: trunkID,
					Call:    c.call,
					Pin:     pin,
					NoPin:   noPin,
				})
				if disp.ProjectID != "" {
					c.appendLogValues("projectID", disp.ProjectID)
					c.projectID = disp.ProjectID
				}
				if disp.TrunkID != "" {
					c.appendLogValues("sipTrunk", disp.TrunkID)
				}
				if disp.DispatchRuleID != "" {
					c.appendLogValues("sipRule", disp.DispatchRuleID)
				}
				if disp.Result != DispatchAccept || disp.Room.RoomName == "" {
					c.log().Infow("Rejecting call", "pin", pin, "noPin", noPin)
					c.playAudio(ctx, c.s.res.wrongPin)
					c.close(ctx, false, callDropped, "wrong-pin")
					return disp, false, psrpc.NewErrorf(psrpc.PermissionDenied, "wrong pin")
				}
				c.playAudio(ctx, c.s.res.roomJoin)
				return disp, true, nil
			}
			// Gather pin numbers
			pin += string(b.Digit)
			if len(pin) > pinLimit {
				c.playAudio(ctx, c.s.res.wrongPin)
				c.close(ctx, false, callDropped, "wrong-pin")
				return disp, false, psrpc.NewErrorf(psrpc.PermissionDenied, "wrong pin")
			}
		}
	}
}

func (c *inboundCall) printStats(log logger.Logger) {
	c.stats.Log(log, c.callStart)
}

// close should only be called from handleInvite.
func (c *inboundCall) close(ctx context.Context, error bool, status CallStatus, reason string) {
	ctx = context.WithoutCancel(ctx)
	if !c.done.CompareAndSwap(false, true) {
		return
	}
	c.stats.Closed.Store(true)
	sipCode, sipStatus := status.SIPStatus()
	log := c.log().WithValues("status", sipCode, "reason", reason)
	defer func() {
		c.stats.Update()
		c.printStats(log)
	}()
	c.setStatus(status)
	c.mon.CallTerminate(reason)
	isWarn := error || status == callHangupMedia
	if isWarn {
		log.Warnw("Closing inbound call with error", nil)
	} else {
		log.Infow("Closing inbound call")
	}
	if status != callFlood {
		defer log.Infow("Inbound call closed")
	}

	// Send BYE _before_ closing media/room connection.
	// This ensures participant attributes are still available for
	// attributes_to_headers mapping in the setHeaders callback.
	// See: https://github.com/livekit/sip/issues/404
	c.cc.CloseWithStatus(ctx, sipCode, sipStatus)
	c.closeMedia()
	if c.callDur != nil {
		c.callDur()
	}
	c.s.cmu.Lock()
	delete(c.s.byRemoteTag, c.cc.Tag())
	delete(c.s.byLocalTag, c.cc.ID())
	delete(c.s.byCallID, c.cc.SIPCallID())
	c.s.cmu.Unlock()

	c.s.DeregisterTransferSIPParticipant(c.cc.ID())

	// Call the handler asynchronously to avoid blocking
	if c.s.handler != nil {
		go func(tid traceid.ID) {
			ctx := context.WithoutCancel(ctx)
			ctx, span := Tracer.Start(ctx, "sip.inbound.OnSessionEnd")
			defer span.End()
			c.s.handler.OnSessionEnd(ctx, &CallIdentifier{
				ProjectID: c.projectID,
				CallID:    c.call.LkCallId,
				SipCallID: c.call.SipCallId,
			}, c.state.callInfo, reason)
		}(c.tid)
	}

	c.cancel()
}

func (c *inboundCall) closeWithTimeout(ctx context.Context, isError bool) {
	status := callDropped
	if !isError {
		status = callHangupMedia
	}
	c.close(ctx, isError, status, "media-timeout")
}

func (c *inboundCall) closeWithNoACK(ctx context.Context) {
	c.close(ctx, true, callNoACK, "no-ack")
}

func (c *inboundCall) closeWithCancelled(ctx context.Context) {
	var reason ReasonHeader
	if p := c.closeReason.Load(); p != nil {
		reason = *p
	}
	c.closeWithReason(ctx, CallHangup, "cancelled", reason)
}

func (c *inboundCall) closeWithHangup(ctx context.Context) {
	var reason ReasonHeader
	if p := c.closeReason.Load(); p != nil {
		reason = *p
	}
	c.closeWithReason(ctx, CallHangup, "hangup", reason)
}

func (c *inboundCall) closeWithReason(ctx context.Context, status CallStatus, reasonName string, reason ReasonHeader) {
	ctx = context.WithoutCancel(ctx)
	c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
		info.DisconnectReason = livekit.DisconnectReason_CLIENT_INITIATED
		if info.Error == "" {
			if !reason.IsNormal() {
				info.Error = reason.String()
			}
		}
	})
	if reason.Type != "" {
		if !reason.IsNormal() {
			reasonName = fmt.Sprintf("bye-%s-%d", strings.ToLower(reason.Type), reason.Cause)
		}
	}
	c.close(ctx, false, status, reasonName)
}

func (c *inboundCall) Bye(reason ReasonHeader) {
	c.closeReason.Store(&reason)
	_ = c.Close()
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
	ctx, span := Tracer.Start(ctx, "sip.inbound.createLiveKitParticipant")
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
	c.appendLogValues(
		"room", rconf.RoomName,
		"participant", rconf.Participant.Identity,
		"participantName", rconf.Participant.Name,
	)
	c.log().Infow("Joining room")
	if err := c.createLiveKitParticipant(ctx, rconf, status); err != nil {
		c.log().Errorw("Cannot create LiveKit participant", err)
		c.close(ctx, true, callDropped, "participant-failed")
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
				c.log().Infow("cannot play dial tone", "error", err)
			}
		}()
	}

	err = c.cc.TransferCall(ctx, transferTo, headers)
	if err != nil {
		c.log().Infow("inbound call failed to transfer", "error", err, "transferTo", transferTo)
		return err
	}

	c.log().Infow("inbound call transferred", "transferTo", transferTo)

	// Give time for the peer to hang up first, but hang up ourselves if this doesn't happen within 1 second
	time.AfterFunc(referByeTimeout, func() { c.Close() })

	return nil

}

func (s *Server) newInbound(log logger.Logger, id LocalTag, contact URI, invite *sip.Request, inviteTx sip.ServerTransaction, getHeaders setHeadersFunc) *sipInbound {
	c := &sipInbound{
		log:      log,
		s:        s,
		id:       id,
		invite:   invite,
		inviteTx: inviteTx,
		legTr:    legTransportFromReq(invite),
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
		c.inviteCSeq = h.SeqNo
		c.nextRequestCSeq = h.SeqNo + 1
	}
	if callID := invite.CallID(); callID != nil {
		c.sipCallID = callID.Value()
	}
	return c
}

type sipInbound struct {
	log        logger.Logger
	s          *Server
	id         LocalTag // SCL
	tag        RemoteTag
	sipCallID  string
	invite     *sip.Request
	inviteCSeq uint32
	inviteTx   sip.ServerTransaction
	contact    *sip.ContactHeader
	cancelled  chan struct{}
	from       *sip.FromHeader
	to         *sip.ToHeader
	legTr      Transport
	referDone  chan error

	mu              sync.RWMutex
	lastSDP         []byte
	inviteOk        *sip.Response
	nextRequestCSeq uint32
	referCseq       uint32
	ringing         chan struct{}
	acked           core.Fuse
	setHeaders      setHeadersFunc
}

func (c *sipInbound) ValidateInvite() error {
	if c.sipCallID == "" {
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
	c.respondWithData(status, reason, "", nil)
}

func (c *sipInbound) respondWithData(status sip.StatusCode, reason string, contentType string, body []byte) {
	if c.inviteTx == nil {
		return
	}

	r := sip.NewResponseFromRequest(c.invite, status, reason, body)
	if typ := sip.ContentTypeHeader(contentType); typ != "" {
		r.AppendHeader(&typ)
	}
	r.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))
	if status >= 200 {
		// For an ACK to error statuses.
		r.AppendHeader(c.contact)
	}
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

func (c *sipInbound) SIPCallID() string {
	return c.sipCallID
}

func (c *sipInbound) InviteCSeq() uint32 {
	return c.inviteCSeq
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

func (c *sipInbound) GotACK() bool {
	return c.acked.IsBroken()
}

func (c *sipInbound) InviteACK() <-chan struct{} {
	return c.acked.Watch()
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

func (c *sipInbound) accepted(inviteOK *sip.Response) {
	c.inviteOk = inviteOK
	c.inviteTx = nil
}

func (c *sipInbound) AcceptAsKeepAlive(sdp []byte) {
	c.respondWithData(sip.StatusOK, "OK", "application/sdp", sdp)
}

func (c *sipInbound) OwnSDP() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSDP
}

func (c *sipInbound) Accept(ctx context.Context, sdpData []byte, headers map[string]string) error {
	ctx, span := Tracer.Start(ctx, "sip.inbound.Accept")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteTx == nil {
		return errors.New("call already rejected")
	}
	c.lastSDP = sdpData
	r := sip.NewResponseFromRequest(c.invite, sip.StatusOK, "OK", sdpData)

	// This will effectively redirect future SIP requests to this server instance (if host address is not LB).
	r.AppendHeader(c.contact)

	c.addExtraHeaders(r)

	r.AppendHeader(&contentTypeHeaderSDP)
	for k, v := range headers {
		r.AppendHeader(sip.NewHeader(k, v))
	}
	c.stopRinging()
	retryAfter := inviteOkRetryInterval
	maxRetries := inviteOKRetryAttempts
	if !c.s.conf.Experimental.InboundWaitACK {
		// Still retry, but limit it to ~750ms.
		maxRetries = inviteOKRetryAttemptsNoACK
	}
	if c.legTr != TransportUDP {
		maxRetries = 1
		// That actually becomes an ACK timeout here.
		retryAfter = inviteOkRetryIntervalMax
	}
	var acceptErr error
retries:
	for try := 1; ; try++ {
		if err := c.inviteTx.Respond(r); err != nil {
			return err
		}
		if c.legTr != TransportUDP && !c.s.conf.Experimental.InboundWaitACK {
			// Reliable transport and we are not waiting for ACK - return immediately.
			break retries
		}
		t := time.NewTimer(retryAfter)
		select {
		case <-c.inviteTx.Acks():
			t.Stop()
			break retries
		case <-c.acked.Watch():
			t.Stop()
			break retries
		case <-t.C:
		}
		if try > maxRetries {
			// Only set error if an option is enabled.
			// Otherwise, ignore missing ACK for now.
			if c.s.conf.Experimental.InboundWaitACK {
				acceptErr = errNoACK
			}
			break retries
		}
		retryAfter *= 2
		retryAfter = min(retryAfter, inviteOkRetryIntervalMax)
	}
	// Other side likely thinks it's accepted, so update our state accordingly, even if no ACK follows.
	c.accepted(r)
	return acceptErr
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

func (c *sipInbound) sendBye(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if c.inviteOk == nil {
		return // call wasn't established
	}
	if c.invite == nil {
		return // rejected or closed
	}
	ctx, span := Tracer.Start(ctx, "sip.inbound.sendBye")
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

func (c *sipInbound) sendStatus(ctx context.Context, code sip.StatusCode, status string) {
	ctx = context.WithoutCancel(ctx)
	if c.inviteOk != nil {
		return // call already established
	}
	if c.inviteTx == nil {
		return // rejected or closed
	}
	ctx, span := Tracer.Start(ctx, "sip.inbound.sendStatus")
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
	method, cseq, status, reason, err := handleNotify(req)
	if err != nil {
		return err
	}
	c.log.Infow("handling NOTIFY", "method", method, "status", status, "reason", reason, "cseq", cseq)

	switch method {
	default:
		return nil
	case sip.REFER:
		c.mu.RLock()
		defer c.mu.RUnlock()
		handleReferNotify(cseq, status, reason, c.referCseq, c.referDone)
		return nil
	}
}

// Close the inbound call cleanly. Depending on the call state it either sends BYE or terminates INVITE with busy status.
func (c *sipInbound) Close(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	c.CloseWithStatus(ctx, sip.StatusBusyHere, "Rejected")
}

// CloseWithStatus the inbound call cleanly. Depending on the call state it either sends BYE or terminates INVITE with a specified status.
func (c *sipInbound) CloseWithStatus(ctx context.Context, code sip.StatusCode, status string) {
	ctx = context.WithoutCancel(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteOk != nil {
		// TODO: add cause for a failure, if any
		c.sendBye(ctx)
	} else if c.inviteTx != nil {
		c.sendStatus(ctx, code, status)
	} else {
		c.drop()
	}
}
