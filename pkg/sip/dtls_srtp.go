// Copyright 2026 LiveKit, Inc.
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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	pdtls "github.com/pion/dtls/v3"
	pice "github.com/pion/ice/v4"
	prtp "github.com/pion/rtp"
	psrtp "github.com/pion/srtp/v3"
)

// dtlsSrtpSession is deliberately a media transport: callers see the same
// RTP Session interface used by clear RTP and SDES-SRTP.
type dtlsSrtpSession struct {
	log       logger.Logger
	conf      *dtlsMediaConfig
	raw       *udpConn
	mux       *dtlsMux
	ready     chan struct{}
	mu        sync.RWMutex
	err       error
	srtp      *psrtp.SessionSRTP
	srtcp     *psrtp.SessionSRTCP
	dtls      *pdtls.Conn
	iceAgent  *pice.Agent
	iceMux    pice.UDPMux
	remote    net.Addr
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	closeOnce sync.Once
}

func newDTLSSRTPSession(log logger.Logger, conn *udpConn, conf *dtlsMediaConfig, timeout time.Duration, remote net.Addr) *dtlsSrtpSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &dtlsSrtpSession{log: log, conf: conf, raw: conn, ready: make(chan struct{}), remote: remote, ctx: ctx, cancel: cancel}
	if conf.ice == nil {
		s.mux = newDTLSMux(conn)
	}
	go s.start(timeout)
	return s
}

func (s *dtlsSrtpSession) start(timeout time.Duration) {
	defer close(s.ready)
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return
	}
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("DTLS peer sent no certificate")
		}
		d := sha256.Sum256(raw[0])
		got := make([]string, len(d))
		for i, b := range d {
			got[i] = fmt.Sprintf("%02X", b)
		}
		if !strings.EqualFold(strings.Join(got, ":"), s.conf.remoteFingerprint) {
			return errors.New("DTLS peer fingerprint mismatch")
		}
		return nil
	}
	cfg := &pdtls.Config{Certificates: []tls.Certificate{s.conf.certificate.certificate}, InsecureSkipVerify: true, VerifyPeerCertificate: verify, ClientAuth: pdtls.RequireAnyClientCert, SRTPProtectionProfiles: []pdtls.SRTPProtectionProfile{pdtls.SRTP_AEAD_AES_128_GCM, pdtls.SRTP_AES128_CM_HMAC_SHA1_80}}
	if s.conf.ice != nil {
		iceConn, err := s.connectICE(ctx, s.conf.ice)
		if err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.log.Warnw("ICE connectivity failed", err)
			return
		}
		mux := newDTLSMux(iceConn)
		s.mu.Lock()
		closed := s.closed
		if !closed {
			s.mux = mux
		}
		s.mu.Unlock()
		if closed {
			_ = mux.Close()
			return
		}
	}
	var c *pdtls.Conn
	var srtpSession *psrtp.SessionSRTP
	var srtcpSession *psrtp.SessionSRTCP
	var err error
	if s.conf.isClient {
		c, err = pdtls.Client(s.mux.dtls, s.remote, cfg)
	} else {
		c, err = pdtls.Server(s.mux.dtls, s.remote, cfg)
	}
	if err == nil {
		err = c.HandshakeContext(ctx)
	}
	if err == nil {
		profile, ok := c.SelectedSRTPProtectionProfile()
		if !ok {
			err = errors.New("DTLS did not negotiate an SRTP profile")
		}
		var sp psrtp.ProtectionProfile
		if err == nil {
			switch profile {
			case pdtls.SRTP_AEAD_AES_128_GCM:
				sp = psrtp.ProtectionProfileAeadAes128Gcm
			case pdtls.SRTP_AES128_CM_HMAC_SHA1_80:
				sp = psrtp.ProtectionProfileAes128CmHmacSha1_80
			default:
				err = fmt.Errorf("unsupported DTLS-SRTP profile %v", profile)
			}
		}
		if err == nil {
			state, ok := c.ConnectionState()
			if !ok {
				err = errors.New("DTLS connection state unavailable")
			}
			if err == nil {
				scfg := &psrtp.Config{Profile: sp}
				err = scfg.ExtractSessionKeysFromDTLS(&state, s.conf.isClient)
				if err == nil {
					srtpSession, err = psrtp.NewSessionSRTP(s.mux.srtp, scfg)
				}
				if err == nil {
					srtcpSession, err = psrtp.NewSessionSRTCP(s.mux.srtcp, scfg)
				}
			}
		}
	}
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.dtls, s.srtp, s.srtcp, s.err = c, srtpSession, srtcpSession, err
	}
	s.mu.Unlock()
	if closed {
		if srtpSession != nil {
			_ = srtpSession.Close()
		}
		if srtcpSession != nil {
			_ = srtcpSession.Close()
		}
		if c != nil {
			_ = c.Close()
		}
		return
	}
	if err != nil {
		s.log.Warnw("DTLS-SRTP handshake failed", err)
	} else {
		s.log.Infow("DTLS-SRTP handshake complete", "role", s.conf.localSetup)
	}
}

// connectICE runs the controlling side of ICE against Meta's ICE-lite offer.
// The returned Conn carries only selected-pair application data; STUN remains
// inside Pion, so the DTLS/RTP mux sees the same transport boundary as before.
func (s *dtlsSrtpSession) connectICE(ctx context.Context, c *dtlsICEConfig) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pc := icePacketConn{udpConn: s.raw}
	mux := pice.NewUDPMuxDefault(pice.UDPMuxParams{UDPConn: pc})
	closeICE := func(agent *pice.Agent) {
		if agent != nil {
			_ = agent.Close()
		}
		_ = mux.Close()
	}
	agent, err := pice.NewAgent(&pice.AgentConfig{
		LocalUfrag: c.localUfrag, LocalPwd: c.localPwd,
		NetworkTypes:           []pice.NetworkType{pice.NetworkTypeUDP4},
		CandidateTypes:         []pice.CandidateType{pice.CandidateTypeHost},
		NAT1To1IPs:             []string{c.local.Addr().String()},
		NAT1To1IPCandidateType: pice.CandidateTypeHost,
		UDPMux:                 mux,
	})
	if err != nil {
		closeICE(nil)
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.iceAgent = agent
		s.iceMux = mux
	}
	s.mu.Unlock()
	if closed {
		closeICE(agent)
		return nil, context.Canceled
	}
	done := make(chan struct{})
	agent.OnCandidate(func(candidate pice.Candidate) {
		if candidate == nil {
			close(done)
		}
	})
	if err = agent.GatherCandidates(); err != nil {
		closeICE(agent)
		return nil, err
	}
	select {
	case <-done:
	case <-ctx.Done():
		closeICE(agent)
		return nil, ctx.Err()
	}
	if err = addRemoteICECandidates(ctx, agent, c.remoteCandidates); err != nil {
		closeICE(agent)
		return nil, err
	}
	return agent.Dial(ctx, c.remoteUfrag, c.remotePwd)
}

func addRemoteICECandidates(ctx context.Context, agent *pice.Agent, candidates []dtlsICECandidate) error {
	for _, remoteConfig := range candidates {
		remote, candidateErr := pice.UnmarshalCandidate(remoteConfig.raw)
		if candidateErr != nil {
			return candidateErr
		}
		if err := agent.AddRemoteCandidate(remote); err != nil {
			return err
		}
	}
	// AddRemoteCandidate schedules its update on the agent loop. Do not start
	// checks until every advertised candidate is visible to that loop.
	for {
		remote, err := agent.GetRemoteCandidates()
		if err != nil {
			return err
		}
		if len(remote) >= len(candidates) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// icePacketConn lets Pion interrupt reads without taking ownership of the
// media port's underlying socket. A transport rebuild reuses that socket.
type icePacketConn struct{ *udpConn }

func (c icePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, err := c.ReadFromUDPAddrPort(b)
	return n, net.UDPAddrFromAddrPort(a), err
}
func (c icePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	a, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, errors.New("ICE destination is not UDP")
	}
	return c.WriteToUDPAddrPort(b, a.AddrPort())
}

func (s *dtlsSrtpSession) wait() (*psrtp.SessionSRTP, error) {
	select {
	case <-s.ready:
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.srtp == nil {
		return nil, io.EOF
	}
	return s.srtp, nil
}
func (s *dtlsSrtpSession) OpenWriteStream() (rtp.WriteStream, error) {
	return dtlsWriteStream{s: s}, nil
}
func (s *dtlsSrtpSession) AcceptStream() (rtp.ReadStream, uint32, error) {
	x, err := s.wait()
	if err != nil {
		return nil, 0, err
	}
	r, ssrc, err := x.AcceptStream()
	if err != nil {
		return nil, 0, err
	}
	return dtlsReadStream{r}, ssrc, nil
}
func (s *dtlsSrtpSession) Close() error {
	s.closeOnce.Do(func() {
		// Cancel startup first so ICE gathering/checks, DTLS handshaking and
		// callers waiting for readiness all become interruptible immediately.
		s.cancel()
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		s.closeTransports()
		<-s.ready
		// Catch any resource whose creation was already in flight when the
		// first snapshot was taken. start has exited before this second pass.
		s.closeTransports()
	})
	return nil
}

func (s *dtlsSrtpSession) closeTransports() {
	// Never hold the session mutex while closing Pion transports. Their Close
	// methods may wait for readers that need the same session state to finish.
	s.mu.RLock()
	agent, iceMux, mux := s.iceAgent, s.iceMux, s.mux
	srtpSession, srtcpSession, dtlsConn := s.srtp, s.srtcp, s.dtls
	s.mu.RUnlock()

	if agent != nil {
		_ = agent.Close()
	}
	if iceMux != nil {
		_ = iceMux.Close()
	}
	if mux != nil {
		_ = mux.Close()
	}
	if srtpSession != nil {
		_ = srtpSession.Close()
	}
	if srtcpSession != nil {
		_ = srtcpSession.Close()
	}
	if dtlsConn != nil {
		_ = dtlsConn.Close()
	}
}

type dtlsWriteStream struct{ s *dtlsSrtpSession }

func (w dtlsWriteStream) String() string { return "DTLS-SRTPWriteStream" }
func (w dtlsWriteStream) WriteRTP(h *prtp.Header, payload []byte) (int, error) {
	x, err := w.s.wait()
	if err != nil {
		return 0, err
	}
	out, err := x.OpenWriteStream()
	if err != nil {
		return 0, err
	}
	return out.WriteRTP(h, payload)
}

type dtlsReadStream struct{ r *psrtp.ReadStreamSRTP }

func (r dtlsReadStream) ReadRTP(h *prtp.Header, payload []byte) (int, error) {
	n, err := r.r.Read(payload)
	if err != nil {
		return 0, err
	}
	var p prtp.Packet
	if err = p.Unmarshal(payload[:n]); err != nil {
		return 0, err
	}
	*h = p.Header
	return copy(payload, p.Payload), nil
}

type muxPacket struct {
	b    []byte
	addr net.Addr
}
type dtlsEndpoint struct {
	parent  *dtlsMux
	packets chan muxPacket
	closed  chan struct{}
	once    sync.Once
}
type dtlsMux struct {
	conn              net.Conn
	dtls, srtp, srtcp *dtlsEndpoint
	closed            chan struct{}
	once              sync.Once
}

func newDTLSEndpoint(m *dtlsMux) *dtlsEndpoint {
	return &dtlsEndpoint{parent: m, packets: make(chan muxPacket, 128), closed: make(chan struct{})}
}
func newDTLSMux(c net.Conn) *dtlsMux {
	m := &dtlsMux{conn: c, closed: make(chan struct{})}
	m.dtls = newDTLSEndpoint(m)
	m.srtp = newDTLSEndpoint(m)
	m.srtcp = newDTLSEndpoint(m)
	go m.readLoop()
	return m
}
func (m *dtlsMux) readLoop() {
	b := make([]byte, 2048)
	for {
		n, err := m.conn.Read(b)
		if err != nil {
			m.Close()
			return
		}
		if n == 0 {
			continue
		}
		p := muxPacket{b: append([]byte(nil), b[:n]...), addr: m.conn.RemoteAddr()}
		var e *dtlsEndpoint
		if p.b[0] >= 20 && p.b[0] <= 63 {
			e = m.dtls
		} else if p.b[0] >= 128 && p.b[0] <= 191 {
			if len(p.b) > 1 && p.b[1] >= 192 && p.b[1] <= 223 {
				e = m.srtcp
			} else {
				e = m.srtp
			}
		} else {
			continue
		}
		select {
		case e.packets <- p:
		case <-e.closed:
		case <-m.closed:
			return
		}
	}
}
func (m *dtlsMux) Close() error {
	m.once.Do(func() { close(m.closed); _ = m.dtls.Close(); _ = m.srtp.Close(); _ = m.srtcp.Close() })
	return nil
}
func (e *dtlsEndpoint) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-e.closed:
		return 0, nil, io.EOF
	case p := <-e.packets:
		return copy(b, p.b), p.addr, nil
	}
}
func (e *dtlsEndpoint) WriteTo(b []byte, _ net.Addr) (int, error) {
	select {
	case <-e.closed:
		return 0, io.EOF
	default:
		return e.parent.conn.Write(b)
	}
}
func (e *dtlsEndpoint) Read(b []byte) (int, error)       { n, _, err := e.ReadFrom(b); return n, err }
func (e *dtlsEndpoint) Write(b []byte) (int, error)      { return e.WriteTo(b, nil) }
func (e *dtlsEndpoint) RemoteAddr() net.Addr             { return e.parent.conn.RemoteAddr() }
func (e *dtlsEndpoint) Close() error                     { e.once.Do(func() { close(e.closed) }); return nil }
func (e *dtlsEndpoint) LocalAddr() net.Addr              { return e.parent.conn.LocalAddr() }
func (e *dtlsEndpoint) SetDeadline(time.Time) error      { return nil }
func (e *dtlsEndpoint) SetReadDeadline(time.Time) error  { return nil }
func (e *dtlsEndpoint) SetWriteDeadline(time.Time) error { return nil }
