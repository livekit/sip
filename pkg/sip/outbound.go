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
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"github.com/pkg/errors"
	"golang.org/x/exp/maps"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/protocol/utils/traceid"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type sipOutboundConfig struct {
	address         string
	transport       livekit.SIPTransport
	host            string
	from            string
	to              string
	user            string
	pass            string
	dtmf            string
	dialtone        bool
	headers         map[string]string
	includeHeaders  livekit.SIPHeaderOptions
	headersToAttrs  map[string]string
	attrsToHeaders  map[string]string
	ringingTimeout  time.Duration
	maxCallDuration time.Duration
	enabledFeatures []livekit.SIPFeature
	featureFlags    map[string]string
	mediaEncryption sdp.Encryption
	displayName     *string
}

type outboundCall struct {
	c         *Client
	tid       traceid.ID
	log       logger.Logger
	state     *CallState
	callStart time.Time
	cc        *sipOutbound
	media     *MediaPort
	started   core.Fuse
	stopped   core.Fuse
	closing   core.Fuse
	stats     Stats
	jitterBuf bool
	projectID string

	mu       sync.RWMutex
	mon      *stats.CallMonitor
	lkRoom   RoomInterface
	lkRoomIn msdk.PCM16Writer // output to room; OPUS at 48k
	sipConf  sipOutboundConfig
}

func (c *Client) newCall(ctx context.Context, tid traceid.ID, conf *config.Config, log logger.Logger, id LocalTag, room RoomConfig, sipConf sipOutboundConfig, state *CallState, projectID string) (*outboundCall, error) {
	if sipConf.maxCallDuration <= 0 || sipConf.maxCallDuration > maxCallDuration {
		sipConf.maxCallDuration = maxCallDuration
	}
	if sipConf.ringingTimeout <= 0 {
		sipConf.ringingTimeout = defaultRingingTimeout
	}
	jitterBuf := SelectValueBool(conf.EnableJitterBuffer, conf.EnableJitterBufferProb)
	room.JitterBuf = jitterBuf

	tr := TransportFrom(sipConf.transport)
	contact := c.ContactURI(tr)
	if sipConf.host == "" {
		sipConf.host = contact.GetHost()
	}
	call := &outboundCall{
		c:         c,
		tid:       tid,
		log:       log,
		sipConf:   sipConf,
		state:     state,
		callStart: time.Now(),
		jitterBuf: jitterBuf,
		projectID: projectID,
	}
	call.stats.Update()
	call.log = call.log.WithValues("jitterBuf", call.jitterBuf)
	call.cc = c.newOutbound(log, id, URI{
		User:      sipConf.from,
		Host:      sipConf.host,
		Addr:      contact.Addr,
		Transport: tr,
	}, contact, sipConf.displayName, func(headers map[string]string) map[string]string {
		c := call
		if len(c.sipConf.attrsToHeaders) == 0 {
			return headers
		}
		r := c.lkRoom.Room()
		if r == nil {
			return headers
		}
		return AttrsToHeaders(r.LocalParticipant.Attributes(), c.sipConf.attrsToHeaders, headers)
	})

	call.mon = c.mon.NewCall(stats.Outbound, sipConf.host, sipConf.address)
	var err error

	call.media, err = NewMediaPort(tid, call.log, call.mon, &MediaOptions{
		IP:                  c.sconf.MediaIP,
		Ports:               conf.RTPPort,
		MediaTimeoutInitial: c.conf.MediaTimeoutInitial,
		MediaTimeout:        c.conf.MediaTimeout,
		EnableJitterBuffer:  call.jitterBuf,
		Stats:               &call.stats.Port,
		NoInputResample:     !RoomResample,
	}, RoomSampleRate)
	if err != nil {
		call.close(ctx, errors.Wrap(err, "media failed"), callDropped, "media-failed", livekit.DisconnectReason_UNKNOWN_REASON)
		return nil, err
	}
	call.media.SetDTMFAudio(conf.AudioDTMF)
	call.media.EnableTimeout(false)
	call.media.DisableOut() // disabled until we get 200
	if err := call.connectToRoom(ctx, room, c.getRoom); err != nil {
		call.close(ctx, errors.Wrap(err, "room join failed"), callDropped, "join-failed", livekit.DisconnectReason_UNKNOWN_REASON)
		return nil, fmt.Errorf("update room failed: %w", err)
	}

	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.activeCalls[id] = call
	return call, nil
}

func (c *outboundCall) ensureClosed(ctx context.Context) {
	c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
		if info.Error != "" {
			info.CallStatus = livekit.SIPCallStatus_SCS_ERROR
		} else {
			info.CallStatus = livekit.SIPCallStatus_SCS_DISCONNECTED
		}
		if r := c.lkRoom.Room(); r != nil {
			if p := r.LocalParticipant; p != nil {
				info.ParticipantIdentity = p.Identity()
				info.ParticipantAttributes = p.Attributes()
			}
		}
		info.EndedAtNs = time.Now().UnixNano()
	})
}

func (c *outboundCall) setErrStatus(ctx context.Context, err error) {
	if err == nil {
		return
	}
	c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
		if info.Error != "" {
			return
		}
		info.Error = err.Error()
		info.CallStatus = livekit.SIPCallStatus_SCS_ERROR
	})
}

func (c *outboundCall) Dial(ctx context.Context) error {
	ctx, span := Tracer.Start(ctx, "sip.outbound.Dial")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, c.sipConf.maxCallDuration)
	defer cancel()
	c.mon.CallStart()
	defer c.mon.CallEnd()

	err := c.connectSIP(ctx, c.tid)
	if err != nil {
		c.ensureClosed(ctx)
		return err // connectSIP updates the error code on the callInfo
	}

	c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
		lkroom := c.lkRoom.Room()
		if lkroom == nil {
			c.log.Errorw("failed to update SIP info", fmt.Errorf("unexpected state: lkroom is not set"))
			return
		}
		info.RoomId = lkroom.SID()
		info.StartedAtNs = time.Now().UnixNano()
		info.CallStatus = livekit.SIPCallStatus_SCS_ACTIVE
	})
	return nil
}

func (c *outboundCall) WaitClose(ctx context.Context) error {
	ctx, span := Tracer.Start(ctx, "sip.outbound.WaitClose")
	defer span.End()
	return c.waitClose(ctx, c.tid)
}
func (c *outboundCall) waitClose(ctx context.Context, tid traceid.ID) error {
	ctx = context.WithoutCancel(ctx)
	defer c.ensureClosed(ctx)

	ticker := time.NewTicker(stateUpdateTick)
	defer ticker.Stop()

	statsTicker := time.NewTicker(statsInterval)
	defer statsTicker.Stop()
	for {
		select {
		case <-statsTicker.C:
			c.stats.Update()
			c.printStats()
		case <-ticker.C:
			c.log.Debugw("sending keep-alive")
			c.state.ForceFlush(ctx)
		case <-c.Disconnected():
			c.CloseWithReason(ctx, callDropped, "removed", livekit.DisconnectReason_CLIENT_INITIATED)
			return nil
		case <-c.media.Timeout():
			c.closeWithTimeout(ctx)
			err := psrpc.NewErrorf(psrpc.DeadlineExceeded, "media timeout")
			c.setErrStatus(ctx, err)
			return err
		case <-c.Closed():
			return nil
		}
	}
}

func (c *outboundCall) DialAsync(ctx context.Context) {
	ctx, span := Tracer.Start(ctx, "sip.outbound.DialAsync")
	defer span.End()
	ctx = context.WithoutCancel(ctx)
	go func() {
		if err := c.Dial(ctx); err != nil {
			return
		}
		_ = c.WaitClose(ctx)
	}()
}

func (c *outboundCall) Closed() <-chan struct{} {
	return c.stopped.Watch()
}

func (c *outboundCall) Disconnected() <-chan struct{} {
	return c.lkRoom.Closed()
}

func (c *outboundCall) Close(ctx context.Context) error {
	c.closing.Break()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(ctx, nil, callDropped, "shutdown", livekit.DisconnectReason_SERVER_SHUTDOWN)
	return nil
}

func (c *outboundCall) CloseWithReason(ctx context.Context, status CallStatus, description string, reason livekit.DisconnectReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(ctx, nil, status, description, reason)
}

func (c *outboundCall) closeWithTimeout(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(ctx, psrpc.NewErrorf(psrpc.DeadlineExceeded, "media-timeout"), callDropped, "media-timeout", livekit.DisconnectReason_UNKNOWN_REASON)
}

func (c *outboundCall) printStats() {
	c.stats.Log(c.log, c.callStart)
}

func (c *outboundCall) close(ctx context.Context, err error, status CallStatus, description string, reason livekit.DisconnectReason) {
	ctx = context.WithoutCancel(ctx)
	c.stopped.Once(func() {
		c.stats.Closed.Store(true)
		defer func() {
			c.stats.Update()
			c.printStats()
		}()

		c.setStatus(status)
		if err != nil {
			c.log.Warnw("Closing outbound call with error", nil, "reason", description)
		} else {
			c.log.Infow("Closing outbound call", "reason", description)
		}
		c.state.Update(ctx, func(info *livekit.SIPCallInfo) {
			if err != nil && info.Error == "" {
				info.Error = err.Error()
				info.CallStatus = livekit.SIPCallStatus_SCS_ERROR
			}
			info.DisconnectReason = reason
		})

		// Send BYE _before_ closing media/room connection.
		// This ensures participant attributes are still available for
		// attributes_to_headers mapping in the setHeaders callback.
		// See: https://github.com/livekit/sip/issues/404
		c.stopSIP(ctx, description)
		c.media.Close()

		if r := c.lkRoom; r != nil {
			_ = r.CloseOutput()
			_ = r.CloseWithReason(status.DisconnectReason())
		}
		c.lkRoomIn = nil

		c.c.cmu.Lock()
		delete(c.c.activeCalls, c.cc.ID())
		if tag := c.cc.Tag(); tag != "" {
			delete(c.c.byRemote, tag)
		}
		c.c.cmu.Unlock()

		c.c.DeregisterTransferSIPParticipant(string(c.cc.ID()))

		// Call the handler asynchronously to avoid blocking
		if c.c.handler != nil {
			go func(tid traceid.ID) {
				ctx := context.WithoutCancel(ctx)
				ctx, span := Tracer.Start(ctx, "sip.outbound.OnSessionEnd")
				defer span.End()
				c.c.handler.OnSessionEnd(ctx, &CallIdentifier{
					TraceID:   tid,
					ProjectID: c.projectID,
					CallID:    c.state.callInfo.CallId,
					SipCallID: c.cc.SIPCallID(),
				}, c.state.callInfo, description)
			}(c.tid)
		}
	})
}

func (c *outboundCall) Participant() ParticipantInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lkRoom.Participant()
}

func (c *outboundCall) connectSIP(ctx context.Context, tid traceid.ID) error {
	ctx, span := Tracer.Start(ctx, "sip.outbound.connectSIP")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialSIP(ctx, tid); err != nil {
		c.log.Infow("SIP call failed", "error", err)

		reportErr := err
		status, desc, reason := callDropped, "invite-failed", livekit.DisconnectReason_UNKNOWN_REASON
		var e *livekit.SIPStatus
		if errors.As(err, &e) {
			switch int(e.Code) {
			case int(sip.StatusTemporarilyUnavailable):
				status, desc, reason = callUnavailable, "unavailable", livekit.DisconnectReason_USER_UNAVAILABLE
				reportErr = nil
			case int(sip.StatusBusyHere):
				status, desc, reason = callRejected, "busy", livekit.DisconnectReason_USER_REJECTED
				reportErr = nil
			}
		}
		c.close(ctx, reportErr, status, desc, reason)
		return err
	}
	c.connectMedia()
	c.started.Break()
	c.lkRoom.Subscribe()
	c.log.Infow("Outbound SIP call established")
	return nil
}

func (c *outboundCall) connectToRoom(ctx context.Context, lkNew RoomConfig, getRoom GetRoomFunc) error {
	ctx, span := Tracer.Start(ctx, "sip.outbound.connectToRoom")
	defer span.End()
	attrs := lkNew.Participant.Attributes
	if attrs == nil {
		attrs = make(map[string]string)
	}

	sipCallID := attrs[livekit.AttrSIPCallID]
	if sipCallID != "" {
		c.c.RegisterTransferSIPParticipant(sipCallID, c)
	}

	attrs[livekit.AttrSIPCallStatus] = CallDialing.Attribute()
	lkNew.Participant.Attributes = attrs
	r := getRoom(c.log, &c.stats.Room)
	if err := r.Connect(c.c.conf, lkNew); err != nil {
		_ = r.Close()
		return err
	}
	// We have to create the track early because we might play a dialtone while SIP connects.
	// Thus, we are forced to set full sample rate here instead of letting the codec adapt to the SIP source sample rate.
	local, err := r.NewParticipantTrack(RoomSampleRate)
	if err != nil {
		_ = r.Close()
		return err
	}
	c.lkRoom = r
	c.lkRoomIn = local
	return nil
}

func (c *outboundCall) dialSIP(ctx context.Context, tid traceid.ID) error {
	if c.sipConf.dialtone {
		const ringVolume = math.MaxInt16 / 2
		rctx, rcancel := context.WithCancel(ctx)
		defer rcancel()

		dst := c.lkRoomIn // already under mutex

		// Play dialtone to the room while participant connects
		go func(tid traceid.ID) {
			rctx, span := Tracer.Start(rctx, "tones.Play")
			defer span.End()

			if dst == nil {
				c.log.Infow("room is not ready, ignoring dial tone")
				return
			}
			err := tones.Play(rctx, dst, ringVolume, tones.ETSIRinging)
			if err != nil && !errors.Is(err, context.Canceled) {
				c.log.Infow("cannot play dial tone", "error", err)
			}
		}(tid)
	}
	err := c.sipSignal(ctx, tid)
	if err != nil {
		return err
	}

	if digits := c.sipConf.dtmf; digits != "" {
		c.setStatus(CallAutomation)
		// Write initial DTMF to SIP
		if err := c.media.WriteDTMF(ctx, digits); err != nil {
			return err
		}
	}
	c.setStatus(CallActive)

	return nil
}

func (c *outboundCall) connectMedia() {
	if w := c.lkRoom.SwapOutput(c.media.GetAudioWriter()); w != nil {
		_ = w.Close()
	}
	c.lkRoom.SetDTMFOutput(c.media)

	c.media.WriteAudioTo(c.lkRoomIn)
	c.media.HandleDTMF(c.handleDTMF)
}

type sipRespFunc func(code sip.StatusCode, hdrs Headers)

func sipResponse(ctx context.Context, tx sip.ClientTransaction, stop <-chan struct{}, setState sipRespFunc) (*sip.Response, error) {
	cnt := 0
	for {
		select {
		case <-ctx.Done():
			_ = tx.Cancel()
			return nil, psrpc.NewErrorf(psrpc.Canceled, "canceled")
		case <-stop:
			_ = tx.Cancel()
			return nil, psrpc.NewErrorf(psrpc.Canceled, "canceled")
		case <-tx.Done():
			return nil, psrpc.NewErrorf(psrpc.Canceled, "transaction failed to complete (%d intermediate responses)", cnt)
		case res := <-tx.Responses():
			status := res.StatusCode
			if setState != nil {
				setState(res.StatusCode, res.Headers())
			}
			if status/100 != 1 { // != 1xx
				return res, nil
			}
			// continue
			cnt++
		}
	}
}

func (c *outboundCall) stopSIP(ctx context.Context, reason string) {
	c.mon.CallTerminate(reason)
	c.cc.Close(ctx)
}

func (c *outboundCall) setStatus(v CallStatus) {
	attr := v.Attribute()
	if attr == "" {
		return
	}
	if c.lkRoom == nil {
		return
	}
	r := c.lkRoom.Room()
	if r == nil {
		return
	}
	r.LocalParticipant.SetAttributes(map[string]string{
		livekit.AttrSIPCallStatus: attr,
	})
}

func (c *outboundCall) setExtraAttrs(hdrToAttr map[string]string, opts livekit.SIPHeaderOptions, cc Signaling, hdrs Headers) {
	extra := HeadersToAttrs(nil, hdrToAttr, opts, cc, hdrs)
	if c.lkRoom != nil && len(extra) != 0 {
		room := c.lkRoom.Room()
		if room != nil {
			room.LocalParticipant.SetAttributes(extra)
		} else {
			c.log.Warnw("could not set attributes on nil room", nil, "attrs", extra)
		}
	}
}

func (c *outboundCall) sipSignal(ctx context.Context, tid traceid.ID) error {
	ctx, span := Tracer.Start(ctx, "sip.outbound.sipSignal")
	defer span.End()

	if c.sipConf.ringingTimeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, c.sipConf.ringingTimeout)
		defer cancel()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			// parent context cancellation or success
			return
		case <-c.Disconnected():
		case <-c.Closed():
		}
		cancel()
	}()

	sdpOffer, err := c.media.NewOffer(c.sipConf.mediaEncryption)
	if err != nil {
		return err
	}
	sdpOfferData, err := sdpOffer.SDP.Marshal()
	if err != nil {
		return err
	}
	c.mon.SDPSize(len(sdpOfferData), true)
	c.log.Debugw("SDP offer", "sdp", string(sdpOfferData))
	joinDur := c.mon.JoinDur()

	c.mon.InviteReq()

	toUri := CreateURIFromUserAndAddress(c.sipConf.to, c.sipConf.address, TransportFrom(c.sipConf.transport))

	ringing := false
	sdpResp, err := c.cc.Invite(ctx, toUri, c.sipConf.user, c.sipConf.pass, c.sipConf.headers, sdpOfferData, func(code sip.StatusCode, hdrs Headers) {
		if code == sip.StatusOK {
			return // is set separately
		}
		if !ringing && code >= sip.StatusRinging && code < sip.StatusOK {
			ringing = true
			c.setStatus(CallRinging)
		}
		c.setExtraAttrs(nil, 0, nil, hdrs)
	})
	// Update SIPCallInfo with the SIP Call-ID after Invite
	if sipCallID := c.cc.SIPCallID(); sipCallID != "" {
		c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
			info.SipCallId = sipCallID
			// Set callidfull in participant attributes for backwards compatibility
			if info.ParticipantAttributes == nil {
				info.ParticipantAttributes = make(map[string]string)
			}
			info.ParticipantAttributes[AttrSIPCallIDFull] = sipCallID
		})
	}
	if err != nil {
		// TODO: should we retry? maybe new offer will work
		var e *livekit.SIPStatus
		if errors.As(err, &e) {
			c.mon.InviteError(statusName(int(e.Code)))
			c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
				info.CallStatusCode = e
			})
		} else {
			c.mon.InviteError("other")
		}
		c.cc.Close(ctx)
		c.log.Infow("SIP invite failed", "error", err)
		return err
	}
	c.mon.SDPSize(len(sdpResp), false)
	c.log.Debugw("SDP answer", "sdp", string(sdpResp))

	c.log = LoggerWithHeaders(c.log, c.cc)

	mc, err := c.media.SetAnswer(sdpOffer, sdpResp, c.sipConf.mediaEncryption)
	if err != nil {
		return err
	}
	mc.Processor = c.c.handler.GetMediaProcessor(c.sipConf.enabledFeatures, c.sipConf.featureFlags)
	if err = c.media.SetConfig(mc); err != nil {
		return err
	}

	c.c.cmu.Lock()
	c.c.byRemote[c.cc.Tag()] = c
	c.c.cmu.Unlock()

	c.mon.InviteAccept()
	c.media.EnableOut()
	c.media.EnableTimeout(true)
	err = c.cc.AckInviteOK(ctx)
	if err != nil {
		c.log.Infow("SIP accept failed", "error", err)
		return err
	}
	joinDur()

	c.setExtraAttrs(c.sipConf.headersToAttrs, c.sipConf.includeHeaders, c.cc, nil)
	c.state.DeferUpdate(func(info *livekit.SIPCallInfo) {
		info.AudioCodec = mc.Audio.Codec.Info().SDPName
		if r := c.lkRoom.Room(); r != nil {
			info.ParticipantAttributes = r.LocalParticipant.Attributes()
		}
	})
	return nil
}

func (c *outboundCall) handleDTMF(ev dtmf.Event) {
	if c.lkRoom == nil {
		return
	}
	_ = c.lkRoom.SendData(&livekit.SipDTMF{
		Code:  uint32(ev.Code),
		Digit: string([]byte{ev.Digit}),
	}, lksdk.WithDataPublishReliable(true))
}

func (c *outboundCall) transferCall(ctx context.Context, transferTo string, headers map[string]string, dialtone bool) (retErr error) {
	ctx, span := Tracer.Start(ctx, "sip.outbound.transferCall")
	defer span.End()
	var err error

	tID := c.state.StartTransfer(ctx, transferTo)
	defer func() {
		c.state.EndTransfer(ctx, tID, retErr)
	}()

	if dialtone && c.started.IsBroken() && !c.stopped.IsBroken() {
		const ringVolume = math.MaxInt16 / 2
		rctx, rcancel := context.WithCancel(ctx)
		defer rcancel()

		// mute the room audio to the SIP participant
		w := c.lkRoom.SwapOutput(nil)

		defer func() {
			if retErr != nil && !c.stopped.IsBroken() {
				c.lkRoom.SwapOutput(w)
			} else {
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

	err = c.cc.transferCall(ctx, transferTo, headers)
	if err != nil {
		c.log.Infow("outbound call failed to transfer", "error", err, "transferTo", transferTo)
		return err
	}

	c.log.Infow("outbound call transferred", "transferTo", transferTo)

	// Give time for the peer to hang up first, but hang up ourselves if this doesn't happen within 1 second
	time.AfterFunc(referByeTimeout, func() {
		c.CloseWithReason(ctx, CallHangup, "call transferred", livekit.DisconnectReason_CLIENT_INITIATED)
	})

	return nil
}

func (c *Client) newOutbound(log logger.Logger, id LocalTag, from, contact URI, displayName *string, getHeaders setHeadersFunc) *sipOutbound {
	from = from.Normalize()
	if displayName == nil { // Nothing specified, preserve legacy behavior
		displayName = &from.User
	}

	fromHeader := &sip.FromHeader{
		DisplayName: *displayName,
		Address:     *from.GetURI(),
		Params:      sip.NewParams(),
	}
	contactHeader := &sip.ContactHeader{
		Address: *contact.GetContactURI(),
	}
	fromHeader.Params.Add("tag", string(id))
	return &sipOutbound{
		log:        log,
		c:          c,
		id:         id,
		from:       fromHeader,
		contact:    contactHeader,
		referDone:  make(chan error), // Do not buffer the channel to avoid reading a result for an old request
		nextCSeq:   1,
		getHeaders: getHeaders,
	}
}

type sipOutbound struct {
	log     logger.Logger
	c       *Client
	id      LocalTag
	from    *sip.FromHeader
	contact *sip.ContactHeader

	mu         sync.RWMutex
	tag        RemoteTag
	callID     string
	invite     *sip.Request
	inviteOk   *sip.Response
	to         *sip.ToHeader
	nextCSeq   uint32
	getHeaders setHeadersFunc

	referCseq uint32
	referDone chan error
}

func (c *sipOutbound) From() sip.Uri {
	return c.from.Address
}

func (c *sipOutbound) To() sip.Uri {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.to == nil {
		return sip.Uri{}
	}
	return c.to.Address
}

func (c *sipOutbound) Address() sip.Uri {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.invite == nil {
		return sip.Uri{}
	}
	return c.invite.Recipient
}

func (c *sipOutbound) ID() LocalTag {
	return c.id
}

func (c *sipOutbound) Tag() RemoteTag {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tag
}

func (c *sipOutbound) SIPCallID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.callID
}

func (c *sipOutbound) RemoteHeaders() Headers {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.inviteOk == nil {
		return nil
	}
	return c.inviteOk.Headers()
}

func (c *sipOutbound) Invite(ctx context.Context, to URI, user, pass string, headers map[string]string, sdpOffer []byte, setState sipRespFunc) ([]byte, error) {
	ctx, span := Tracer.Start(ctx, "sip.outbound.Invite")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	toHeader := &sip.ToHeader{Address: *to.GetURI()}

	c.callID = guid.HashedID(fmt.Sprintf("%s-%s", string(c.id), toHeader.Address.String()))
	c.log = c.log.WithValues("sipCallID", c.callID)

	var (
		sipHeaders         Headers
		authHeader         = ""
		authHeaderRespName string
		req                *sip.Request
		resp               *sip.Response
		err                error
	)
	if keys := maps.Keys(headers); len(keys) != 0 {
		sort.Strings(keys)
		for _, key := range keys {
			sipHeaders = append(sipHeaders, sip.NewHeader(key, headers[key]))
		}
	}
authLoop:
	for try := 0; ; try++ {
		if try >= 5 {
			return nil, fmt.Errorf("max auth retry attemps reached")
		}
		req, resp, err = c.attemptInvite(ctx, sip.CallIDHeader(c.callID), toHeader, sdpOffer, authHeaderRespName, authHeader, sipHeaders, setState)
		if err != nil {
			return nil, err
		}
		var authHeaderName string
		switch resp.StatusCode {
		case sip.StatusOK:
			break authLoop
		default:
			return nil, fmt.Errorf("unexpected status from INVITE response: %w", &livekit.SIPStatus{
				Code:   livekit.SIPStatusCode(resp.StatusCode),
				Status: resp.Reason,
			})
		case sip.StatusBadRequest,
			sip.StatusNotFound,
			sip.StatusTemporarilyUnavailable,
			sip.StatusNotAcceptableHere,
			sip.StatusBusyHere:
			err := &livekit.SIPStatus{
				Code:   livekit.SIPStatusCode(resp.StatusCode),
				Status: resp.Reason,
			}
			if body := resp.Body(); len(body) != 0 {
				err.Status = string(body)
			} else if s := resp.GetHeader("X-Twilio-Error"); s != nil {
				err.Status = s.Value()
			}
			return nil, fmt.Errorf("INVITE failed: %w", err)
		case sip.StatusUnauthorized:
			authHeaderName = "WWW-Authenticate"
			authHeaderRespName = "Authorization"
		case sip.StatusProxyAuthRequired:
			authHeaderName = "Proxy-Authenticate"
			authHeaderRespName = "Proxy-Authorization"
		}
		c.log.Infow("auth requested", "status", resp.StatusCode, "body", string(resp.Body()))
		// auth required
		if user == "" || pass == "" {
			return nil, errors.New("server required auth, but no username or password was provided")
		}
		headerVal := resp.GetHeader(authHeaderName)
		if headerVal == nil {
			return nil, errors.New("no auth header in response")
		}
		challengeStr := headerVal.Value()
		challenge, err := digest.ParseChallenge(challengeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid challenge %q: %w", challengeStr, err)
		}
		toHeader := resp.To()
		if toHeader == nil {
			return nil, errors.New("no 'To' header on Response")
		}

		cred, err := digest.Digest(challenge, digest.Options{
			Method:   req.Method.String(),
			URI:      toHeader.Address.String(),
			Username: user,
			Password: pass,
		})
		if err != nil {
			return nil, err
		}
		authHeader = cred.String()
		// Try again with a computed digest
	}

	c.invite, c.inviteOk = req, resp
	toHeader = resp.To()
	if toHeader == nil {
		return nil, errors.New("no To header in INVITE response")
	}
	var ok bool
	c.tag, ok = getTagFrom(toHeader.Params)
	if !ok {
		return nil, errors.New("no tag in To header in INVITE response")
	}

	if cont := resp.Contact(); cont != nil {
		req.Recipient = cont.Address
		if req.Recipient.Port == 0 {
			req.Recipient.Port = 5060
		}
	}

	// We currently don't plumb the request back to caller to construct the ACK with.
	// Thus, we need to modify the request to update any route sets.
	for req.RemoveHeader("Route") {
	}
	for _, hdr := range resp.GetHeaders("Record-Route") {
		req.PrependHeader(&sip.RouteHeader{Address: hdr.(*sip.RecordRouteHeader).Address})
	}

	return c.inviteOk.Body(), nil
}

func (c *sipOutbound) AcceptBye(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop() // mark as closed
}

func (c *sipOutbound) AckInviteOK(ctx context.Context) error {
	ctx, span := Tracer.Start(ctx, "sip.outbound.AckInviteOK")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invite == nil || c.inviteOk == nil {
		return errors.New("call already closed")
	}
	return c.c.sipCli.WriteRequest(sip.NewAckRequest(c.invite, c.inviteOk, nil))
}

func (c *sipOutbound) attemptInvite(ctx context.Context, callID sip.CallIDHeader, to *sip.ToHeader, offer []byte, authHeaderName, authHeader string, headers Headers, setState sipRespFunc) (*sip.Request, *sip.Response, error) {
	ctx, span := Tracer.Start(ctx, "sip.outbound.attemptInvite")
	defer span.End()
	req := sip.NewRequest(sip.INVITE, to.Address)
	c.setCSeq(req)
	req.RemoveHeader("Call-ID")
	req.AppendHeader(&callID)

	req.SetBody(offer)
	req.AppendHeader(to)
	req.AppendHeader(c.from)
	req.AppendHeader(c.contact)

	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authHeader != "" {
		req.AppendHeader(sip.NewHeader(authHeaderName, authHeader))
	}
	for _, h := range headers {
		req.AppendHeader(h)
	}

	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Terminate()

	// Log the actual local port used for TCP connections from the DialPort range
	if req.Transport() == "TCP" {
		// Type-assert to *sipgo.Client to access the embedded UserAgent
		if sipClient, ok := c.c.sipCli.(*sipgo.Client); ok {
			if tpl := sipClient.TransportLayer(); tpl != nil {
				// Try to get the connection using the destination address
				// The connection should be available after TransactionRequest creates it
				if dest := req.Destination(); dest != "" {
					if conn, err := tpl.GetConnection("tcp", dest); err == nil && conn != nil {
						if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok && tcpAddr != nil {
							c.log.Debugw("TCP connection using port on cloud-sip side", "port", tcpAddr.Port)
						}
					}
				}
			}
		}
	}

	resp, err := sipResponse(ctx, tx, c.c.closing.Watch(), setState)
	return req, resp, err
}

func (c *sipOutbound) WriteRequest(req *sip.Request) error {
	return c.c.sipCli.WriteRequest(req)
}

func (c *sipOutbound) Transaction(req *sip.Request) (sip.ClientTransaction, error) {
	return c.c.sipCli.TransactionRequest(req)
}

func (c *sipOutbound) setCSeq(req *sip.Request) {
	setCSeq(req, c.nextCSeq)

	c.nextCSeq++
}

func (c *sipOutbound) sendBye(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if c.invite == nil || c.inviteOk == nil {
		return // call wasn't established
	}
	ctx, span := Tracer.Start(ctx, "sip.outbound.sendBye")
	defer span.End()
	r := sip.NewByeRequest(c.invite, c.inviteOk, nil)
	r.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))
	if c.getHeaders != nil {
		for k, v := range c.getHeaders(nil) {
			r.AppendHeader(sip.NewHeader(k, v))
		}
	}
	if c.c.closing.IsBroken() {
		// do not wait for a response
		_ = c.WriteRequest(r)
		return
	}
	c.setCSeq(r)
	c.drop()
	sendAndACK(ctx, c, r)
}

func (c *sipOutbound) sendCancel(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if c.invite == nil {
		return
	}
	ctx, span := Tracer.Start(ctx, "sip.outbound.sendCancel")
	defer span.End()
	r := sip.NewCancelRequest(c.invite)
	r.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))
	if c.getHeaders != nil {
		for k, v := range c.getHeaders(nil) {
			r.AppendHeader(sip.NewHeader(k, v))
		}
	}
	_ = c.WriteRequest(r)
	c.drop()
}

func (c *sipOutbound) drop() {
	c.invite = nil
	c.inviteOk = nil
	c.nextCSeq = 0
}

func (c *sipOutbound) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
}

func (c *sipOutbound) transferCall(ctx context.Context, transferTo string, headers map[string]string) error {
	c.mu.Lock()

	if c.invite == nil || c.inviteOk == nil {
		c.mu.Unlock()
		return psrpc.NewErrorf(psrpc.FailedPrecondition, "can't transfer non established call") // call wasn't established
	}

	if c.c.closing.IsBroken() {
		c.mu.Unlock()
		return psrpc.NewErrorf(psrpc.FailedPrecondition, "can't transfer hung up call")
	}

	if c.getHeaders != nil {
		headers = c.getHeaders(headers)
	}

	req := NewReferRequest(c.invite, c.inviteOk, c.contact, transferTo, headers)
	c.setCSeq(req)
	cseq := req.CSeq()

	if cseq == nil {
		c.mu.Unlock()
		return psrpc.NewErrorf(psrpc.Internal, "missing CSeq header in REFER request")
	}
	c.referCseq = cseq.SeqNo
	c.mu.Unlock()

	_, err := sendRefer(ctx, c, req, c.c.closing.Watch())
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

func (c *sipOutbound) handleNotify(req *sip.Request, tx sip.ServerTransaction) error {
	method, cseq, status, reason, err := handleNotify(req)
	if err != nil {
		c.log.Infow("error parsing NOTIFY request", "error", err)

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

func (c *sipOutbound) Close(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteOk != nil {
		c.sendBye(ctx)
	} else if c.invite != nil {
		c.sendCancel(ctx)
	} else {
		c.drop()
	}
}
