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
	"math"
	"net/netip"
	"sort"
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"golang.org/x/exp/maps"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/tones"
	"github.com/livekit/sip/pkg/stats"
)

type sipOutboundConfig struct {
	address        string
	transport      livekit.SIPTransport
	host           string
	from           string
	to             string
	user           string
	pass           string
	dtmf           string
	ringtone       bool
	headers        map[string]string
	headersToAttrs map[string]string
}

type outboundCall struct {
	c       *Client
	log     logger.Logger
	cc      *sipOutbound
	media   *MediaPort
	stopped core.Fuse
	closing core.Fuse

	mu         sync.RWMutex
	mon        *stats.CallMonitor
	lkRoom     *Room
	lkRoomIn   media.PCM16Writer // output to room; OPUS at 48k
	sipConf    sipOutboundConfig
	sipRunning bool
}

func (c *Client) newCall(ctx context.Context, conf *config.Config, log logger.Logger, id LocalTag, room RoomConfig, sipConf sipOutboundConfig) (*outboundCall, error) {
	if sipConf.host == "" {
		sipConf.host = c.signalingIp
	}
	call := &outboundCall{
		c:   c,
		log: log,
		cc: c.newOutbound(id, URI{
			User: sipConf.from,
			Host: sipConf.host,
			Addr: netip.AddrPortFrom(netip.Addr{}, uint16(conf.SIPPort)),
		}),
		sipConf: sipConf,
	}
	call.mon = c.mon.NewCall(stats.Outbound, sipConf.host, sipConf.address)
	var err error
	call.media, err = NewMediaPort(call.log, call.mon, &MediaConfig{
		IP:                  c.signalingIp,
		Ports:               conf.RTPPort,
		MediaTimeoutInitial: c.conf.MediaTimeoutInitial,
		MediaTimeout:        c.conf.MediaTimeout,
	}, RoomSampleRate)
	if err != nil {
		call.close(true, callDropped, "media-failed")
		return nil, err
	}
	call.media.SetDTMFAudio(conf.AudioDTMF)
	if err := call.connectToRoom(ctx, room); err != nil {
		call.close(true, callDropped, "join-failed")
		return nil, fmt.Errorf("update room failed: %w", err)
	}

	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.activeCalls[id] = call
	return call, nil
}

func (c *outboundCall) Start(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	c.mon.CallStart()
	defer c.mon.CallEnd()
	err := c.ConnectSIP(ctx)
	if err != nil {
		c.log.Infow("SIP call failed", "error", err)
		c.CloseWithReason(callDropped, "connect error")
		return
	}
	select {
	case <-c.Disconnected():
		c.CloseWithReason(callDropped, "removed")
	case <-c.media.Timeout():
		c.closeWithTimeout()
	case <-c.Closed():
	}
}

func (c *outboundCall) Closed() <-chan struct{} {
	return c.stopped.Watch()
}

func (c *outboundCall) Disconnected() <-chan struct{} {
	return c.lkRoom.Closed()
}

func (c *outboundCall) Close() error {
	c.closing.Break()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(false, callDropped, "shutdown")
	return nil
}

func (c *outboundCall) CloseWithReason(status CallStatus, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(false, status, reason)
}

func (c *outboundCall) closeWithTimeout() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(true, callDropped, "media-timeout")
}

func (c *outboundCall) close(error bool, status CallStatus, reason string) {
	c.stopped.Once(func() {
		if status != "" {
			c.setStatus(status)
		}
		if error {
			c.log.Warnw("Closing outbound call with error", nil, "reason", reason)
		} else {
			c.log.Infow("Closing outbound call", "reason", reason)
		}
		c.media.Close()
		_ = c.lkRoom.CloseOutput()

		_ = c.lkRoom.Close()
		c.lkRoomIn = nil

		c.stopSIP(reason)

		c.c.cmu.Lock()
		delete(c.c.activeCalls, c.cc.ID())
		if tag := c.cc.Tag(); tag != "" {
			delete(c.c.byRemote, tag)
		}
		c.c.cmu.Unlock()

		c.c.DeregisterTransferSIPParticipant(string(c.cc.ID()))
	})
}

func (c *outboundCall) Participant() ParticipantInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lkRoom.Participant()
}

func (c *outboundCall) ConnectSIP(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "outboundCall.ConnectSIP")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialSIP(ctx); err != nil {
		c.close(true, callDropped, "invite-failed")
		return fmt.Errorf("update SIP failed: %w", err)
	}
	c.connectMedia()
	c.lkRoom.Subscribe()
	c.log.Infow("Outbound SIP call established")
	return nil
}

func (c *outboundCall) connectToRoom(ctx context.Context, lkNew RoomConfig) error {
	ctx, span := tracer.Start(ctx, "outboundCall.connectToRoom")
	defer span.End()
	attrs := lkNew.Participant.Attributes
	if attrs == nil {
		attrs = make(map[string]string)
	}

	sipCallID := attrs[livekit.AttrSIPCallID]
	if sipCallID != "" {
		c.c.RegisterTransferSIPParticipant(sipCallID, c)
	}

	attrs[AttrSIPCallStatus] = string(CallDialing)
	lkNew.Participant.Attributes = attrs
	r := NewRoom(c.log)
	if err := r.Connect(c.c.conf, lkNew); err != nil {
		return err
	}
	// We have to create the track early because we might play a ringtone while SIP connects.
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

func (c *outboundCall) dialSIP(ctx context.Context) error {
	if c.sipConf.ringtone {
		const ringVolume = math.MaxInt16 / 2
		rctx, rcancel := context.WithCancel(ctx)
		defer rcancel()

		// Play a ringtone to the room while participant connects
		go func() {
			rctx, span := tracer.Start(rctx, "tones.Play")
			defer span.End()
			tones.Play(rctx, c.lkRoomIn, ringVolume, tones.ETSIRinging)
		}()
	}
	err := c.sipSignal(ctx)
	if err != nil {
		// TODO: busy, no-response
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

	c.sipRunning = true
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

func sipResponse(tx sip.ClientTransaction, stop <-chan struct{}) (*sip.Response, error) {
	cnt := 0
	for {
		select {
		case <-stop:
			return nil, psrpc.NewErrorf(psrpc.Canceled, "canceled")
		case <-tx.Done():
			return nil, psrpc.NewErrorf(psrpc.Canceled, "transaction failed to complete (%d intermediate responses)", cnt)
		case res := <-tx.Responses():
			switch res.StatusCode {
			default:
				return res, nil
			case 100, 180, 183:
				// continue
				cnt++
			}
		}
	}
}

func (c *outboundCall) stopSIP(reason string) {
	c.mon.CallTerminate(reason)
	c.cc.Close()
	c.sipRunning = false
}

func (c *outboundCall) setStatus(v CallStatus) {
	r := c.lkRoom.Room()
	if r == nil {
		return
	}
	r.LocalParticipant.SetAttributes(map[string]string{
		AttrSIPCallStatus: string(v),
	})
}

func (c *outboundCall) sipSignal(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "outboundCall.sipSignal")
	defer span.End()

	sdpOffer, err := c.media.NewOffer()
	if err != nil {
		return err
	}
	c.mon.SDPSize(len(sdpOffer), true)
	c.log.Debugw("SDP offer", "sdp", string(sdpOffer))
	joinDur := c.mon.JoinDur()

	c.mon.InviteReq()
	sdpResp, err := c.cc.Invite(ctx, c.sipConf.transport, URI{
		User: c.sipConf.to,
		Host: c.sipConf.address,
	}, c.sipConf.user, c.sipConf.pass, c.sipConf.headers, sdpOffer)
	if err != nil {
		// TODO: should we retry? maybe new offer will work
		var e *ErrorStatus
		if errors.As(err, &e) {
			c.mon.InviteError(fmt.Sprintf("status-%d", e.StatusCode))
		} else {
			c.mon.InviteError("other")
		}
		c.cc.Close()
		c.log.Infow("SIP invite failed", "error", err)
		return err
	}
	c.mon.SDPSize(len(sdpResp), false)
	c.log.Debugw("SDP answer", "sdp", string(sdpResp))

	c.log = LoggerWithHeaders(c.log, c.cc)

	if err := c.media.SetAnswer(sdpResp); err != nil {
		return err
	}
	c.c.cmu.Lock()
	c.c.byRemote[c.cc.Tag()] = c
	c.c.cmu.Unlock()

	c.mon.InviteAccept()
	err = c.cc.AckInvite(ctx)
	if err != nil {
		c.log.Infow("SIP accept failed", "error", err)
		return err
	}
	joinDur()

	if len(c.cc.RemoteHeaders()) != 0 {
		extra := HeadersToAttrs(nil, c.sipConf.headersToAttrs, c.cc)
		if c.lkRoom != nil && len(extra) != 0 {
			room := c.lkRoom.Room()
			if room != nil {
				room.LocalParticipant.SetAttributes(extra)
			} else {
				c.log.Warnw("could not set attributes on nil room", nil, "attrs", extra)
			}
		}
	}
	return nil
}

func (c *outboundCall) handleDTMF(ev dtmf.Event) {
	_ = c.lkRoom.SendData(&livekit.SipDTMF{
		Code:  uint32(ev.Code),
		Digit: string([]byte{ev.Digit}),
	}, lksdk.WithDataPublishReliable(true))
}

func (c *outboundCall) transferCall(ctx context.Context, transferTo string) error {
	err := c.cc.transferCall(ctx, transferTo)
	if err != nil {
		return err
	}

	c.log.Infow("outbound l tranferred", "transferTo", transferTo)

	// This is needed to actually terminate the session before a media timeout
	c.CloseWithReason(CallHangup, "call transferred")

	return nil
}

func (c *Client) newOutbound(id LocalTag, from URI) *sipOutbound {
	from = from.Normalize()
	fromHeader := &sip.FromHeader{
		DisplayName: from.User,
		Address:     *from.GetURI(),
		Params:      sip.NewParams(),
	}
	fromHeader.Params.Add("tag", string(id))
	return &sipOutbound{
		c:         c,
		id:        id,
		from:      fromHeader,
		referDone: make(chan error, 1),
	}
}

type sipOutbound struct {
	c    *Client
	id   LocalTag
	from *sip.FromHeader

	mu              sync.RWMutex
	tag             RemoteTag
	invite          *sip.Request
	inviteOk        *sip.Response
	to              *sip.ToHeader
	nextRequestCSeq uint32

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

func (c *sipOutbound) ID() LocalTag {
	return c.id
}

func (c *sipOutbound) Tag() RemoteTag {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tag
}

func (c *sipOutbound) RemoteHeaders() Headers {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.inviteOk == nil {
		return nil
	}
	return c.inviteOk.Headers()
}

func (c *sipOutbound) Invite(ctx context.Context, transport livekit.SIPTransport, to URI, user, pass string, headers map[string]string, sdpOffer []byte) ([]byte, error) {
	ctx, span := tracer.Start(ctx, "sipOutbound.Invite")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	to = to.Normalize()
	toHeader := &sip.ToHeader{Address: *to.GetURI()}
	toHeader.Address.UriParams = make(sip.HeaderParams)
	switch transport {
	case livekit.SIPTransport_SIP_TRANSPORT_UDP:
		toHeader.Address.UriParams.Add("transport", "udp")
	case livekit.SIPTransport_SIP_TRANSPORT_TCP:
		toHeader.Address.UriParams.Add("transport", "tcp")
	}

	dest := to.GetDest()

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
	for {
		req, resp, err = c.attemptInvite(ctx, dest, toHeader, sdpOffer, authHeaderRespName, authHeader, sipHeaders)
		if err != nil {
			return nil, err
		}
		var authHeaderName string
		switch resp.StatusCode {
		case 200:
			break authLoop
		default:
			return nil, fmt.Errorf("unexpected status from INVITE response: %w", &ErrorStatus{StatusCode: int(resp.StatusCode)})
		case 400:
			err := &ErrorStatus{StatusCode: int(resp.StatusCode)}
			if body := resp.Body(); len(body) != 0 {
				err.Message = string(body)
			} else if s := resp.GetHeader("X-Twillio-Error"); s != nil {
				err.Message = s.Value()
			}
			return nil, fmt.Errorf("INVITE failed: %w", err)
		case 401:
			authHeaderName = "WWW-Authenticate"
			authHeaderRespName = "Authorization"
		case 407:
			authHeaderName = "Proxy-Authenticate"
			authHeaderRespName = "Proxy-Authorization"
		}
		// auth required
		if user == "" || pass == "" {
			return nil, errors.New("server required auth, but no username or password was provided")
		}
		headerVal := resp.GetHeader(authHeaderName)
		if headerVal == nil {
			return nil, errors.New("no auth header in response")
		}
		challenge, err := digest.ParseChallenge(headerVal.Value())
		if err != nil {
			return nil, err
		}
		toHeader, ok := resp.To()
		if !ok {
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
	var ok bool
	toHeader, ok = resp.To()
	if !ok {
		return nil, errors.New("no To header in INVITE response")
	}
	c.tag, ok = getTagFrom(toHeader.Params)
	if !ok {
		return nil, errors.New("no tag in To header in INVITE response")
	}
	h, _ := c.invite.CSeq()
	if h != nil {
		c.nextRequestCSeq = h.SeqNo + 1
	}

	if cont, ok := resp.Contact(); ok {
		req.Recipient = &cont.Address
		if req.Recipient.Port == 0 {
			req.Recipient.Port = 5060
		}
	}

	if recordRouteHeader, ok := resp.RecordRoute(); ok {
		req.AppendHeader(&sip.RouteHeader{Address: recordRouteHeader.Address})
	}

	return c.inviteOk.Body(), nil
}

func (c *sipOutbound) AckInvite(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "sipOutbound.AckInvite")
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.sipCli.WriteRequest(sip.NewAckRequest(c.invite, c.inviteOk, nil))
}

func (c *sipOutbound) attemptInvite(ctx context.Context, dest string, to *sip.ToHeader, offer []byte, authHeaderName, authHeader string, headers Headers) (*sip.Request, *sip.Response, error) {
	ctx, span := tracer.Start(ctx, "sipOutbound.attemptInvite")
	defer span.End()
	req := sip.NewRequest(sip.INVITE, &to.Address)
	req.SetDestination(dest)
	req.SetBody(offer)
	req.AppendHeader(to)
	req.AppendHeader(c.from)
	req.AppendHeader(&sip.ContactHeader{Address: c.from.Address})

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

	resp, err := sipResponse(tx, c.c.closing.Watch())
	return req, resp, err
}

func (c *sipOutbound) WriteRequest(req *sip.Request) error {
	return c.c.sipCli.WriteRequest(req)
}

func (c *sipOutbound) Transaction(req *sip.Request) (sip.ClientTransaction, error) {
	return c.c.sipCli.TransactionRequest(req)
}

func (c *sipOutbound) setCSeq(req *sip.Request) {
	setCSeq(req, c.nextRequestCSeq)

	c.nextRequestCSeq++
}

func (c *sipOutbound) sendBye() {
	if c.invite == nil || c.inviteOk == nil {
		return // call wasn't established
	}
	_, span := tracer.Start(context.Background(), "sipOutbound.sendBye")
	defer span.End()
	bye := sip.NewByeRequest(c.invite, c.inviteOk, nil)
	bye.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))
	if c.c.closing.IsBroken() {
		// do not wait for a response
		_ = c.WriteRequest(bye)
		return
	}
	c.setCSeq(bye)
	c.drop()
	sendAndACK(c, bye)
}

func (c *sipOutbound) drop() {
	// TODO: cancel the call?
	c.invite = nil
	c.inviteOk = nil
	c.nextRequestCSeq = 0
}

func (c *sipOutbound) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
}

func (c *sipOutbound) transferCall(ctx context.Context, transferTo string) error {
	c.mu.Lock()

	if c.invite == nil || c.inviteOk == nil {
		c.mu.Unlock()
		return psrpc.NewErrorf(psrpc.FailedPrecondition, "can't transfer non established call") // call wasn't established
	}

	if c.c.closing.IsBroken() {
		c.mu.Unlock()
		return psrpc.NewErrorf(psrpc.FailedPrecondition, "can't transfer hung up call")
	}

	req := NewReferRequest(c.invite, c.inviteOk, transferTo)
	c.setCSeq(req)
	cseq, _ := req.CSeq()

	if cseq == nil {
		c.mu.Unlock()
		return psrpc.NewErrorf(psrpc.Internal, "missing CSeq header in REFER request")
	}
	c.referCseq = cseq.SeqNo
	c.mu.Unlock()

	_, err := sendRefer(c, req, c.c.closing.Watch())
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
	method, cseq, status, err := handleNotify(req)
	if err != nil {
		return err
	}

	switch method {
	case sip.REFER:
		c.mu.RLock()
		defer c.mu.RUnlock()

		if cseq != 0 && cseq != c.referCseq {
			// NOTIFY for a different REFER, skip
			return nil
		}

		switch {
		case status >= 100 && status < 200:
			// still trying
		case status == 200:
			// Success
			select {
			case c.referDone <- nil:
			default:
			}
		default:
			// Failure
			select {
			// TODO be more specific in the reported error
			case c.referDone <- psrpc.NewErrorf(psrpc.Canceled, "call transfer failed"):
			default:
			}
		}
	}
	return nil
}

func (c *sipOutbound) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inviteOk != nil {
		c.sendBye()
	} else {
		c.drop()
	}
}
