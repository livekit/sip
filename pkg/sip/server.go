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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/icholy/digest"
	"golang.org/x/exp/maps"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/traceid"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	UserAgent   = "LiveKit"
	digestLimit = 500
)

const (
	maxCallCache = 5000        // ~8 B per entry, ~40 KB
	callCacheTTL = time.Minute // we only need it for detecting retries from providers for now
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

type CallInfo struct {
	TrunkID string
	Call    *rpc.SIPCall
	Pin     string
	NoPin   bool
}

type AuthResult int

const (
	AuthNotFound = AuthResult(iota)
	AuthDrop
	AuthPassword
	AuthAccept
	AuthQuotaExceeded
	AuthNoTrunkFound
)

type AuthInfo struct {
	Result       AuthResult
	ProjectID    string
	TrunkID      string
	Username     string
	Password     string
	ProviderInfo *livekit.ProviderInfo
}

type DispatchResult int

const (
	DispatchAccept = DispatchResult(iota)
	DispatchRequestPin
	DispatchNoRuleReject // reject the call with an error
	DispatchNoRuleDrop   // silently drop the call
)

type CallDispatch struct {
	Result              DispatchResult
	Room                RoomConfig
	ProjectID           string
	TrunkID             string
	DispatchRuleID      string
	Headers             map[string]string
	HeadersToAttributes map[string]string
	IncludeHeaders      livekit.SIPHeaderOptions
	AttributesToHeaders map[string]string
	EnabledFeatures     []livekit.SIPFeature
	FeatureFlags        map[string]string
	RingingTimeout      time.Duration
	MaxCallDuration     time.Duration
	MediaEncryption     livekit.SIPMediaEncryption
}

type CallIdentifier struct {
	TraceID   traceid.ID
	ProjectID string
	CallID    string
	SipCallID string
}

type Handler interface {
	GetAuthCredentials(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error)
	DispatchCall(ctx context.Context, info *CallInfo) CallDispatch
	GetMediaProcessor(features []livekit.SIPFeature, featureFlags map[string]string) msdk.PCM16Processor

	RegisterTransferSIPParticipantTopic(sipCallId string) error
	DeregisterTransferSIPParticipantTopic(sipCallId string)

	OnInboundInfo(log logger.Logger, callInfo *rpc.SIPCall, headers Headers)
	OnSessionEnd(ctx context.Context, callIdentifier *CallIdentifier, callInfo *livekit.SIPCallInfo, reason string)
}

type Server struct {
	log          logger.Logger
	mon          *stats.Monitor
	region       string
	sipSrv       *sipgo.Server
	getIOClient  GetIOInfoClient
	getRoom      GetRoomFunc
	sipListeners []io.Closer
	sipUnhandled RequestHandler

	imu               sync.Mutex
	inProgressInvites []*inProgressInvite

	closing     core.Fuse
	cmu         sync.RWMutex
	byRemoteTag map[RemoteTag]*inboundCall
	byLocalTag  map[LocalTag]*inboundCall
	byCallID    map[string]*inboundCall

	infos struct {
		sync.Mutex
		byCallID *expirable.LRU[string, *inboundCallInfo]
	}

	handler Handler
	conf    *config.Config
	sconf   *ServiceConfig

	res mediaRes
}

type inProgressInvite struct {
	sipCallID string
	challenge digest.Challenge
}

type ServerOption func(s *Server)

func WithGetRoomServer(fn GetRoomFunc) ServerOption {
	return func(s *Server) {
		if fn != nil {
			s.getRoom = fn
		}
	}
}

func NewServer(region string, conf *config.Config, log logger.Logger, mon *stats.Monitor, getIOClient GetIOInfoClient, options ...ServerOption) *Server {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &Server{
		log:         log,
		conf:        conf,
		region:      region,
		mon:         mon,
		getIOClient: getIOClient,
		getRoom:     DefaultGetRoomFunc,
		byRemoteTag: make(map[RemoteTag]*inboundCall),
		byLocalTag:  make(map[LocalTag]*inboundCall),
		byCallID:    make(map[string]*inboundCall),
	}
	for _, option := range options {
		option(s)
	}
	s.infos.byCallID = expirable.NewLRU[string, *inboundCallInfo](maxCallCache, nil, callCacheTTL)
	s.initMediaRes()
	return s
}

func (s *Server) SetHandler(handler Handler) {
	s.handler = handler
}

func (s *Server) ContactURI(tr Transport) URI {
	return getContactURI(s.conf, s.sconf.SignalingIP, tr)
}

func (s *Server) startUDP(addr netip.AddrPort) error {
	lis, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   addr.Addr().AsSlice(),
		Port: int(addr.Port()),
	})
	if err != nil {
		return fmt.Errorf("cannot listen on the UDP signaling port %d: %w", s.conf.SIPPortListen, err)
	}
	s.sipListeners = append(s.sipListeners, lis)
	s.log.Infow("sip signaling listening on",
		"local", s.sconf.SignalingIPLocal, "external", s.sconf.SignalingIP,
		"port", addr.Port(), "announce-port", s.conf.SIPPort,
		"proto", "udp",
	)

	go func() {
		if err := s.sipSrv.ServeUDP(lis); err != nil {
			panic(fmt.Errorf("SIP listen UDP error: %w", err))
		}
	}()
	return nil
}

func (s *Server) startTCP(addr netip.AddrPort) error {
	lis, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   addr.Addr().AsSlice(),
		Port: int(addr.Port()),
	})
	if err != nil {
		return fmt.Errorf("cannot listen on the TCP signaling port %d: %w", s.conf.SIPPortListen, err)
	}
	s.sipListeners = append(s.sipListeners, lis)
	s.log.Infow("sip signaling listening on",
		"local", s.sconf.SignalingIPLocal, "external", s.sconf.SignalingIP,
		"port", addr.Port(), "announce-port", s.conf.SIPPort,
		"proto", "tcp",
	)

	go func() {
		if err := s.sipSrv.ServeTCP(lis); err != nil && !errors.Is(err, net.ErrClosed) {
			panic(fmt.Errorf("SIP listen TCP error: %w", err))
		}
	}()
	return nil
}

func (s *Server) startTLS(addr netip.AddrPort, conf *tls.Config) error {
	tlis, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   addr.Addr().AsSlice(),
		Port: int(addr.Port()),
	})
	if err != nil {
		return fmt.Errorf("cannot listen on the TLS signaling port %d: %w", s.conf.SIPPortListen, err)
	}
	lis := tls.NewListener(tlis, conf)
	s.sipListeners = append(s.sipListeners, lis)
	s.log.Infow("sip signaling listening on",
		"local", s.sconf.SignalingIPLocal, "external", s.sconf.SignalingIP,
		"port", addr.Port(), "announce-port", s.conf.TLS.Port,
		"proto", "tls",
	)

	go func() {
		if err := s.sipSrv.ServeTLS(lis); err != nil && !errors.Is(err, net.ErrClosed) {
			panic(fmt.Errorf("SIP listen TLS error: %w", err))
		}
	}()
	return nil
}

type RequestHandler func(req *sip.Request, tx sip.ServerTransaction) bool

func (s *Server) Start(agent *sipgo.UserAgent, sc *ServiceConfig, tlsConf *tls.Config, unhandled RequestHandler) error {
	s.sconf = sc
	s.log.Infow("server starting", "local", s.sconf.SignalingIPLocal, "external", s.sconf.SignalingIP)

	if agent == nil {
		ua, err := sipgo.NewUA(
			sipgo.WithUserAgent(UserAgent),
			sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
		)
		if err != nil {
			return err
		}
		agent = ua
	}

	var err error
	s.sipSrv, err = sipgo.NewServer(agent,
		sipgo.WithServerLogger(slog.New(logger.ToSlogHandler(s.log))),
	)
	if err != nil {
		return err
	}

	s.sipSrv.OnOptions(s.onOptions)
	s.sipSrv.OnInvite(s.onInvite)
	s.sipSrv.OnAck(s.onAck)
	s.sipSrv.OnBye(s.onBye)
	s.sipSrv.OnNotify(s.onNotify)
	s.sipSrv.OnNoRoute(s.OnNoRoute)
	s.sipUnhandled = unhandled

	listenIP := s.conf.ListenIP
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	ip, err := netip.ParseAddr(listenIP)
	if err != nil {
		return err
	}
	addr := netip.AddrPortFrom(ip, uint16(s.conf.SIPPortListen))
	if err := s.startUDP(addr); err != nil {
		return err
	}
	if err := s.startTCP(addr); err != nil {
		return err
	}
	if tlsConf != nil && s.conf.TLS != nil {
		tconf := s.conf.TLS
		addrTLS := netip.AddrPortFrom(ip, uint16(tconf.ListenPort))
		if err := s.startTLS(addrTLS, tlsConf); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) Stop() {
	s.closing.Break()
	s.cmu.Lock()
	calls := maps.Values(s.byRemoteTag)
	s.byRemoteTag = make(map[RemoteTag]*inboundCall)
	s.cmu.Unlock()
	for _, c := range calls {
		_ = c.Close()
	}
	if s.sipSrv != nil {
		_ = s.sipSrv.Close()
	}
	for _, l := range s.sipListeners {
		_ = l.Close()
	}
}

func (s *Server) RegisterTransferSIPParticipant(sipCallID LocalTag, i *inboundCall) error {
	return s.handler.RegisterTransferSIPParticipantTopic(string(sipCallID))
}

func (s *Server) DeregisterTransferSIPParticipant(sipCallID LocalTag) {
	s.handler.DeregisterTransferSIPParticipantTopic(string(sipCallID))
}
