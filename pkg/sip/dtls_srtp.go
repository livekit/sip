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
	remote    net.Addr
	closeOnce sync.Once
}

func newDTLSSRTPSession(log logger.Logger, conn *udpConn, conf *dtlsMediaConfig, timeout time.Duration, remote net.Addr) *dtlsSrtpSession {
	s := &dtlsSrtpSession{log: log, conf: conf, raw: conn, ready: make(chan struct{}), remote: remote}
	if conf.ice == nil {
		s.mux = newDTLSMux(conn)
	}
	go s.start(timeout)
	return s
}

func (s *dtlsSrtpSession) start(timeout time.Duration) {
	defer close(s.ready)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
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
		s.mux = newDTLSMux(iceConn)
	}
	var c *pdtls.Conn
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
					s.srtp, err = psrtp.NewSessionSRTP(s.mux.srtp, scfg)
				}
				if err == nil {
					s.srtcp, err = psrtp.NewSessionSRTCP(s.mux.srtcp, scfg)
				}
			}
		}
	}
	s.mu.Lock()
	s.dtls, s.err = c, err
	s.mu.Unlock()
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
	pc := icePacketConn{UDPConn: s.raw.UDPConn}
	mux := pice.NewUDPMuxDefault(pice.UDPMuxParams{UDPConn: pc})
	agent, err := pice.NewAgent(&pice.AgentConfig{
		LocalUfrag: c.localUfrag, LocalPwd: c.localPwd,
		NetworkTypes:           []pice.NetworkType{pice.NetworkTypeUDP4},
		CandidateTypes:         []pice.CandidateType{pice.CandidateTypeHost},
		NAT1To1IPs:             []string{c.local.Addr().String()},
		NAT1To1IPCandidateType: pice.CandidateTypeHost,
		UDPMux:                 mux,
	})
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	s.mu.Lock()
	s.iceAgent = agent
	s.mu.Unlock()
	done := make(chan struct{})
	agent.OnCandidate(func(candidate pice.Candidate) {
		if candidate == nil {
			close(done)
		}
	})
	if err = agent.GatherCandidates(); err != nil {
		_ = agent.Close()
		return nil, err
	}
	select {
	case <-done:
	case <-ctx.Done():
		_ = agent.Close()
		return nil, ctx.Err()
	}
	remote, err := pice.NewCandidateHost(&pice.CandidateHostConfig{Network: "udp4", Address: c.remote.Addr().String(), Port: int(c.remote.Port())})
	if err != nil {
		_ = agent.Close()
		return nil, err
	}
	if err = agent.AddRemoteCandidate(remote); err != nil {
		_ = agent.Close()
		return nil, err
	}
	return agent.Dial(ctx, c.remoteUfrag, c.remotePwd)
}

type icePacketConn struct{ UDPConn }

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
	<-s.ready
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
		// Never hold the session mutex while closing Pion transports. Their
		// Close methods wait for reader goroutines, and those readers must be
		// free to finish while the ICE connection is being torn down.
		s.mu.RLock()
		agent, mux := s.iceAgent, s.mux
		srtpSession, srtcpSession, dtlsConn := s.srtp, s.srtcp, s.dtls
		s.mu.RUnlock()

		// ICE owns the selected net.Conn. Close it first to unblock the mux
		// read loop; otherwise SIP call teardown can remain stuck indefinitely.
		if agent != nil {
			_ = agent.Close()
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
	})
	return nil
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
