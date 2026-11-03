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
	"slices"
	"sync/atomic"
	"time"

	"github.com/icholy/digest"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/traceid"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
	"github.com/livekit/sipgo/transaction"
	"github.com/livekit/sipgo/transport"

	"github.com/livekit/sip/pkg/config"
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
	TrunkID         string
	Call            *rpc.SIPCall
	Pin             string
	NoPin           bool
	ExtraAttributes map[string]string
}

type AuthResult int

const (
	AuthNotFound = AuthResult(iota)
	AuthDrop
	AuthPassword
	AuthAccept
	AuthQuotaExceeded
	AuthNoTrunkFound
	AuthRouteNotAllowed
	// AuthRejectedAsError is a rejection the auth service reported as an error
	// instead of a response.
	AuthRejectedAsError
	// AuthFailureOther is a catch-all value.
	AuthFailureOther
)

type AuthInfo struct {
	Result        AuthResult
	ProjectID     string
	TrunkID       string
	Auth          InboundAuth
	ProviderInfo  *livekit.ProviderInfo
	Observability *rpc.SIPCallObservability
}

type InboundAuth struct {
	Username string
	Password string
	Realm    string
}

type DispatchResult int

const (
	DispatchAccept = DispatchResult(iota)
	DispatchRequestPin
	DispatchNoRuleReject       // reject the call with an error
	DispatchNoRuleDrop         // silently drop the call
	DispatchServiceUnavailable // dispatch rule evaluation failed at the transport level
)

type CallDispatch struct {
	Result               DispatchResult
	Room                 RoomConfig
	ProjectID            string
	TrunkID              string
	DispatchRuleID       string
	Headers              map[string]string
	HeadersToAttributes  map[string]string
	IncludeHeaders       livekit.SIPHeaderOptions
	AttributesToHeaders  map[string]string
	EnabledFeatures      []livekit.SIPFeature
	FeatureFlags         map[string]string
	RingingTimeout       time.Duration
	RingingTimeoutStatus livekit.SIPStatusCode
	MaxCallDuration      time.Duration
	MediaConfig          *livekit.SIPMediaConfig
}

type CallIdentifier struct {
	TraceID   traceid.ID
	ProjectID string
	CallID    string
	SipCallID string
}

type MediaProcessorOpts struct {
	InputSampleRate int
}

type Handler interface {
	GetAuthCredentials(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error)
	DispatchCall(ctx context.Context, info *CallInfo) CallDispatch
	GetMediaProcessor(features []livekit.SIPFeature, featureFlags map[string]string, callID string, opts MediaProcessorOpts) msdk.PCM16Processor

	RegisterTransferSIPParticipantTopic(sipCallId string) error
	DeregisterTransferSIPParticipantTopic(sipCallId string)

	OnInboundInfo(log logger.Logger, callInfo *rpc.SIPCall, headers Headers)
	OnSessionEnd(ctx context.Context, callIdentifier *CallIdentifier, state *CallState, reason string)
}

// HandlerInterceptor wraps a handler function.
type HandlerInterceptor func(sipgo.RequestHandler) sipgo.RequestHandler

type Server struct {
	log          logger.Logger
	sipSrv       *sipgo.Server
	sipListeners []io.Closer

	conf         *config.Config
	sconf        *ServiceConfig
	interceptors []HandlerInterceptor
}

type inProgressInvite struct {
	sipCallID    string
	challenge    digest.Challenge
	authResolved atomic.Bool
}

type rejectedInviteResponse struct {
	status sip.StatusCode
	reason string
}

type ServerOption func(s *Server)

// WithInterceptors configures all sip handlers to be wrapped with the given set
// of interceptors. Interceptors are applied s.t. the first interceptor is the
// outermost one.
func WithInterceptors(interceptors ...HandlerInterceptor) ServerOption {
	return func(s *Server) {
		s.interceptors = interceptors
	}
}

func NewServer(conf *config.Config, log logger.Logger, options ...ServerOption) *Server {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &Server{
		log:  log,
		conf: conf,
	}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Server) ContactURI(tr Transport) URI {
	return getContactURI(s.conf, s.sconf.SignalingIP, tr)
}

func (s *Server) transportLayer() *transport.Layer {
	return s.sipSrv.TransportLayer()
}

func (s *Server) transactionLayer() *transaction.Layer {
	return s.sipSrv.TransactionLayer()
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

func (s *Server) wrapHandler(handler sipgo.RequestHandler) sipgo.RequestHandler {
	ret := handler
	for _, interceptor := range slices.Backward(s.interceptors) {
		ret = interceptor(ret)
	}
	return ret
}

func (s *Server) Start(agent *sipgo.UserAgent, sc *ServiceConfig, tlsConf *tls.Config, manager CallManager) error {
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

	s.sipSrv.OnOptions(s.wrapHandler(manager.HandleOptions))
	s.sipSrv.OnInvite(s.wrapHandler(manager.HandleInvite))
	s.sipSrv.OnAck(s.wrapHandler(manager.HandleAck))
	s.sipSrv.OnBye(s.wrapHandler(manager.HandleBye))
	s.sipSrv.OnNotify(s.wrapHandler(manager.HandleNotify))
	s.sipSrv.OnNoRoute(s.wrapHandler(manager.HandleNoRoute))

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
	if s.sipSrv != nil {
		_ = s.sipSrv.Close()
	}
	for _, l := range s.sipListeners {
		_ = l.Close()
	}
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) RegisterTransferSIPParticipant(sipCallID LocalTag, i *inboundCall) error {
	return cm.handler.RegisterTransferSIPParticipantTopic(string(sipCallID))
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) DeregisterTransferSIPParticipant(sipCallID LocalTag) {
	cm.handler.DeregisterTransferSIPParticipantTopic(string(sipCallID))
}
