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
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"golang.org/x/exp/maps"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/traceid"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	siperrors "github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/stats"
)

// An interface mirroring sipgo.Client to be able to mock it in tests.
// Note: *sipgo.Client implements this interface directly, so no wrapper is needed.
type SIPClient interface {
	TransactionRequest(req *sip.Request, options ...sipgo.ClientRequestOption) (sip.ClientTransaction, error)
	WriteRequest(req *sip.Request, options ...sipgo.ClientRequestOption) error
	Close() error
}

type GetSipClientFunc func(ua *sipgo.UserAgent, options ...sipgo.ClientOption) (SIPClient, error)

func DefaultGetSipClientFunc(ua *sipgo.UserAgent, options ...sipgo.ClientOption) (SIPClient, error) {
	return sipgo.NewClient(ua, options...)
}

type Client struct {
	conf   *config.Config
	sconf  *ServiceConfig
	log    logger.Logger
	region string
	mon    *stats.Monitor

	sipCli SIPClient

	closing     core.Fuse
	cmu         sync.Mutex
	activeCalls map[LocalTag]*outboundCall
	byRemote    map[RemoteTag]*outboundCall

	handler      Handler
	getIOClient  GetIOInfoClient
	getSipClient GetSipClientFunc
	getRoom      GetRoomFunc
}

type ClientOption func(c *Client)

func WithGetSipClient(fn GetSipClientFunc) ClientOption {
	return func(c *Client) {
		if fn != nil {
			c.getSipClient = fn
		}
	}
}

func WithGetRoomClient(fn GetRoomFunc) ClientOption {
	return func(c *Client) {
		if fn != nil {
			c.getRoom = fn
		}
	}
}

func NewClient(region string, conf *config.Config, log logger.Logger, mon *stats.Monitor, getIOClient GetIOInfoClient, options ...ClientOption) *Client {
	if log == nil {
		log = logger.GetLogger()
	}
	c := &Client{
		conf:         conf,
		log:          log,
		region:       region,
		mon:          mon,
		getIOClient:  getIOClient,
		getSipClient: DefaultGetSipClientFunc,
		getRoom:      DefaultGetRoomFunc,
		activeCalls:  make(map[LocalTag]*outboundCall),
		byRemote:     make(map[RemoteTag]*outboundCall),
	}
	for _, option := range options {
		option(c)
	}
	return c
}

func (c *Client) Start(agent *sipgo.UserAgent, sc *ServiceConfig) error {
	c.sconf = sc
	c.log.Infow("client starting", "local", c.sconf.SignalingIPLocal, "external", c.sconf.SignalingIP)

	if agent == nil {
		ua, err := sipgo.NewUA(
			sipgo.WithUserAgent(UserAgent),
			sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(c.log))),
		)
		if err != nil {
			return err
		}
		agent = ua
	}

	var err error
	c.sipCli, err = c.getSipClient(agent,
		sipgo.WithClientHostname(c.sconf.SignalingIP.String()),
		sipgo.WithClientLogger(slog.New(logger.ToSlogHandler(c.log))),
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) Stop() {
	ctx := context.Background()
	ctx, span := Tracer.Start(ctx, "sip.Client.Stop")
	defer span.End()
	c.closing.Break()
	c.cmu.Lock()
	calls := maps.Values(c.activeCalls)
	c.activeCalls = make(map[LocalTag]*outboundCall)
	c.byRemote = make(map[RemoteTag]*outboundCall)
	c.cmu.Unlock()
	for _, call := range calls {
		call.Close(ctx)
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
	return getContactURI(c.conf, c.sconf.SignalingIP, tr)
}

func (c *Client) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	ctx, span := Tracer.Start(ctx, "Client.CreateSIPParticipant")
	defer span.End()
	return c.createSIPParticipant(ctx, req)
}

func (c *Client) createSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (resp *rpc.InternalCreateSIPParticipantResponse, retErr error) {
	if c.mon.Health() != stats.HealthOK {
		return nil, siperrors.ErrUnavailable
	}
	if req.CallTo == "" {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "call-to number must be set")
	} else if req.Address == "" {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "trunk adresss must be set")
	} else if req.Number == "" {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "trunk outbound number must be set")
	} else if req.RoomName == "" {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "room name must be set")
	}
	if strings.Contains(req.CallTo, "@") {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "call_to should be a phone number or SIP user, not a full SIP URI")
	}
	if strings.HasPrefix(req.Address, "sip:") || strings.HasPrefix(req.Address, "sips:") {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "address must be a hostname without 'sip:' prefix")
	}
	if strings.Contains(req.Address, "transport=") {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "address must not contain parameters; use transport field")
	}
	if strings.ContainsAny(req.Address, ";=") {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "address must not contain parameters")
	}
	log := c.log
	if req.ProjectId != "" {
		log = log.WithValues("projectID", req.ProjectId)
	}
	if req.SipTrunkId != "" {
		log = log.WithValues("sipTrunk", req.SipTrunkId)
	}
	enc, err := sdpEncryption(req.MediaEncryption)
	if err != nil {
		return nil, err
	}
	tid := traceid.FromGUID(req.SipCallId)
	log = log.WithValues(
		"callID", req.SipCallId,
		"traceID", tid.String(),
		"room", req.RoomName,
		"participant", req.ParticipantIdentity,
		"participantName", req.ParticipantName,
		"fromHost", req.Hostname,
		"fromUser", req.Number,
		"toHost", req.Address,
		"toUser", req.CallTo,
	)

	state := NewCallState(c.getIOClient(req.ProjectId), c.createSIPCallInfo(req))

	defer func() {
		state.Update(ctx, func(info *livekit.SIPCallInfo) {

			switch retErr {
			case nil:
				info.CallStatus = livekit.SIPCallStatus_SCS_PARTICIPANT_JOINED
			default:
				info.CallStatus = livekit.SIPCallStatus_SCS_ERROR
				info.DisconnectReason = livekit.DisconnectReason_UNKNOWN_REASON
				info.Error = retErr.Error()
			}
		})
	}()

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
		includeHeaders:  req.IncludeHeaders,
		headersToAttrs:  req.HeadersToAttributes,
		attrsToHeaders:  req.AttributesToHeaders,
		ringingTimeout:  req.RingingTimeout.AsDuration(),
		maxCallDuration: req.MaxCallDuration.AsDuration(),
		enabledFeatures: req.EnabledFeatures,
		featureFlags:    req.FeatureFlags,
		mediaEncryption: enc,
		displayName:     req.DisplayName,
	}
	log.Infow("Creating SIP participant")
	call, err := c.newCall(ctx, tid, c.conf, log, LocalTag(req.SipCallId), roomConf, sipConf, state, req.ProjectId)
	if err != nil {
		return nil, err
	}
	p := call.Participant()
	// Start actual SIP call async.

	info := &rpc.InternalCreateSIPParticipantResponse{
		ParticipantId:       p.ID,
		ParticipantIdentity: p.Identity,
		SipCallId:           req.SipCallId,
	}
	if !req.WaitUntilAnswered {
		call.DialAsync(ctx)
		return info, nil
	}
	if err := call.Dial(ctx); err != nil {
		return nil, err
	}
	go call.WaitClose(context.WithoutCancel(ctx))
	return info, nil
}

func (c *Client) createSIPCallInfo(req *rpc.InternalCreateSIPParticipantRequest) *livekit.SIPCallInfo {
	toUri := CreateURIFromUserAndAddress(req.CallTo, req.Address, TransportFrom(req.Transport))
	fromiUri := URI{
		User: req.Number,
		Host: req.Hostname,
		Addr: netip.AddrPortFrom(c.sconf.SignalingIP, uint16(c.conf.SIPPort)),
	}

	callInfo := &livekit.SIPCallInfo{
		CallId:                req.SipCallId,
		Region:                c.region,
		TrunkId:               req.SipTrunkId,
		RoomName:              req.RoomName,
		ParticipantIdentity:   req.ParticipantIdentity,
		ParticipantAttributes: req.ParticipantAttributes,
		CallDirection:         livekit.SIPCallDirection_SCD_OUTBOUND,
		ToUri:                 toUri.ToSIPUri(),
		FromUri:               fromiUri.ToSIPUri(),
		CreatedAtNs:           time.Now().UnixNano(),
	}

	return callInfo
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
	ctx := context.Background()
	ctx, span := Tracer.Start(ctx, "sip.Client.onBye")
	defer span.End()
	tag, _ := getFromTag(req)
	c.cmu.Lock()
	call := c.byRemote[tag]
	c.cmu.Unlock()
	if call == nil {
		if tag != "" {
			c.log.Infow("BYE for non-existent call", "sipTag", tag)
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call does not exist", nil))
		return false
	}
	call.log.Infow("BYE from remote")
	go func(call *outboundCall) {
		call.cc.AcceptBye(req, tx)
		call.CloseWithReason(ctx, CallHangup, "bye", livekit.DisconnectReason_CLIENT_INITIATED)
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
