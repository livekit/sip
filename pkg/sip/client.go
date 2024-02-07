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
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"golang.org/x/exp/maps"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type Client struct {
	conf *config.Config
	mon  *stats.Monitor

	sipCli      *sipgo.Client
	signalingIp string

	cmu         sync.Mutex
	activeCalls map[*outboundCall]struct{}
}

func NewClient(conf *config.Config, mon *stats.Monitor) *Client {
	c := &Client{
		conf:        conf,
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
	} else if c.conf.NAT1To1IP != "" {
		c.signalingIp = c.conf.NAT1To1IP
	} else {
		if c.signalingIp, err = getLocalIP(); err != nil {
			return err
		}
	}

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
	logger.Infow("Creating SIP participant",
		"roomName", req.RoomName, "from", req.Number, "to", req.CallTo, "address", req.Address)
	call, err := c.newCall(c.conf, lkRoomConfig{
		roomName: req.RoomName,
		identity: req.ParticipantIdentity,
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
			address: req.Address,
			from:    req.Number,
			to:      req.CallTo,
			user:    req.Username,
			pass:    req.Password,
		})
		if err != nil {
			logger.Errorw("SIP call failed", err,
				"roomName", req.RoomName, "from", req.Number, "to", req.CallTo, "address", req.Address)
			return
		}
		select {
		case <-call.Disconnected():
			call.CloseWithReason("removed")
		case <-call.Closed():
		}
	}()

	p := call.Participant()
	return &rpc.InternalCreateSIPParticipantResponse{ParticipantId: p.ID, ParticipantIdentity: p.Identity}, nil
}

func (c *Client) CreateSIPParticipantAffinity(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) float32 {
	// TODO: scale affinity based on a number or active calls?
	return 0.5
}
