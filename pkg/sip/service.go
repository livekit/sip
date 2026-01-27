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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"

	"github.com/livekit/sip/pkg/config"
	siperrors "github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/version"
)

type ServiceConfig struct {
	SignalingIP      netip.Addr
	SignalingIPLocal netip.Addr
	MediaIP          netip.Addr
}

type Service struct {
	conf    *config.Config
	sconf   *ServiceConfig
	log     logger.Logger
	mon     *stats.Monitor
	cli     *Client
	srv     *Server
	closers []io.Closer

	mu               sync.Mutex
	pendingTransfers map[transferKey]chan struct{}
}

type transferKey struct {
	SipCallId  string
	TransferTo string
}

type GetIOInfoClient func(projectID string) rpc.IOInfoClient

func NewService(region string, conf *config.Config, mon *stats.Monitor, log logger.Logger, getIOClient GetIOInfoClient) (*Service, error) {
	if log == nil {
		log = logger.GetLogger()
	}
	if conf.MediaTimeout <= 0 {
		conf.MediaTimeout = defaultMediaTimeout
	}
	if conf.MediaTimeoutInitial <= 0 {
		conf.MediaTimeoutInitial = defaultMediaTimeoutInitial
	}
	s := &Service{
		conf:             conf,
		log:              log,
		mon:              mon,
		cli:              NewClient(region, conf, log, mon, getIOClient),
		srv:              NewServer(region, conf, log, mon, getIOClient),
		pendingTransfers: make(map[transferKey]chan struct{}),
	}
	var err error
	s.sconf, err = GetServiceConfig(s.conf)
	if err != nil {
		return nil, err
	}

	const placeholder = "${IP}"
	if strings.Contains(s.conf.SIPHostname, placeholder) {
		s.conf.SIPHostname = strings.ReplaceAll(
			s.conf.SIPHostname,
			placeholder,
			strings.NewReplacer(
				".", "-", // IPv4
				"[", "", "]", "", ":", "-", // IPv6
			).Replace(s.sconf.SignalingIP.String()),
		)
		addr, err := net.ResolveIPAddr("tcp4", s.conf.SIPHostname)
		if err != nil {
			log.Errorw("cannot resolve node hostname", err, "hostname", s.conf.SIPHostname)
		} else {
			log.Infow("resolved node hostname", "hostname", s.conf.SIPHostname, "ip", addr.IP.String())
		}
	}
	if strings.ContainsAny(s.conf.SIPHostname, "$%{}[]:/| ") {
		return nil, fmt.Errorf("invalid hostname: %q", s.conf.SIPHostname)
	}
	if s.conf.SIPHostname != "" {
		log.Infow("using hostname", "hostname", s.conf.SIPHostname)
	}
	if s.conf.SIPRingingInterval < 1*time.Second || s.conf.SIPRingingInterval > 60*time.Second {
		s.conf.SIPRingingInterval = 1 * time.Second
		log.Infow("ringing interval", "seconds", s.conf.SIPRingingInterval)
	}
	return s, nil
}

type ActiveCalls struct {
	Inbound   int
	Outbound  int
	SampleIDs []string
}

func (st ActiveCalls) Total() int {
	return st.Outbound + st.Inbound
}

func sampleMap[K comparable, V any](limit int, m map[K]V, sample func(v V) string) ([]string, int) {
	total := len(m)
	var out []string
	for _, v := range m {
		if s := sample(v); s != "" {
			out = append(out, s)
		}
		limit--
		if limit <= 0 {
			break
		}
	}
	return out, total
}

func (s *Service) ActiveCalls() ActiveCalls {
	st := ActiveCalls{}

	s.cli.cmu.Lock()
	samples, total := sampleMap(5, s.cli.activeCalls, func(v *outboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	st.Outbound = total
	st.SampleIDs = append(st.SampleIDs, samples...)
	s.cli.cmu.Unlock()

	s.srv.cmu.Lock()
	samples, total = sampleMap(5, s.srv.byRemoteTag, func(v *inboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	st.Inbound = total
	st.SampleIDs = append(st.SampleIDs, samples...)
	s.srv.cmu.Unlock()

	return st
}

func (s *Service) Stop() {
	s.cli.Stop()
	s.srv.Stop()
	s.mon.Stop()
	for _, c := range s.closers {
		_ = c.Close()
	}
}

func (s *Service) SetHandler(handler Handler) {
	s.srv.SetHandler(handler)
	s.cli.SetHandler(handler)
}

func (s *Service) Start() error {
	s.log.Debugw("starting sip service", "version", version.Version)
	for name, enabled := range s.conf.Codecs {
		if enabled {
			s.log.Warnw("codec enabled", nil, "name", name)
		} else {
			s.log.Warnw("codec disabled", nil, "name", name)
		}
	}
	msdk.CodecsSetEnabled(s.conf.Codecs)

	if err := s.mon.Start(s.conf); err != nil {
		return err
	}
	// The UA must be shared between the client and the server.
	// Otherwise, the client will have to listen on a random port, which must then be forwarded.
	//
	// Routers are smart, they usually keep the UDP "session" open for a few moments, and may allow INVITE handshake
	// to pass even without forwarding rules on the firewall. ut it will inevitably fail later on follow-up requests like BYE.
	var opts = []sipgo.UserAgentOption{
		sipgo.WithUserAgent(UserAgent),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(s.log))),
	}
	if tconf := s.conf.TCP; tconf != nil {
		s.log.Debugw("configuring TCP dial port range", "start", tconf.DialPort.Start, "end", tconf.DialPort.End)
		opts = append(opts, sipgo.WithUserAgentTCPConfig(&sipgo.TCPConfig{
			DialPorts: sipgo.PortRange{
				Min: tconf.DialPort.Start,
				Max: tconf.DialPort.End,
			},
		}))
	} else {
		s.log.Debugw("TCP config is nil")
	}
	var tlsConf *tls.Config
	if tconf := s.conf.TLS; tconf != nil {
		if len(tconf.Certs) == 0 {
			return errors.New("TLS certificate required")
		}
		var certs []tls.Certificate
		for _, c := range tconf.Certs {
			cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
			if err != nil {
				return err
			}
			certs = append(certs, cert)
		}
		var keyLog io.Writer
		if tconf.KeyLog != "" {
			f, err := os.OpenFile(tconf.KeyLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			s.closers = append(s.closers, f)
			keyLog = f
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					f.Sync()
				}
			}()
		}
		tlsConf = &tls.Config{
			NextProtos:   []string{"sip"},
			Certificates: certs,
			KeyLogWriter: keyLog,
		}

		if len(tconf.CipherSuites) > 0 {
			suits, err := parseCipherSuites(s.log, tconf.CipherSuites)
			if err != nil {
				return err
			}
			tlsConf.CipherSuites = suits
		}
		if tconf.MinVersion != "" {
			minVer, err := parseTLSVersion(tconf.MinVersion)
			if err != nil {
				return err
			}
			tlsConf.MinVersion = minVer
		}
		if tconf.MaxVersion != "" {
			maxVer, err := parseTLSVersion(tconf.MaxVersion)
			if err != nil {
				return err
			}
			tlsConf.MaxVersion = maxVer
		}

		ConfigureTLS(tlsConf)
		opts = append(opts, sipgo.WithUserAgenTLSConfig(tlsConf))
	}
	ua, err := sipgo.NewUA(opts...)
	if err != nil {
		return err
	}
	if err := s.cli.Start(ua, s.sconf); err != nil {
		return err
	}
	// Server is responsible for answering all transactions. However, the client may also receive some (e.g. BYE).
	// Thus, all unhandled transactions will be checked by the client.
	if err := s.srv.Start(ua, s.sconf, tlsConf, s.cli.OnRequest); err != nil {
		return err
	}
	s.log.Debugw("sip service ready")
	return nil
}

func (s *Service) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	resp, err := s.cli.CreateSIPParticipant(ctx, req)
	return resp, siperrors.ApplySIPStatus(err)
}

func (s *Service) CreateSIPParticipantAffinity(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) float32 {
	// TODO: scale affinity based on a number or active calls?
	return 0.5
}

func (s *Service) TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*emptypb.Empty, error) {
	resp, err := s.transferSIPParticipant(ctx, req)
	return resp, siperrors.ApplySIPStatus(err)
}

func (s *Service) transferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*emptypb.Empty, error) {
	s.log.Infow("transferring SIP call", "callID", req.SipCallId, "transferTo", req.TransferTo)

	// Check if provider is internal and config is set before allowing transfer
	if err := s.checkInternalProviderRequest(ctx, req.SipCallId); err != nil {
		return &emptypb.Empty{}, err
	}

	var transferResult atomic.Pointer[error]

	s.mu.Lock()
	k := transferKey{
		SipCallId:  req.SipCallId,
		TransferTo: req.TransferTo,
	}
	done, ok := s.pendingTransfers[k]
	if !ok {
		done = make(chan struct{})
		s.pendingTransfers[k] = done

		timeout := req.RingingTimeout.AsDuration()
		if timeout <= 0 {
			timeout = 80 * time.Second
		}

		go func() {
			ctx, cdone := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cdone()

			err := s.processParticipantTransfer(ctx, req.SipCallId, req.TransferTo, req.Headers, req.PlayDialtone)
			transferResult.Store(&err)
			close(done)

			s.mu.Lock()
			delete(s.pendingTransfers, k)
			s.mu.Unlock()
		}()
	} else {
		s.log.Debugw("repeated request for call transfer", "callID", req.SipCallId, "transferTo", req.TransferTo)
	}
	s.mu.Unlock()

	select {
	case <-done:
		var err error
		errPtr := transferResult.Load()
		if errPtr != nil {
			err = *errPtr
		}
		return &emptypb.Empty{}, err
	case <-ctx.Done():
		return &emptypb.Empty{}, psrpc.NewError(psrpc.Canceled, ctx.Err())
	}
}

func (s *Service) processParticipantTransfer(ctx context.Context, callID string, transferTo string, headers map[string]string, dialtone bool) error {
	// Look for call both in client (outbound) and server (inbound)
	s.cli.cmu.Lock()
	out := s.cli.activeCalls[LocalTag(callID)]
	s.cli.cmu.Unlock()

	if out != nil {
		s.mon.TransferStarted(stats.Outbound)
		err := out.transferCall(ctx, transferTo, headers, dialtone)
		if err != nil {
			s.mon.TransferFailed(stats.Outbound, extractTransferErrorReason(err), true)
			return err
		}
		s.mon.TransferSucceeded(stats.Outbound)
		return nil
	}

	s.srv.cmu.Lock()
	in := s.srv.byLocalTag[LocalTag(callID)]
	s.srv.cmu.Unlock()

	if in != nil {
		s.mon.TransferStarted(stats.Inbound)
		err := in.transferCall(ctx, transferTo, headers, dialtone)
		if err != nil {
			s.mon.TransferFailed(stats.Inbound, extractTransferErrorReason(err), true)
			return err
		}
		s.mon.TransferSucceeded(stats.Inbound)
		return nil
	}

	err := psrpc.NewErrorf(psrpc.NotFound, "unknown call")
	s.mon.TransferFailed(stats.Inbound, "unknown_call", false)
	return err
}

func (s *Service) checkInternalProviderRequest(ctx context.Context, callID string) error {
	// Look for call both in client (outbound) and server (inbound)
	s.cli.cmu.Lock()
	out := s.cli.activeCalls[LocalTag(callID)]
	s.cli.cmu.Unlock()

	if out != nil {
		return s.validateCallProvider(out.state)
	}

	s.srv.cmu.Lock()
	in := s.srv.byLocalTag[LocalTag(callID)]
	s.srv.cmu.Unlock()

	if in != nil {
		return s.validateCallProvider(in.state)
	}

	return psrpc.NewErrorf(psrpc.NotFound, "unknown call")
}

func (s *Service) validateCallProvider(state *CallState) error {
	if state == nil || state.callInfo == nil || state.callInfo.ProviderInfo == nil {
		return nil // No provider info to validate
	}

	// Check if provider is internal and prevent transfer is enabled
	if state.callInfo.ProviderInfo.Type == livekit.ProviderType_PROVIDER_TYPE_INTERNAL && state.callInfo.ProviderInfo.PreventTransfer {
		return fmt.Errorf("we don't yet support transfers for this phone number type")
	}

	return nil
}

// extractTransferErrorReason extracts a user-friendly reason string from an error
func extractTransferErrorReason(err error) string {
	if err == nil {
		return "unknown"
	}

	// Check for livekit.SIPStatus errors first
	var sipStatus *livekit.SIPStatus
	if errors.As(err, &sipStatus) {
		// Use ShortName() to get the status code name without "SIP_STATUS_" prefix
		// and convert to lowercase for metric labels
		return strings.ToLower(sipStatus.Code.ShortName())
	}

	// Check for psrpc errors
	var psrpcErr psrpc.Error
	if errors.As(err, &psrpcErr) {
		switch psrpcErr.Code() {
		case psrpc.NotFound:
			return "not_found"
		case psrpc.Canceled:
			return "canceled"
		case psrpc.DeadlineExceeded:
			return "timeout"
		case psrpc.InvalidArgument:
			return "invalid_argument"
		case psrpc.Internal:
			return "internal_error"
		default:
			return "psrpc_error"
		}
	}

	// Check for context errors
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}

	// Return "other" for unknown errors
	return "other"
}
