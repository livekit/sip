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
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/ulaw"
)

type sipOutboundConfig struct {
	address string
	from    string
	to      string
	user    string
	pass    string
}

type outboundCall struct {
	c             *Client
	participantID string
	rtpConn       *MediaConn

	mu            sync.RWMutex
	mediaRunning  bool
	lkCur         lkRoomConfig
	lkRoom        *Room
	lkRoomIn      media.Writer[media.PCM16Sample]
	sipCur        sipOutboundConfig
	sipInviteReq  *sip.Request
	sipInviteResp *sip.Response
	sipRunning    bool
}

func (c *Client) getCall(participantId string) *outboundCall {
	c.cmu.RLock()
	defer c.cmu.RUnlock()
	return c.activeCalls[participantId]
}

func (c *Client) getOrCreateCall(participantId string) *outboundCall {
	// Fast path
	if call := c.getCall(participantId); call != nil {
		return call
	}
	// Slow path
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if call := c.activeCalls[participantId]; call != nil {
		return call
	}
	call := c.newCall(participantId)
	c.activeCalls[participantId] = call
	return call
}

func (c *Client) newCall(participantId string) *outboundCall {
	call := &outboundCall{
		c:             c,
		participantID: participantId,
		rtpConn:       NewMediaConn(),
	}
	return call
}

func (c *outboundCall) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close()
	return nil
}

func (c *outboundCall) close() {
	c.rtpConn.OnRTP(nil)
	c.lkRoom.SetOutput(nil)

	if c.mediaRunning {
		_ = c.rtpConn.Close()
	}
	c.mediaRunning = false

	if c.lkRoom != nil {
		_ = c.lkRoom.Close()
	}
	c.lkRoom = nil
	c.lkRoomIn = nil
	c.lkCur = lkRoomConfig{}

	c.stopSIP()
	c.sipCur = sipOutboundConfig{}

	// FIXME: remove call from the client map?
}

func (c *outboundCall) Update(ctx context.Context, sipNew sipOutboundConfig, lkNew lkRoomConfig) error {
	c.mu.RLock()
	sipCur, lkCur := c.sipCur, c.lkCur
	c.mu.RUnlock()
	if sipCur == sipNew && lkCur == lkNew {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sipCur == sipNew && c.lkCur == lkNew {
		return nil
	}
	if sipNew.address == "" || sipNew.to == "" {
		logger.Infow("Shutdown of outbound SIP call",
			"roomName", lkNew.roomName, "from", sipNew.from, "to", sipNew.to, "address", sipNew.address)
		// shutdown the call
		c.close()
		return nil
	}
	if err := c.startMedia(); err != nil {
		c.close()
		return fmt.Errorf("start media failed: %w", err)
	}
	if err := c.updateRoom(lkNew); err != nil {
		c.close()
		return fmt.Errorf("update room failed: %w", err)
	}
	if err := c.updateSIP(sipNew); err != nil {
		c.close()
		return fmt.Errorf("update SIP failed: %w", err)
	}
	c.relinkMedia()
	logger.Infow("Outbound SIP update complete",
		"roomName", lkNew.roomName, "from", sipNew.from, "to", sipNew.to, "address", sipNew.address)
	return nil
}

func (c *outboundCall) startMedia() error {
	if c.mediaRunning {
		return nil
	}
	if err := c.rtpConn.Start("0.0.0.0"); err != nil {
		return err
	}
	c.mediaRunning = true
	return nil
}

func (c *outboundCall) updateRoom(lkNew lkRoomConfig) error {
	if c.lkRoom != nil && c.lkCur == lkNew {
		return nil
	}
	if c.lkRoom != nil {
		_ = c.lkRoom.Close()
		c.lkRoom = nil
		c.lkRoomIn = nil
	}
	r, err := ConnectToRoom(c.c.conf, lkNew.roomName, lkNew.identity)
	if err != nil {
		return err
	}
	local, err := r.NewParticipant()
	if err != nil {
		_ = r.Close()
		return err
	}
	c.lkRoom = r
	c.lkRoomIn = local
	c.lkCur = lkNew
	return nil
}

func (c *outboundCall) updateSIP(sipNew sipOutboundConfig) error {
	if c.sipCur == sipNew {
		return nil
	}
	c.stopSIP()
	if err := c.sipSignal(sipNew); err != nil {
		return err
	}
	c.sipRunning = true
	c.sipCur = sipNew
	return nil
}

func (c *outboundCall) relinkMedia() {
	if c.lkRoom == nil || !c.mediaRunning {
		c.lkRoom.SetOutput(nil)
		c.rtpConn.OnRTP(nil)
		return
	}
	// Encoding pipeline (LK -> SIP)
	s := rtp.NewMediaStreamOut[ulaw.Sample](c.rtpConn, rtpPacketDur)
	c.lkRoom.SetOutput(ulaw.Decode(s))

	// Decoding pipeline (SIP -> LK)
	lpcm := media.DecodePCM(c.lkRoomIn)
	law := ulaw.Encode(lpcm)
	c.rtpConn.OnRTP(rtp.NewMediaStreamIn(law))
}

func (c *outboundCall) SendDTMF(ctx context.Context, digits string) error {
	c.mu.RLock()
	running := c.mediaRunning
	c.mu.RUnlock()
	if !running {
		return fmt.Errorf("call is not active")
	}
	// FIXME: c.media.WriteRTP()
	return nil
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

func (c *outboundCall) stopSIP() {
	if c.sipInviteReq != nil {
		c.sipBye()
	}
	c.sipInviteReq = nil
	c.sipInviteResp = nil
	c.sipCur = sipOutboundConfig{}
	c.sipRunning = false
}

func (c *outboundCall) sipSignal(conf sipOutboundConfig) error {
	offer, err := sdpGenerateOffer(c.c.publicIp, c.rtpConn.LocalAddr().Port)
	if err != nil {
		return err
	}
	inviteReq, inviteResp, err := c.sipInvite(offer, conf)
	if err != nil {
		logger.Errorw("SIP invite failed", err)
		return err // TODO: should we retry? maybe new offer will work
	}
	err = c.sipAccept(inviteReq, inviteResp)
	if err != nil {
		logger.Errorw("SIP accept failed", err)
		return err
	}
	c.sipInviteReq, c.sipInviteResp = inviteReq, inviteResp
	return nil
}

func (c *outboundCall) sipAttemptInvite(offer []byte, conf sipOutboundConfig, authHeader string) (*sip.Request, *sip.Response, error) {
	req := sip.NewRequest(sip.INVITE, &sip.Uri{User: conf.to, Host: conf.address})
	req.SetDestination("") // FIXME: what should be here
	req.SetBody(offer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:livekit@%s:5060>", c.c.publicIp)))
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

func (c *outboundCall) sipInvite(offer []byte, conf sipOutboundConfig) (*sip.Request, *sip.Response, error) {
	authHeader := ""
	for {
		req, resp, err := c.sipAttemptInvite(offer, conf, authHeader)
		if err != nil {
			return nil, nil, err
		} else if resp.StatusCode == 200 {
			return req, resp, nil
		} else if resp.StatusCode != 407 {
			return nil, nil, fmt.Errorf("Unexpected StatusCode from INVITE response %d", resp.StatusCode)
		}
		if conf.user == "" || conf.pass == "" {
			return nil, nil, fmt.Errorf("Server responded with 407, but no username or password was provided")
		}

		headerVal := resp.GetHeader("Proxy-Authenticate")
		challenge, err := digest.ParseChallenge(headerVal.Value())
		if err != nil {
			return nil, nil, err
		}

		toHeader, ok := resp.To()
		if !ok {
			return nil, nil, fmt.Errorf("No To Header on Request")
		}

		cred, err := digest.Digest(challenge, digest.Options{
			Method:   req.Method.String(),
			URI:      toHeader.Address.String(),
			Username: conf.user,
			Password: conf.pass,
		})
		if err != nil {
			return nil, nil, err
		}
		authHeader = cred.String()
		// Try again with a computed digest
	}
}

func (c *outboundCall) sipAccept(inviteReq *sip.Request, inviteResp *sip.Response) error {
	if cont, ok := inviteResp.Contact(); ok {
		inviteReq.Recipient = &cont.Address
		inviteReq.Recipient.Port = 5060
	}
	return c.c.sipCli.WriteRequest(sip.NewAckRequest(inviteReq, inviteResp, nil))
}

func (c *outboundCall) sipBye() error {
	req := sip.NewByeRequest(c.sipInviteReq, c.sipInviteResp, nil)
	c.sipInviteReq.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))

	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}
