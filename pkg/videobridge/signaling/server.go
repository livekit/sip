// Copyright 2024 LiveKit, Inc.
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

package signaling

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"

	"github.com/frostbyte73/core"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/config"
	"github.com/livekit/sip/pkg/videobridge/security"
	"github.com/livekit/sip/pkg/videobridge/stats"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
)

// CallHandler is called when a new inbound SIP video call is received.
// It receives the parsed SDP and negotiated media info, and must return
// the local SDP answer. Returning an error rejects the call.
type CallHandler func(ctx context.Context, call *InboundCall) error

// InboundCall represents an incoming SIP video call.
type InboundCall struct {
	CallID    string
	FromURI   string
	ToURI     string
	FromTag   string
	ToTag     string
	RemoteSDP *ParsedSDP
	Media     *NegotiatedMedia
	// LocalSDP is set by the handler to send back in the 200 OK
	LocalSDP string
}

// SIPServer handles SIP signaling for video calls using a B2BUA pattern.
type SIPServer struct {
	log    logger.Logger
	conf   *config.Config
	ua     *sipgo.UserAgent
	server *sipgo.Server

	handler      CallHandler
	srtpEnforcer *security.SRTPEnforcer

	// Active calls tracked by Call-ID
	mu    sync.RWMutex
	calls map[string]*callState

	closed  core.Fuse
	localIP netip.Addr
}

type callState struct {
	call     *InboundCall
	tx       sip.ServerTransaction
	cancelFn context.CancelFunc
}

// NewSIPServer creates a new SIP signaling server.
func NewSIPServer(log logger.Logger, conf *config.Config) (*SIPServer, error) {
	localIP, err := resolveLocalIP(conf.SIP.ExternalIP)
	if err != nil {
		return nil, fmt.Errorf("resolving local IP: %w", err)
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(conf.SIP.UserAgent),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(log))),
	)
	if err != nil {
		return nil, fmt.Errorf("creating SIP user agent: %w", err)
	}

	server, err := sipgo.NewServer(ua,
		sipgo.WithServerLogger(slog.New(logger.ToSlogHandler(log))),
	)
	if err != nil {
		ua.Close()
		return nil, fmt.Errorf("creating SIP server: %w", err)
	}

	s := &SIPServer{
		log:          log,
		conf:         conf,
		ua:           ua,
		server:       server,
		calls:        make(map[string]*callState),
		localIP:      localIP,
		srtpEnforcer: security.NewSRTPEnforcer(conf.SRTP),
	}

	server.OnInvite(s.onInvite)
	server.OnBye(s.onBye)
	server.OnAck(s.onAck)
	server.OnCancel(s.onCancel)

	return s, nil
}

// SetCallHandler sets the handler function for incoming calls.
func (s *SIPServer) SetCallHandler(h CallHandler) {
	s.handler = h
}

// Start begins listening for SIP traffic.
func (s *SIPServer) Start() error {
	listenAddr := fmt.Sprintf("0.0.0.0:%d", s.conf.SIP.Port)
	s.log.Infow("SIP server starting", "addr", listenAddr, "transports", s.conf.SIP.Transport)

	for _, transport := range s.conf.SIP.Transport {
		switch transport {
		case "udp":
			go func() {
				if err := s.server.ListenAndServe(context.Background(), "udp", listenAddr); err != nil {
					s.log.Errorw("SIP UDP server error", err)
				}
			}()
		case "tcp":
			go func() {
				if err := s.server.ListenAndServe(context.Background(), "tcp", listenAddr); err != nil {
					s.log.Errorw("SIP TCP server error", err)
				}
			}()
		default:
			s.log.Warnw("unsupported SIP transport", nil, "transport", transport)
		}
	}

	return nil
}

// Close shuts down the SIP server and terminates all active calls.
func (s *SIPServer) Close() error {
	s.closed.Break()

	s.mu.Lock()
	for callID, cs := range s.calls {
		cs.cancelFn()
		delete(s.calls, callID)
	}
	s.mu.Unlock()

	s.server.Close()
	s.ua.Close()

	s.log.Infow("SIP server closed")
	return nil
}

// LocalIP returns the server's external IP address.
func (s *SIPServer) LocalIP() netip.Addr {
	return s.localIP
}

// ActiveCalls returns the number of active calls.
func (s *SIPServer) ActiveCalls() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.calls)
}

func (s *SIPServer) onInvite(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID()
	if callID == nil {
		s.log.Warnw("INVITE without Call-ID", nil)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Missing Call-ID", nil))
		return
	}

	from := req.From()
	to := req.To()
	callIDStr := callID.Value()

	log := s.log.WithValues("callID", callIDStr)
	log.Infow("received SIP INVITE",
		"from", from.Address.String(),
		"to", to.Address.String(),
	)

	// Send 100 Trying
	_ = tx.Respond(sip.NewResponseFromRequest(req, 100, "Trying", nil))

	// Parse SDP from INVITE body
	sdpBody := string(req.Body())
	if sdpBody == "" {
		log.Warnw("INVITE without SDP body", nil)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Missing SDP", nil))
		return
	}

	// SRTP enforcement: validate SDP transport profile before parsing
	if s.srtpEnforcer != nil {
		if err := s.srtpEnforcer.ValidateSDP(sdpBody); err != nil {
			log.Warnw("SRTP enforcement rejected SDP", err)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "SRTP Required", nil))
			return
		}
	}

	parsedSDP, err := ParseSDP(sdpBody)
	if err != nil {
		log.Warnw("failed to parse SDP", err, "sdp", sdpBody)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "SDP Parse Error", nil))
		return
	}

	// Check for video media
	if parsedSDP.VideoMedia() == nil {
		log.Infow("INVITE has no video media, rejecting")
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Video Required", nil))
		return
	}

	// Negotiate media
	negotiated, err := parsedSDP.Negotiate()
	if err != nil {
		log.Warnw("SDP negotiation failed", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Negotiation Failed", nil))
		return
	}

	log.Infow("SDP negotiated",
		"videoCodec", negotiated.VideoCodec,
		"videoPT", negotiated.VideoPayloadType,
		"audioCodec", negotiated.AudioCodec,
		"audioPT", negotiated.AudioPayloadType,
		"remoteAddr", negotiated.RemoteAddr.String(),
	)

	// Send 180 Ringing
	_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))

	stats.SessionsTotal.Inc()
	stats.SessionsActive.Inc()

	// Create call context
	ctx, cancel := context.WithCancel(context.Background())
	inbound := &InboundCall{
		CallID:    callIDStr,
		FromURI:   from.Address.String(),
		ToURI:     to.Address.String(),
		RemoteSDP: parsedSDP,
		Media:     negotiated,
	}
	if from.Params != nil {
		inbound.FromTag, _ = from.Params.Get("tag")
	}

	cs := &callState{
		call:     inbound,
		tx:       tx,
		cancelFn: cancel,
	}

	s.mu.Lock()
	s.calls[callIDStr] = cs
	s.mu.Unlock()

	// Invoke call handler asynchronously
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorw("panic in call handler", fmt.Errorf("%v", r))
				_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Error", nil))
				s.removeCall(callIDStr)
			}
		}()

		if s.handler == nil {
			log.Warnw("no call handler configured", nil)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
			s.removeCall(callIDStr)
			return
		}

		err := s.handler(ctx, inbound)
		if err != nil {
			log.Warnw("call handler rejected call", err)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
			s.removeCall(callIDStr)
			return
		}

		// Send 200 OK with local SDP
		resp := sip.NewResponseFromRequest(req, 200, "OK", []byte(inbound.LocalSDP))
		resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		if err := tx.Respond(resp); err != nil {
			log.Errorw("failed to send 200 OK", err)
			s.removeCall(callIDStr)
			return
		}

		log.Infow("call answered with 200 OK")
	}()
}

func (s *SIPServer) onAck(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID()
	if callID == nil {
		return
	}
	s.log.Debugw("received ACK", "callID", callID.Value())
}

func (s *SIPServer) onBye(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID()
	if callID == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Missing Call-ID", nil))
		return
	}

	callIDStr := callID.Value()
	log := s.log.WithValues("callID", callIDStr)
	log.Infow("received BYE")

	s.mu.RLock()
	cs, ok := s.calls[callIDStr]
	s.mu.RUnlock()

	if !ok {
		log.Warnw("BYE for unknown call", nil)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call Does Not Exist", nil))
		return
	}

	// Cancel the call context to trigger cleanup
	cs.cancelFn()
	s.removeCall(callIDStr)

	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	log.Infow("call terminated via BYE")
}

func (s *SIPServer) onCancel(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID()
	if callID == nil {
		return
	}

	callIDStr := callID.Value()
	log := s.log.WithValues("callID", callIDStr)
	log.Infow("received CANCEL")

	s.mu.RLock()
	cs, ok := s.calls[callIDStr]
	s.mu.RUnlock()

	if ok {
		cs.cancelFn()
		s.removeCall(callIDStr)
	}

	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
}

func (s *SIPServer) removeCall(callID string) {
	s.mu.Lock()
	if _, ok := s.calls[callID]; ok {
		delete(s.calls, callID)
		stats.SessionsActive.Dec()
	}
	s.mu.Unlock()
}

func resolveLocalIP(configuredIP string) (netip.Addr, error) {
	if configuredIP != "" {
		addr, err := netip.ParseAddr(configuredIP)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("invalid external IP %q: %w", configuredIP, err)
		}
		return addr, nil
	}

	// Auto-detect by dialing a public address (no actual traffic sent)
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to resolve local IP: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	addr, ok := netip.AddrFromSlice(localAddr.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("failed to convert local IP")
	}
	return addr, nil
}
