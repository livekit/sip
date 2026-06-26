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
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"golang.org/x/exp/maps"

	esip "github.com/emiago/sipgo/sip"

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

// SIPClient is an interface mirroring sipgo.Client to be able to mock it in tests.
//
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

	handler         Handler
	getStateHandler GetStateHandler
	getSipClient    GetSipClientFunc
	getRoom         GetRoomFunc
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

func NewClient(region string, conf *config.Config, log logger.Logger, mon *stats.Monitor, getStateHandler GetStateHandler, options ...ClientOption) *Client {
	if log == nil {
		log = logger.GetLogger()
	}
	c := &Client{
		conf:            conf,
		log:             log,
		region:          region,
		mon:             mon,
		getStateHandler: getStateHandler,
		getSipClient:    DefaultGetSipClientFunc,
		getRoom:         getRoomFuncForConfig(conf),
		activeCalls:     make(map[LocalTag]*outboundCall),
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

func (c *Client) getActiveCall(tag LocalTag) *outboundCall {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.activeCalls[tag]
}

func setUriTransport(p *sip.Uri, tr livekit.SIPTransport) {
	if tr != livekit.SIPTransport_SIP_TRANSPORT_AUTO {
		p.UriParams.Add("transport", tr.Name())
	}
}

func buildLegacyURI(user, addr string, tr livekit.SIPTransport) (*sip.Uri, error) {
	if user == "" {
		return nil, fmt.Errorf("number must be set")
	} else if strings.Contains(user, "@") {
		return nil, fmt.Errorf("should be a phone number or SIP user, not a full SIP URI")
	}
	if addr == "" {
		return nil, fmt.Errorf("address must be set")
	}
	if strings.HasPrefix(addr, "sip:") || strings.HasPrefix(addr, "sips:") {
		return nil, fmt.Errorf("address must be a hostname without 'sip:' prefix")
	} else if strings.Contains(addr, "transport=") {
		return nil, fmt.Errorf("legacy address must not contain parameters; use transport field")
	} else if strings.ContainsAny(addr, ";=") {
		return nil, fmt.Errorf("legacy address must not contain parameters")
	}
	p := &sip.Uri{Scheme: "sip"}
	setUriTransport(p, tr)

	p.User = user
	if host, sport, err := net.SplitHostPort(addr); err == nil && sport != "" {
		p.Host = host
		p.Port, err = strconv.Atoi(sport)
		if err != nil {
			return nil, fmt.Errorf("invalid port in hostname: %q", sport)
		}
	} else {
		p.Host = addr
	}
	return p, nil
}

func buildRawURI(raw string, tr livekit.SIPTransport) (*sip.Uri, error) {
	p := &sip.Uri{Scheme: "sip"}
	if n := len(raw); n != 0 && raw[0] == '<' && raw[n-1] == '>' {
		raw = raw[1 : n-1]
	}
	if err := esip.ParseUri(raw, p); err != nil {
		return nil, errors.New("invalid request URI")
	}
	setUriTransport(p, tr)
	return p, nil
}

func buildValuesURI(u *livekit.SIPUri, tr livekit.SIPTransport) (*sip.Uri, error) {
	if tr != u.Transport {
		if u.Transport == livekit.SIPTransport_SIP_TRANSPORT_AUTO {
			//tr = tr
		} else if tr == livekit.SIPTransport_SIP_TRANSPORT_AUTO {
			tr = u.Transport
		} else {
			return nil, fmt.Errorf("different transports specified: %v vs %v", tr, u.Transport)
		}
	}
	p := &sip.Uri{Scheme: "sip"}
	setUriTransport(p, tr)
	if u.User == "" {
		return nil, fmt.Errorf("username or number must be set")
	}
	if u.Host == "" && u.Ip == "" {
		return nil, fmt.Errorf("host or ip must be set")
	}
	p.User = u.User
	p.Host = u.Host
	if p.Host == "" {
		p.Host = u.Ip
	}
	if _, sport, err := net.SplitHostPort(p.Host); err == nil && sport != "" {
		return nil, fmt.Errorf("host or ip must not contain port")
	}
	p.Port = int(u.Port)
	return p, nil
}

func buildRequestURI(u *livekit.SIPRequestDest, legacyUser, legacyAddr string, tr livekit.SIPTransport) (*sip.Uri, error) {
	if u == nil {
		return buildLegacyURI(legacyUser, legacyAddr, tr)
	}
	switch u := u.Uri.(type) {
	default:
	case *livekit.SIPRequestDest_Raw:
		return buildRawURI(u.Raw, tr)
	case *livekit.SIPRequestDest_Values:
		return buildValuesURI(u.Values, tr)
	}
	return nil, fmt.Errorf("invalid request URI type")
}

func buildFromToURI(u *livekit.SIPNamedDest, legacyUser, legacyAddr string, tr livekit.SIPTransport) (*sip.Uri, error) {
	if u == nil {
		return buildLegacyURI(legacyUser, legacyAddr, tr)
	}
	switch u := u.Uri.(type) {
	default:
	case *livekit.SIPNamedDest_Raw:
		return buildRawURI(u.Raw, tr)
	case *livekit.SIPNamedDest_Values:
		return buildValuesURI(u.Values, tr)
	}
	return nil, fmt.Errorf("invalid URI type")
}

func buildFromHeader(u *livekit.SIPNamedDest, legacyName *string, legacyUser, legacyAddr string, tr livekit.SIPTransport) (*sip.FromHeader, error) {
	su, err := buildFromToURI(u, legacyUser, legacyAddr, tr)
	if err != nil {
		return nil, err
	}
	h := &sip.FromHeader{
		Address: *su,
	}
	if u != nil {
		h.DisplayName = u.DisplayName
	} else if legacyName != nil {
		h.DisplayName = *legacyName
	} else {
		// Nothing specified, preserve legacy behavior
		h.DisplayName = su.User
	}
	return h, nil
}

func buildToHeader(u *livekit.SIPNamedDest, legacyUser, legacyAddr string, tr livekit.SIPTransport) (*sip.ToHeader, error) {
	if u != nil && legacyUser != "" {
		return nil, errors.New("cannot use both CallTo and SipToHeader")
	}
	su, err := buildFromToURI(u, legacyUser, legacyAddr, tr)
	if err != nil {
		return nil, err
	}
	h := &sip.ToHeader{
		Address: *su,
	}
	if u != nil {
		h.DisplayName = u.DisplayName
	}
	return h, nil
}

func buildOutboundHeaders(req *rpc.InternalCreateSIPParticipantRequest, defaultHost string) (*sip.Uri, *sip.FromHeader, *sip.ToHeader, error) {
	uri, err := buildRequestURI(req.SipRequestUri, req.CallTo, req.Address, req.Transport)
	if err != nil {
		return nil, nil, nil, psrpc.NewError(psrpc.InvalidArgument, fmt.Errorf("invalid request URI: %w", err))
	}
	to, err := buildToHeader(req.SipToHeader, req.CallTo, req.Address, req.Transport)
	if err != nil {
		return nil, nil, nil, psrpc.NewError(psrpc.InvalidArgument, fmt.Errorf("invalid To header: %w", err))
	}
	fromHost := req.Hostname
	if fromHost == "" {
		fromHost = defaultHost
	}
	from, err := buildFromHeader(req.SipFromHeader, req.DisplayName, req.Number, fromHost, req.Transport)
	if err != nil {
		return nil, nil, nil, psrpc.NewError(psrpc.InvalidArgument, fmt.Errorf("invalid From header: %w", err))
	}
	return uri, from, to, nil
}

func (c *Client) createSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (resp *rpc.InternalCreateSIPParticipantResponse, retErr error) {
	if c.mon.Health() != stats.HealthOK {
		return nil, siperrors.ErrUnavailable
	}
	req.Upgrade()
	if req.RoomName == "" {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "room name must be set")
	}
	defaultHost := c.ContactURI(TransportFrom(req.Transport)).GetHost()
	log := c.log
	if req.ProjectId != "" {
		log = log.WithValues("projectID", req.ProjectId)
	}
	if req.SipTrunkId != "" {
		log = log.WithValues("sipTrunk", req.SipTrunkId)
	}
	mconf, err := newMediaConfig(req.Media, c.conf.MediaTimeout)
	if err != nil {
		return nil, err
	}
	uri, from, to, err := buildOutboundHeaders(req, defaultHost)
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
		"fromHost", from.Address.Host,
		"fromUser", from.Address.User,
		"toHost", to.Address.Host,
		"toUser", to.Address.User,
		"reqHost", uri.Host,
		"reqUser", uri.User,
		"direction", "outbound",
	)

	req.ParticipantAttributes = maps.Clone(req.ParticipantAttributes) // shallow clone - string/string map. Needed to avoid mutating psrpc req
	initial := c.createSIPCallInfo(uri, from, to, req)
	state := NewCallState(c.getStateHandler(req.ProjectId, req.Observability, initial), initial)

	defer func() {
		state.Update(func(info *livekit.SIPCallInfo) {
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
		transport:       req.Transport,
		uri:             uri,
		from:            from,
		to:              to,
		user:            req.Username,
		pass:            req.Password,
		dtmf:            req.Dtmf,
		dialtone:        req.PlayDialtone,
		headers:         maps.Clone(req.Headers), // shallow clone - string/string map. Needed to avoid mutating psrpc req
		includeHeaders:  req.IncludeHeaders,
		headersToAttrs:  req.HeadersToAttributes,
		attrsToHeaders:  req.AttributesToHeaders,
		ringingTimeout:  req.RingingTimeout.AsDuration(),
		maxCallDuration: req.MaxCallDuration.AsDuration(),
		enabledFeatures: req.EnabledFeatures,
		featureFlags:    req.FeatureFlags,
		mediaConfig:     mconf,
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

func (c *Client) createSIPCallInfo(uri *sip.Uri, from *sip.FromHeader, to *sip.ToHeader, req *rpc.InternalCreateSIPParticipantRequest) *livekit.SIPCallInfo {
	toUri := ConvertURI(&to.Address)
	fromUri := ConvertURI(&from.Address)
	fromUri.Addr = netip.AddrPortFrom(c.sconf.SignalingIP, uint16(c.conf.SIPPort))

	callInfo := &livekit.SIPCallInfo{
		CallId:                req.SipCallId,
		Region:                c.region,
		TrunkId:               req.SipTrunkId,
		RoomName:              req.RoomName,
		ParticipantIdentity:   req.ParticipantIdentity,
		ParticipantAttributes: req.ParticipantAttributes,
		CallDirection:         livekit.SIPCallDirection_SCD_OUTBOUND,
		ToUri:                 toUri.ToSIPUri(),
		FromUri:               fromUri.ToSIPUri(),
		CreatedAtNs:           time.Now().UnixNano(),
		MediaEncryption:       req.MediaEncryption.String(),
		EnabledFeatures:       req.EnabledFeatures,
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
	tag, _ := GetLocalTagUAS(req)
	c.cmu.Lock()
	call := c.activeCalls[tag]
	c.cmu.Unlock()
	if call == nil {
		return false
	}
	call.log.Infow("BYE from remote")
	go func(call *outboundCall) {
		call.cc.AcceptBye(req, tx)
		call.CloseWith(ctx, EndCall{
			Status: CallHangup,
			Term:   stats.Success("bye"),
			Reason: livekit.DisconnectReason_CLIENT_INITIATED,
		})
	}(call)
	return true
}

func (c *Client) onNotify(req *sip.Request, tx sip.ServerTransaction) bool {
	tag, _ := GetLocalTagUAS(req)
	c.cmu.Lock()
	call := c.activeCalls[tag]
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
