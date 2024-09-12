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
	"log/slog"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type rpcCallHandler interface {
	transferCall(ctx context.Context, transferTo string) error
}

type Client struct {
	conf *config.Config
	log  logger.Logger
	mon  *stats.Monitor

	sipCli           *sipgo.Client
	signalingIp      string
	signalingIpLocal string

	closing         core.Fuse
	cmu             sync.Mutex
	activeCalls     map[LocalTag]*outboundCall
	byRemote        map[RemoteTag]*outboundCall
	callIdToHandler map[CallId]rpcCallHandler
}

func NewClient(conf *config.Config, log logger.Logger, mon *stats.Monitor) *Client {
	if log == nil {
		log = logger.GetLogger()
	}
	c := &Client{
		conf:        conf,
		log:         log,
		mon:         mon,
		activeCalls: make(map[LocalTag]*outboundCall),
		byRemote:    make(map[RemoteTag]*outboundCall),
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

	c.sipCli, err = sipgo.NewClient(agent,
		sipgo.WithClientHostname(c.signalingIp),
		sipgo.WithClientLogger(slog.New(logger.ToSlogHandler(c.log))),
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) Stop() {
	c.closing.Break()
	c.cmu.Lock()
	calls := maps.Values(c.activeCalls)
	c.activeCalls = make(map[LocalTag]*outboundCall)
	c.byRemote = make(map[RemoteTag]*outboundCall)
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
		"room", req.RoomName,
		"participant", req.ParticipantIdentity,
		"participantName", req.ParticipantName,
		"fromUser", req.Number,
		"toHost", req.Address,
		"toUser", req.CallTo,
	)
	roomConf := RoomConfig{
		WsUrl:    req.WsUrl,
		Token:    req.Token,
		RoomName: req.RoomName,
		Participant: ParticipantConfig{
			Identity:   req.ParticipantIdentity,
			Name:       req.ParticipantName,
			Metadata:   req.ParticipantMetadata,
			Attributes: req.ParticipantAttributes,
		},
	}
	sipConf := sipOutboundConfig{
		address:   req.Address,
		transport: req.Transport,
		from:      req.Number,
		to:        req.CallTo,
		user:      req.Username,
		pass:      req.Password,
		dtmf:      req.Dtmf,
		ringtone:  req.PlayRingtone,
	}
	log.Infow("Creating SIP participant")
	call, err := c.newCall(c.conf, log, LocalTag(req.SipCallId), roomConf, sipConf)
	if err != nil {
		return nil, err
	}
	p := call.Participant()
	// Start actual SIP call async.
	go call.Start(context.WithoutCancel(ctx))

	return &rpc.InternalCreateSIPParticipantResponse{
		ParticipantId:       p.ID,
		ParticipantIdentity: p.Identity,
		SipCallId:           req.SipCallId,
	}, nil
}

func (c *Client) OnRequest(req *sip.Request, tx sip.ServerTransaction) bool {
	switch req.Method {
	default:
		return false
	case "BYE":
		return c.onBye(req, tx)
	}
}

func (c *Client) onBye(req *sip.Request, tx sip.ServerTransaction) bool {
	tag, _ := getFromTag(req)
	c.cmu.Lock()
	call := c.byRemote[tag]
	c.cmu.Unlock()
	if call == nil {
		return false
	}
	call.log.Infow("BYE")
	go func(call *outboundCall) {
		call.CloseWithReason(CallHangup, "bye")
	}(call)
	return true
}

func (c *Client) CreateSIPParticipantAffinity(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) float32 {
	// TODO: scale affinity based on a number or active calls?
	return 0.5
}

func (c *Client) TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*emptypb.Empty, error) {
	var h rpcCallHandler
	c.cmu.Lock()
	h = c.callIdToHandler[req.SipCallId]
	c.cmu.Unlock()

	if h == nil {
		return nil, psrpc.Errorf(psrpc.NotFound, "unknown call id")
	}

	// FIXME Should this be async?
	err := h.transferCall(ctx, req.TransferTo)
	if err != nil {
		return nil, err
	}

	return nil, &emptypb.Empty{}
}
