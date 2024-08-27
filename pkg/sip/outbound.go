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
	"strconv"
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
	id      LocalTag
	log     logger.Logger
	media   *MediaPort
	stopped core.Fuse

	mu            sync.RWMutex
	mon           *stats.CallMonitor
	lkRoom        *Room
	lkRoomIn      media.PCM16Writer // output to room; OPUS at 48k
	sipConf       sipOutboundConfig
	tag           RemoteTag // empty until we receive a response
	sipInviteReq  *sip.Request
	sipInviteResp *sip.Response
	sipRunning    bool
}

func (c *Client) newCall(conf *config.Config, log logger.Logger, id LocalTag, room RoomConfig, sipConf sipOutboundConfig) (*outboundCall, error) {
	call := &outboundCall{
		c:       c,
		id:      id,
		log:     log,
		sipConf: sipConf,
	}
	call.mon = c.mon.NewCall(stats.Outbound, c.signalingIp, sipConf.address)
	var err error
	call.media, err = NewMediaPort(call.log, call.mon, c.signalingIp, conf.RTPPort, RoomSampleRate)
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
		c.log.Errorw("SIP call failed", err)
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
		delete(c.c.activeCalls, c.id)
		if c.tag != "" {
			delete(c.c.byRemote, c.tag)
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

	c.lkRoom.Subscribe() // TODO: postpone
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
	err := c.sipSignal(c.sipConf)
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
	select {
	case <-tx.Done():
		return nil, fmt.Errorf("transaction failed to complete")
	case res := <-tx.Responses():
		if res.StatusCode == 100 || res.StatusCode == 180 || res.StatusCode == 183 {
			return sipResponse(tx)
		}
		return res, nil
	}
}

func (c *outboundCall) stopSIP(reason string) {
	if c.sipInviteReq != nil {
		if err := c.sipBye(); err != nil {
			c.log.Errorw("SIP bye failed", err)
		}
		c.mon.CallTerminate(reason)
	}
	c.sipInviteReq = nil
	c.sipInviteResp = nil
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

func (c *outboundCall) sipSignal(conf sipOutboundConfig) error {
	offer, err := c.media.NewOffer()
	if err != nil {
		return err
	}
	joinDur := c.mon.JoinDur()
	inviteReq, inviteResp, err := c.sipInvite(offer, conf)
	if inviteResp != nil {
		for hdr, name := range headerToLog {
			if h := inviteResp.GetHeader(hdr); h != nil {
				c.log = c.log.WithValues(name, h.Value())
			}
		}
	}
	if err != nil {
		c.log.Errorw("SIP invite failed", err)
		return err // TODO: should we retry? maybe new offer will work
	}
	c.sipInviteReq, c.sipInviteResp = inviteReq, inviteResp

	err = c.sipAccept(inviteReq, inviteResp)
	if err != nil {
		c.log.Errorw("SIP accept failed", err)
		return err
	}
	if err := c.media.SetAnswer(c.sipInviteResp.Body()); err != nil {
		return err
	}
	joinDur()

	if inviteResp != nil {
		extra := make(map[string]string)
		for hdr, name := range headerToAttr {
			if h := inviteResp.GetHeader(hdr); h != nil {
				extra[name] = h.Value()
			}
		}
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

func (c *outboundCall) sipAttemptInvite(offer []byte, conf sipOutboundConfig, authHeader string) (*sip.Request, *sip.Response, error) {
	c.mon.InviteReq()

	dest := conf.address + ":5060"
	to := &sip.Uri{User: conf.to, Host: conf.address, Port: 5060, UriParams: make(sip.HeaderParams)}
	switch conf.transport {
	case livekit.SIPTransport_SIP_TRANSPORT_UDP:
		to.UriParams.Add("transport", "udp")
	case livekit.SIPTransport_SIP_TRANSPORT_TCP:
		to.UriParams.Add("transport", "tcp")
	}
	if addr, sport, err := net.SplitHostPort(conf.address); err == nil {
		if port, err := strconv.Atoi(sport); err == nil {
			to.Host = addr
			to.Port = port
			dest = conf.address
		}
	}
	from := &sip.Uri{User: conf.from, Host: c.c.signalingIp}
	if c.c.conf.SIPPort != 5060 {
		from.Port = c.c.conf.SIPPort
	}

	fromHeader := &sip.FromHeader{Address: *from, DisplayName: conf.from, Params: sip.NewParams()}
	fromHeader.Params.Add("tag", string(c.id))

	req := sip.NewRequest(sip.INVITE, to)
	req.SetDestination(dest)
	req.SetBody(offer)
	req.AppendHeader(&sip.ToHeader{Address: *to})
	req.AppendHeader(fromHeader)
	req.AppendHeader(&sip.ContactHeader{Address: *from})
	// TODO: This issue came out when we tried creating separate UA for the server and the client.
	//       So we might need to set Via explicitly. Anyway, UA must be shared for other reasons,
	//       and the calls work without explicit Via. So keep it simple, and let SIPGO set Via for now.
	//       Code will remain here for the future reference/refactoring.
	if false {
		if c.c.signalingIp != c.c.signalingIpLocal {
			// SIPGO will use Via/From headers to figure out which interface to listen on, which will obviously fail
			// in case we specify our external IP in there. So we must explicitly add a Via with our local IP.
			params := sip.NewParams()
			params["branch"] = sip.GenerateBranch()
			req.AppendHeader(&sip.ViaHeader{
				ProtocolName:    "SIP",
				ProtocolVersion: "2.0",
				Transport:       req.Transport(),
				Host:            c.c.signalingIpLocal,
				Params:          params,
			})
		}
		params := sip.NewParams()
		params["branch"] = sip.GenerateBranch()
		req.AppendHeader(&sip.ViaHeader{
			ProtocolName:    "SIP",
			ProtocolVersion: "2.0",
			Transport:       req.Transport(),
			Host:            c.c.signalingIp,
			Params:          params,
		})
	}
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authHeader != "" {
		req.AppendHeader(sip.NewHeader("Proxy-Authorization", authHeader))
	}

	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		c.mon.InviteError("tx-failed")
		return nil, nil, err
	}
	defer tx.Terminate()

	resp, err := sipResponse(tx)
	if err != nil {
		c.mon.InviteError("tx-failed")
	}
	return req, resp, err
}

func (c *outboundCall) sipInvite(offer []byte, conf sipOutboundConfig) (*sip.Request, *sip.Response, error) {
	authHeader := ""
	for {
		req, resp, err := c.sipAttemptInvite(offer, conf, authHeader)
		if err != nil {
			return nil, nil, err
		}
		switch resp.StatusCode {
		default:
			c.mon.InviteError(fmt.Sprintf("status-%d", resp.StatusCode))
			return nil, resp, fmt.Errorf("Unexpected StatusCode from INVITE response %d", resp.StatusCode)
		case 400:
			c.mon.InviteError("status-400")
			var reason string
			if body := resp.Body(); len(body) != 0 {
				reason = string(body)
			} else if s := resp.GetHeader("X-Twillio-Error"); s != nil {
				reason = s.Value()
			}
			if reason != "" {
				return nil, resp, fmt.Errorf("INVITE failed: %s", reason)
			}
			return nil, resp, fmt.Errorf("INVITE failed with status %d", resp.StatusCode)
		case 200:
			c.mon.InviteAccept()
			return req, resp, nil
		case 407:
			// auth required
			c.mon.InviteError("auth-required")
		}
		if conf.user == "" || conf.pass == "" {
			return nil, resp, fmt.Errorf("Server responded with 407, but no username or password was provided")
		}
		headerVal := resp.GetHeader("Proxy-Authenticate")
		challenge, err := digest.ParseChallenge(headerVal.Value())
		if err != nil {
			return nil, resp, err
		}

		toHeader, ok := resp.To()
		if !ok {
			return nil, resp, fmt.Errorf("No To Header on Request")
		}

		cred, err := digest.Digest(challenge, digest.Options{
			Method:   req.Method.String(),
			URI:      toHeader.Address.String(),
			Username: conf.user,
			Password: conf.pass,
		})
		if err != nil {
			return nil, resp, err
		}
		authHeader = cred.String()
		// Try again with a computed digest
	}
}

func (c *outboundCall) sipAccept(inviteReq *sip.Request, inviteResp *sip.Response) error {
	tag, err := getToTag(inviteResp)
	if err != nil {
		return err
	}
	c.tag = tag
	c.c.cmu.Lock()
	c.c.byRemote[tag] = c
	c.c.cmu.Unlock()

	if cont, ok := inviteResp.Contact(); ok {
		inviteReq.Recipient = &cont.Address
		if inviteReq.Recipient.Port == 0 {
			inviteReq.Recipient.Port = 5060
		}
	}

	if recordRouteHeader, ok := inviteResp.RecordRoute(); ok {
		inviteReq.AppendHeader(&sip.RouteHeader{Address: recordRouteHeader.Address})
	}

	return c.c.sipCli.WriteRequest(sip.NewAckRequest(inviteReq, inviteResp, nil))
}

func (c *outboundCall) sipBye() error {
	req := sip.NewByeRequest(c.sipInviteReq, c.sipInviteResp, nil)
	c.sipInviteReq.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))

	if c.c.closing.IsBroken() {
		// do not wait for a response
		_ = c.c.sipCli.TransportLayer().WriteMsg(req)
		return nil
	}
	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		return err
	}
	defer tx.Terminate()
	r, err := sipResponse(tx)
	if err != nil {
		return err
	}
	if r.StatusCode == 200 {
		_ = c.c.sipCli.WriteRequest(sip.NewAckRequest(req, r, nil))
	}
	return nil
}

func (c *outboundCall) handleDTMF(ev dtmf.Event) {
	_ = c.lkRoom.SendData(&livekit.SipDTMF{
		Code:  uint32(ev.Code),
		Digit: string([]byte{ev.Digit}),
	}, lksdk.WithDataPublishReliable(true))
}
