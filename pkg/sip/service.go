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
	"maps"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/livekit/sipgo/transport"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	lksip "github.com/livekit/protocol/sip"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"

	"github.com/livekit/sip/pkg/config"
	siperrors "github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/version"
)

type PendingTransfer struct {
	CallID     string
	TransferTo string

	// TODO: record the transfer id here as soon as the transfer starts. It is
	// only known inside the worker goroutine today, so a waiter that gives up
	// before the outcome arrives reports a failure with no transfer id, and the
	// caller cannot match it against the transfer in their logs.
	Outcome atomic.Pointer[transferOutcome]
	Done    chan transferOutcome
}

// transferOutcome is what a finished transfer reports back: the id it was
// recorded under, and how it ended.
type transferOutcome struct {
	TransferID string
	Err        error
}

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

	manager *callManager
}

// GetStateHandler returns the per-call StateHandler that CallState forwards
// outgoing changes to. cloud builds typically return a handler that forks the
// stream to both the upstream RPC and local observability; the default
// non-cloud build returns NewRPCStateHandler(client). obs may be nil; initial
// is the SIPCallInfo that NewCallState will own immediately after.
type GetStateHandler func(projectID string, obs *rpc.SIPCallObservability, initial *livekit.SIPCallInfo) StateHandler

func NewService(region string, conf *config.Config, mon *stats.Monitor, log logger.Logger, getStateHandler GetStateHandler, opts ...ServerOption) (*Service, error) {
	if log == nil {
		log = logger.GetLogger()
	}
	if conf.UDPMaxPayload > 0 {
		transport.UDPMTUSize = conf.UDPMaxPayload
	}
	if conf.MediaTimeout <= 0 {
		conf.MediaTimeout = defaultMediaTimeout
	}
	if conf.MediaTimeoutInitial <= 0 {
		conf.MediaTimeoutInitial = defaultMediaTimeoutInitial
	}
	cli := NewClient(conf, log)
	s := &Service{
		conf: conf,
		log:  log,
		mon:  mon,
		cli:  cli,
		srv:  NewServer(conf, log, opts...),
	}
	var err error
	s.sconf, err = GetServiceConfig(s.conf)
	if err != nil {
		return nil, err
	}
	s.manager = newCallManager(s.srv, cli, region, conf, s.sconf, log, mon, getStateHandler)

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

	s.manager.cmu.Lock()
	samples, total := sampleMap(5, s.manager.activeCalls, func(v *outboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	st.Outbound = total
	st.SampleIDs = append(st.SampleIDs, samples...)
	s.manager.cmu.Unlock()

	s.manager.cmu.Lock()
	samples, total = sampleMap(5, s.manager.byLocalTag, func(v *inboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	st.Inbound = total
	st.SampleIDs = append(st.SampleIDs, samples...)
	s.manager.cmu.Unlock()

	return st
}

func (s *Service) Stop() {
	s.manager.stop()
	s.cli.Stop()
	s.srv.Stop()
	s.mon.Stop()
	for _, c := range s.closers {
		_ = c.Close()
	}
}

func (s *Service) SetHandler(handler Handler) {
	s.manager.SetHandler(handler)
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
	DefaultCodecs().SetEnabledMap(s.conf.Codecs)

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
			NextProtos:   tlsALPNProtocols(tconf.ALPNProtocols),
			Certificates: certs,
			KeyLogWriter: keyLog,
		}

		if err := tconf.ApplyPolicy(s.log, tlsConf); err != nil {
			return err
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
	if err := s.srv.Start(ua, s.sconf, tlsConf, s.manager); err != nil {
		return err
	}
	s.log.Debugw("sip service ready")
	return nil
}

func (s *Service) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	resp, err := s.manager.CreateSIPParticipant(ctx, req)
	return resp, siperrors.ApplySIPStatus(err)
}

func (s *Service) TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*rpc.InternalTransferSIPParticipantResponse, error) {
	return s.manager.TransferSIPParticipant(ctx, req)
}

func (s *Service) CreateSIPParticipantAffinity(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) float32 {
	if len(s.conf.SIPTrunkIds) > 0 && !slices.Contains(s.conf.SIPTrunkIds, req.GetSipTrunkId()) {
		return 0
	}
	active := float32(s.ActiveCalls().Total())
	if max := float32(s.conf.MaxActiveCalls); max > 0 {
		if active >= max {
			return 0
		}
		return 1 - active/max
	}
	return 1 / (1 + active)
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*rpc.InternalTransferSIPParticipantResponse, error) {
	out := cm.transferSIPParticipant(ctx, req)
	if out.Err != nil {
		return nil, transferError(out)
	}
	return &rpc.InternalTransferSIPParticipantResponse{
		TransferId: out.TransferID,
		Status:     livekit.SIPTransferStatus_STS_TRANSFER_SUCCESSFUL,
		Reason:     livekit.SIPTransferReason_STR_COMPLETED,
	}, nil
}

// transferError reports the outcome of a failed transfer on the error.
func transferError(out transferOutcome) error {
	if out.Err == nil {
		return nil
	}
	reason, sipStatus := transferReason(out.Err)
	// Borrow the code only. ApplySIPStatus derives it from the SIP status when
	// the failure carries one, and is a pass-through otherwise, leaving the code
	// the error already had.
	code, ok := psrpc.GetErrorCode(siperrors.ApplySIPStatus(out.Err))
	if !ok {
		code = psrpc.Unknown
	}
	details := []proto.Message{&livekit.SIPTransferError{
		TransferId: out.TransferID,
		Reason:     reason,
		SipStatus:  sipStatus,
	}}
	if sipStatus != nil {
		// Deliberately the same status twice. A client whose protocol predates
		// SIPTransferError cannot resolve that detail, and the Go SDK swaps the
		// twirp error for a status error before the caller sees it, so its
		// metadata is out of reach too. This copy keeps SIPStatusFrom working
		// for those clients. Remove once they have all moved on.
		details = append(details, sipStatus)
	}
	return psrpc.NewError(code, out.Err, details...)
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) transferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) transferOutcome {
	cm.log.Infow("transferring SIP call", "callID", req.SipCallId, "transferTo", req.TransferTo)

	// Check if provider is internal and config is set before allowing transfer
	if err := cm.checkInternalProviderRequest(ctx, req.SipCallId, req.TransferTo); err != nil {
		return transferOutcome{Err: err}
	}

	pending, isNew := cm.getOrCreatePendingTransfer(req.SipCallId, req.TransferTo)
	if !isNew {
		if pending.TransferTo != req.TransferTo {
			err := psrpc.NewErrorf(psrpc.InvalidArgument, "call already being transferred elsewhere")
			return transferOutcome{Err: err}
		}
		// Already transferred, resume wait
		cm.log.Debugw("repeated request for call transfer", "callID", req.SipCallId, "transferTo", req.TransferTo)
		// TODO: Maybe just bump the psrpc timeout? It gets auto retried anyway internally.
	} else {
		// Initial transfer request for this call
		timeout := req.RingingTimeout.AsDuration()
		if timeout <= 0 {
			// RingingTimeout is either specified by caller, or defaults to 30 seconds.
			// This code should be pretty much unreachable.
			timeout = 120 * time.Second
		}

		go func() {
			ctx, cdone := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cdone()

			headers := maps.Clone(req.Headers) // shallow clone - string/string map. Needed to avoid mutating psrpc req
			out := cm.processParticipantTransfer(ctx, req.SipCallId, req.TransferTo, headers, req.PlayDialtone)
			select {
			case pending.Done <- out:
			default:
				cm.log.Errorw("pending transfer received more than one error", out.Err, "callID", req.SipCallId, "transferTo", req.TransferTo)
			}
			pending.Outcome.Store(&out)
			close(pending.Done)

			cm.mu.Lock()
			delete(cm.pendingTransfers, LocalTag(req.SipCallId))
			cm.mu.Unlock()
		}()
	}

	select {
	case out := <-pending.Done:
		if out.Err == nil {
			// If there is more than one RPC call waiting on the result,
			// this ensures we return the same outcome to all callers.
			if pOut := pending.Outcome.Load(); pOut != nil {
				out = *pOut
			}
		}
		return out
	case <-ctx.Done():
		return transferOutcome{Err: psrpc.NewError(psrpc.Canceled, ctx.Err())}
	}
}

// transferReason classifies how a transfer ended. A rejected transfer also
// reports the SIP status the transferee, or its provider, answered with.
func transferReason(err error) (livekit.SIPTransferReason, *livekit.SIPStatus) {
	if err == nil {
		return livekit.SIPTransferReason_STR_COMPLETED, nil
	}
	var sipStatus *livekit.SIPStatus
	switch {
	case errors.Is(err, errTransferCallEnded):
		return livekit.SIPTransferReason_STR_CALL_ENDED, nil
	case errors.Is(err, errReferSubscriptionTerminated):
		return livekit.SIPTransferReason_STR_SUBSCRIPTION_TERMINATED, nil
	case errors.As(err, &sipStatus):
		return livekit.SIPTransferReason_STR_REJECTED, sipStatus
	case errors.Is(err, context.DeadlineExceeded):
		return livekit.SIPTransferReason_STR_RINGING_TIMEOUT, nil
	}
	return livekit.SIPTransferReason_STR_UNSPECIFIED, nil
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) getOrCreatePendingTransfer(callID string, transferTo string) (*PendingTransfer, bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	keyCall := LocalTag(callID)
	pending, ok := cm.pendingTransfers[keyCall]
	if ok {
		return pending, false
	}

	pending = &PendingTransfer{
		CallID:     callID,
		TransferTo: transferTo,
		Done:       make(chan transferOutcome, 1),
	}
	cm.pendingTransfers[keyCall] = pending
	return pending, true
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) processParticipantTransfer(ctx context.Context, callID string, transferTo string, headers map[string]string, dialtone bool) transferOutcome {
	// Look for call both in client (outbound) and server (inbound)
	cm.cmu.Lock()
	out := cm.activeCalls[LocalTag(callID)]
	cm.cmu.Unlock()

	if out != nil {
		cm.mon.TransferStarted(stats.Outbound)
		transferID, err := out.transferCall(ctx, transferTo, headers, dialtone)
		if err != nil {
			cm.mon.TransferFailed(stats.Outbound, extractTransferErrorReason(err), true)
			return transferOutcome{TransferID: transferID, Err: err}
		}
		cm.mon.TransferSucceeded(stats.Outbound)
		return transferOutcome{TransferID: transferID}
	}

	cm.cmu.Lock()
	in := cm.byLocalTag[LocalTag(callID)]
	cm.cmu.Unlock()

	if in != nil {
		cm.mon.TransferStarted(stats.Inbound)
		transferID, err := in.transferCall(ctx, transferTo, headers, dialtone)
		if err != nil {
			cm.mon.TransferFailed(stats.Inbound, extractTransferErrorReason(err), true)
			return transferOutcome{TransferID: transferID, Err: err}
		}
		cm.mon.TransferSucceeded(stats.Inbound)
		return transferOutcome{TransferID: transferID}
	}

	err := psrpc.NewErrorf(psrpc.NotFound, "unknown call")
	cm.mon.TransferFailed(stats.Inbound, "unknown_call", false)
	return transferOutcome{Err: err}
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) checkInternalProviderRequest(ctx context.Context, callID, transferTo string) error {
	// Look for call both in client (outbound) and server (inbound)
	cm.cmu.Lock()
	out := cm.activeCalls[LocalTag(callID)]
	cm.cmu.Unlock()

	if out != nil {
		return cm.rejectTransfer(stats.Outbound, cm.validateCallProvider(out.state, transferTo))
	}

	cm.cmu.Lock()
	in := cm.byLocalTag[LocalTag(callID)]
	cm.cmu.Unlock()

	if in != nil {
		return cm.rejectTransfer(stats.Inbound, cm.validateCallProvider(in.state, transferTo))
	}

	return psrpc.NewErrorf(psrpc.NotFound, "unknown call")
}

// rejectTransfer counts a transfer refused before it started as an attempt
// that failed, so failures never outnumber attempts.
// TODO(alexfish): Move this into manager.go
func (cm *callManager) rejectTransfer(dir stats.CallDir, err error) error {
	if err != nil {
		cm.mon.TransferStarted(dir)
		cm.mon.TransferFailed(dir, extractTransferErrorReason(err), true)
	}
	return err
}

// TODO(alexfish): Move this into manager.go
func (cm *callManager) validateCallProvider(state *CallState, transferTo string) error {
	info := state.Info()
	if state == nil || info == nil || info.ProviderInfo == nil {
		return nil // No provider info to validate
	}

	// Validation below is only for internal providers
	if info.ProviderInfo.Type != livekit.ProviderType_PROVIDER_TYPE_INTERNAL {
		return nil
	}
	if info.ProviderInfo.PreventTransfer {
		return psrpc.NewErrorf(psrpc.PermissionDenied, "transfers are not supported for this phone number type")
	}
	if lksip.IsEmergencyNumber(lksip.URIUser(transferTo)) {
		return psrpc.NewErrorf(psrpc.InvalidArgument, "transfers to an emergency number are not supported from LiveKit phone numbers")
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

	// Transfer outcomes the bridge decides itself. They carry no SIP status, so
	// the switch below would report them all as psrpc_error.
	if errors.Is(err, errTransferCallEnded) {
		return "call_ended"
	}
	if errors.Is(err, errReferSubscriptionTerminated) {
		return "refer_terminated"
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
		case psrpc.PermissionDenied:
			return "permission_denied"
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

func (s *Service) GetServiceConfig() ServiceConfig {
	if s.sconf != nil {
		return *s.sconf
	}
	return ServiceConfig{}
}
