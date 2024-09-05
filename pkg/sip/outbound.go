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
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/tones"
	"github.com/livekit/sip/pkg/stats"
)

type sipOutboundConfig struct {
	address   string
	transport livekit.SIPTransport
	from      string
	to        string
	user      string
	pass      string
	dtmf      string
	ringtone  bool
}

type outboundCall struct {
	c       *Client
	log     logger.Logger
	cc      *sipOutbound
	media   *MediaPort
	stopped core.Fuse

	mu         sync.RWMutex
	mon        *stats.CallMonitor
	lkRoom     *Room
	lkRoomIn   media.PCM16Writer // output to room; OPUS at 48k
	sipConf    sipOutboundConfig
	sipRunning bool
}

func (c *Client) newCall(conf *config.Config, log logger.Logger, id LocalTag, room RoomConfig, sipConf sipOutboundConfig) (*outboundCall, error) {
	call := &outboundCall{
		c:   c,
		log: log,
		cc: c.newOutbound(id, URI{
			User: sipConf.from,
			Host: c.signalingIp,
			Addr: netip.AddrPortFrom(netip.Addr{}, uint16(conf.SIPPort)),
		}),
		sipConf: sipConf,
	}
	call.mon = c.mon.NewCall(stats.Outbound, c.signalingIp, sipConf.address)
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
	if err := call.connectToRoom(room); err != nil {
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
	})
}

func (c *outboundCall) Participant() ParticipantInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lkRoom.Participant()
}

func (c *outboundCall) ConnectSIP(ctx context.Context) error {
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

func (c *outboundCall) connectToRoom(lkNew RoomConfig) error {
	attrs := lkNew.Participant.Attributes
	if attrs == nil {
		attrs = make(map[string]string)
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
		go tones.Play(rctx, c.lkRoomIn, ringVolume, tones.ETSIRinging)
	}
	err := c.sipSignal()
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

func sipResponse(tx sip.ClientTransaction) (*sip.Response, error) {
	cnt := 0
	for {
		select {
		case <-tx.Done():
			return nil, fmt.Errorf("transaction failed to complete (%d intermediate responses)", cnt)
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

func (c *outboundCall) sipSignal() error {
	sdpOffer, err := c.media.NewOffer()
	if err != nil {
		return err
	}
	joinDur := c.mon.JoinDur()

	c.mon.InviteReq()
	sdpResp, err := c.cc.Invite(c.sipConf.transport, URI{
		User: c.sipConf.to,
		Host: c.sipConf.address,
	}, c.sipConf.user, c.sipConf.pass, sdpOffer)
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

	c.log = LoggerWithHeaders(c.log, c.cc)

	if err := c.media.SetAnswer(sdpResp); err != nil {
		return err
	}
	c.c.cmu.Lock()
	c.c.byRemote[c.cc.Tag()] = c
	c.c.cmu.Unlock()

	c.mon.InviteAccept()
	err = c.cc.AckInvite()
	if err != nil {
		c.log.Infow("SIP accept failed", "error", err)
		return err
	}
	joinDur()

	if len(c.cc.RemoteHeaders()) != 0 {
		extra := HeadersToAttrs(nil, c.cc)
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

func (c *Client) newOutbound(id LocalTag, from URI) *sipOutbound {
	from = from.Normalize()
	fromHeader := &sip.FromHeader{
		DisplayName: from.User,
		Address:     *from.GetURI(),
		Params:      sip.NewParams(),
	}
	fromHeader.Params.Add("tag", string(id))
	return &sipOutbound{
		c:    c,
		id:   id,
		from: fromHeader,
	}
}

type sipOutbound struct {
	c    *Client
	id   LocalTag
	from *sip.FromHeader

	mu       sync.RWMutex
	tag      RemoteTag
	invite   *sip.Request
	inviteOk *sip.Response
	to       *sip.ToHeader
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

func (c *sipOutbound) Invite(transport livekit.SIPTransport, to URI, user, pass string, sdpOffer []byte) ([]byte, error) {
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
		authHeader = ""
		req        *sip.Request
		resp       *sip.Response
		err        error
	)
authLoop:
	for {
		req, resp, err = c.attemptInvite(dest, toHeader, sdpOffer, authHeader)
		if err != nil {
			return nil, err
		}
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
		case 407:
			// auth required
		}
		if user == "" || pass == "" {
			return nil, errors.New("server required auth, but no username or password was provided")
		}
		headerVal := resp.GetHeader("Proxy-Authenticate")
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

func (c *sipOutbound) AckInvite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.sipCli.WriteRequest(sip.NewAckRequest(c.invite, c.inviteOk, nil))
}

func (c *sipOutbound) attemptInvite(dest string, to *sip.ToHeader, offer []byte, authHeader string) (*sip.Request, *sip.Response, error) {
	req := sip.NewRequest(sip.INVITE, &to.Address)
	req.SetDestination(dest)
	req.SetBody(offer)
	req.AppendHeader(to)
	req.AppendHeader(c.from)
	req.AppendHeader(&sip.ContactHeader{Address: c.from.Address})

	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authHeader != "" {
		req.AppendHeader(sip.NewHeader("Proxy-Authorization", authHeader))
	}

	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Terminate()

	resp, err := sipResponse(tx)
	return req, resp, err
}

func (c *sipOutbound) WriteRequest(req *sip.Request) error {
	return c.c.sipCli.WriteRequest(req)
}

func (c *sipOutbound) Transaction(req *sip.Request) (sip.ClientTransaction, error) {
	return c.c.sipCli.TransactionRequest(req)
}

func (c *sipOutbound) sendBye() {
	if c.invite == nil || c.inviteOk == nil {
		return // call wasn't established
	}
	bye := sip.NewByeRequest(c.invite, c.inviteOk, nil)
	bye.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))
	if c.c.closing.IsBroken() {
		// do not wait for a response
		_ = c.WriteRequest(bye)
		return
	}
	c.drop()
	sendBye(c, bye)
}

func (c *sipOutbound) drop() {
	// TODO: cancel the call?
	c.invite = nil
	c.inviteOk = nil
}

func (c *sipOutbound) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
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
