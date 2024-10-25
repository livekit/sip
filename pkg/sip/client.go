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
	"net/netip"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"golang.org/x/exp/maps"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/tracer"

	"github.com/livekit/sip/pkg/config"
	siperrors "github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/stats"
)

type Client struct {
	conf *config.Config
	log  logger.Logger
	mon  *stats.Monitor

	sipCli           *sipgo.Client
	signalingIp      netip.Addr
	signalingIpLocal netip.Addr

	closing     core.Fuse
	cmu         sync.Mutex
	activeCalls map[LocalTag]*outboundCall
	byRemote    map[RemoteTag]*outboundCall

	handler Handler
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
		ip, err := netip.ParseAddr(c.conf.NAT1To1IP)
		if err != nil {
			return err
		}
		c.signalingIp = ip
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
		sipgo.WithClientHostname(c.signalingIp.String()),
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

func (c *Client) SetHandler(handler Handler) {
	c.handler = handler
}

func (c *Client) ContactURI(tr Transport) URI {
	return getContactURI(c.conf, c.signalingIp, tr)
}

func (c *Client) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	ctx, span := tracer.Start(ctx, "Client.CreateSIPParticipant")
	defer span.End()
	return c.createSIPParticipant(ctx, req)
}

func (c *Client) createSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	if !c.mon.CanAccept() {
		return nil, siperrors.ErrUnavailable
	}
	if req.CallTo == "" {
		return nil, fmt.Errorf("call-to number must be set")
	} else if req.Address == "" {
		return nil, fmt.Errorf("trunk adresss must be set")
	} else if req.Number == "" {
		return nil, fmt.Errorf("trunk outbound number must be set")
	} else if req.RoomName == "" {
		return nil, fmt.Errorf("room name must be set")
	}
	log := c.log
	if req.ProjectId != "" {
		log = log.WithValues("projectID", req.ProjectId)
	}
	if req.SipTrunkId != "" {
		log = log.WithValues("sipTrunk", req.SipTrunkId)
	}
	log = log.WithValues(
		"callID", req.SipCallId,
		"room", req.RoomName,
		"participant", req.ParticipantIdentity,
		"participantName", req.ParticipantName,
		"fromHost", req.Hostname,
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
		address:         req.Address,
		transport:       req.Transport,
		host:            req.Hostname,
		from:            req.Number,
		to:              req.CallTo,
		user:            req.Username,
		pass:            req.Password,
		dtmf:            req.Dtmf,
		dialtone:        req.PlayDialtone,
		headers:         req.Headers,
		headersToAttrs:  req.HeadersToAttributes,
		ringingTimeout:  req.RingingTimeout.AsDuration(),
		maxCallDuration: req.MaxCallDuration.AsDuration(),
		enabledFeatures: req.EnabledFeatures,
	}
	log.Infow("Creating SIP participant")
	call, err := c.newCall(ctx, c.conf, log, LocalTag(req.SipCallId), roomConf, sipConf)
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
	case "NOTIFY":
		return c.onNotify(req, tx)
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

func (c *Client) onNotify(req *sip.Request, tx sip.ServerTransaction) bool {
	tag, _ := getFromTag(req)
	c.cmu.Lock()
	call := c.byRemote[tag]
	c.cmu.Unlock()
	if call == nil {
		return false
	}
	call.log.Infow("NOTIFY")
	go func() {
		err := call.cc.handleNotify(req, tx)

		code, msg := sipCodeAndMessageFromError(err)

		tx.Respond(sip.NewResponseFromRequest(req, code, msg, nil))
	}()
	return true
}

func (c *Client) RegisterTransferSIPParticipant(sipCallID string, o *outboundCall) error {
	return c.handler.RegisterTransferSIPParticipantTopic(sipCallID)
}

func (c *Client) DeregisterTransferSIPParticipant(sipCallID string) {
	c.handler.DeregisterTransferSIPParticipantTopic(sipCallID)
}
