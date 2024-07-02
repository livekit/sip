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

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"golang.org/x/exp/maps"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type Client struct {
	conf *config.Config
	log  logger.Logger
	mon  *stats.Monitor

	sipCli           *sipgo.Client
	signalingIp      string
	signalingIpLocal string

	closing     core.Fuse
	cmu         sync.Mutex
	activeCalls map[*outboundCall]struct{}
}

func NewClient(conf *config.Config, log logger.Logger, mon *stats.Monitor) *Client {
	if log == nil {
		log = logger.GetLogger()
	}
	c := &Client{
		conf:        conf,
		log:         log,
		mon:         mon,
		activeCalls: make(map[*outboundCall]struct{}),
	}
	return c
}

func (c *Client) Start(agent *sipgo.UserAgent) error {
	var err error
	if c.conf.UseExternalIP {
		if c.signalingIp, err = getPublicIP(); err != nil {
			return err
		}
		if c.signalingIpLocal, err = getLocalIP(c.conf.LocalNet); err != nil {
			return err
		}
	} else if c.conf.NAT1To1IP != "" {
		c.signalingIp = c.conf.NAT1To1IP
		c.signalingIpLocal = c.signalingIp
	} else {
		if c.signalingIp, err = getLocalIP(c.conf.LocalNet); err != nil {
			return err
		}
		c.signalingIpLocal = c.signalingIp
	}
	c.log.Infow("client starting", "local", c.signalingIpLocal, "external", c.signalingIp)

	if agent == nil {
		ua, err := sipgo.NewUA(
			sipgo.WithUserAgent(UserAgent),
		)
		if err != nil {
			return err
		}
		agent = ua
	}

	c.sipCli, err = sipgo.NewClient(agent, sipgo.WithClientHostname(c.signalingIp))
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) Stop() {
	c.closing.Break()
	c.cmu.Lock()
	calls := maps.Keys(c.activeCalls)
	c.activeCalls = make(map[*outboundCall]struct{})
	c.cmu.Unlock()
	for _, call := range calls {
		call.Close()
	}
	if c.sipCli != nil {
		c.sipCli.Close()
		c.sipCli = nil
	}
}

func (c *Client) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	if req.CallTo == "" {
		return nil, fmt.Errorf("call-to number must be set")
	} else if req.Address == "" {
		return nil, fmt.Errorf("trunk adresss must be set")
	} else if req.Number == "" {
		return nil, fmt.Errorf("trunk outbound number must be set")
	} else if req.RoomName == "" {
		return nil, fmt.Errorf("room name must be set")
	}
	log := c.log.WithValues(
		"callID", req.SipCallId,
		"roomName", req.RoomName, "identity", req.ParticipantIdentity, "name", req.ParticipantName,
		"fromUser", req.Number,
		"toHost", req.Address, "toUser", req.CallTo,
	)
	log.Infow("Creating SIP participant")
	call, err := c.newCall(c.conf, log, req.SipCallId, lkRoomConfig{
		roomName: req.RoomName,
		identity: req.ParticipantIdentity,
		name:     req.ParticipantName,
		meta:     req.ParticipantMetadata,
		attrs:    req.ParticipantAttributes,
		wsUrl:    req.WsUrl,
		token:    req.Token,
	})
	if err != nil {
		return nil, err
	}
	// Start actual SIP call async.
	go func() {
		ctx := context.WithoutCancel(ctx)
		err := call.UpdateSIP(ctx, sipOutboundConfig{
			address:   req.Address,
			transport: req.Transport,
			from:      req.Number,
			to:        req.CallTo,
			user:      req.Username,
			pass:      req.Password,
			dtmf:      req.Dtmf,
			ringtone:  req.PlayRingtone,
		})
		if err != nil {
			log.Errorw("SIP call failed", err)
			return
		}
		select {
		case <-call.Disconnected():
			call.CloseWithReason("removed")
		case <-call.Closed():
		}
	}()

	p := call.Participant()
	return &rpc.InternalCreateSIPParticipantResponse{
		ParticipantId:       p.ID,
		ParticipantIdentity: p.Identity,
		SipCallId:           req.SipCallId,
	}, nil
}

func (c *Client) OnRequest(req *sip.Request, tx sip.ServerTransaction) {
	switch req.Method {
	case "BYE":
		c.onBye(req, tx)
	}
}

func (c *Client) onBye(req *sip.Request, tx sip.ServerTransaction) {
	tag, _ := getTagValue(req)
	c.cmu.Lock()
	defer c.cmu.Unlock()

	found := false
	for c := range c.activeCalls {
		toHeader, ok := req.To()
		if !ok {
			continue
		}

		fromHeader, ok := req.From()
		if !ok {
			continue
		}

		if c.sipCur.to == fromHeader.Address.User && c.sipCur.from == toHeader.Address.User {
			found = true
			c.log.Infow("BYE")
			go func(call *outboundCall) {
				call.CloseWithReason("bye")
			}(c)
		}
	}
	if !found {
		c.log.Infow("BYE", "sipTag", tag)
	}
}

func (c *Client) CreateSIPParticipantAffinity(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) float32 {
	// TODO: scale affinity based on a number or active calls?
	return 0.5
}
