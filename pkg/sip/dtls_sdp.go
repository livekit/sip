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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"strings"
	"time"

	pice "github.com/pion/ice/v4"
	psdp "github.com/pion/sdp/v3"
)

var errDTLSSDP = errors.New("invalid DTLS-SRTP SDP")

type dtlsCertificate struct {
	certificate tls.Certificate
	fingerprint string
}

func newDTLSCertificate() (*dtlsCertificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "livekit-sip-dtls"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	parts := make([]string, len(digest))
	for i, b := range digest {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return &dtlsCertificate{certificate: tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}, fingerprint: strings.Join(parts, ":")}, nil
}

type dtlsMediaConfig struct {
	remoteFingerprint string
	remoteSetup       string
	localSetup        string
	isClient          bool
	certificate       *dtlsCertificate
	ice               *dtlsICEConfig
}

// dtlsICEConfig is deliberately transport metadata. Meta advertises ICE-lite,
// therefore LiveKit must act as the controlling ICE agent before DTLS starts.
type dtlsICEConfig struct {
	remoteUfrag, remotePwd string
	remoteCandidates       []dtlsICECandidate
	localUfrag, localPwd   string
	local                  netip.AddrPort
}

// dtlsICECandidate keeps the complete candidate line used to construct the
// Pion candidate, plus the fields that determine transport identity. Keeping
// the original metadata is important for priorities and non-host candidates.
type dtlsICECandidate struct {
	foundation string
	component  uint16
	priority   uint32
	address    netip.AddrPort
	typ        pice.CandidateType
	related    *pice.CandidateRelatedAddress
	extensions []pice.CandidateExtension
	raw        string
}

func mediaAttribute(m *psdp.MediaDescription, key string) (string, bool) {
	for _, a := range m.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}

func sessionAttribute(s *psdp.SessionDescription, key string) (string, bool) {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}

func parseDTLSOffer(raw []byte, cert *dtlsCertificate) (*dtlsMediaConfig, error) {
	var s psdp.SessionDescription
	if err := s.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", errDTLSSDP, err)
	}
	for _, m := range s.MediaDescriptions {
		if m.MediaName.Media != "audio" || !strings.EqualFold(strings.Join(m.MediaName.Protos, "/"), "UDP/TLS/RTP/SAVPF") {
			continue
		}
		if cert == nil {
			return nil, fmt.Errorf("%w: DTLS-SRTP is not configured", errDTLSSDP)
		}
		fp, ok := mediaAttribute(m, "fingerprint")
		if !ok {
			fp, ok = sessionAttribute(&s, "fingerprint")
		}
		if !ok {
			return nil, fmt.Errorf("%w: fingerprint missing", errDTLSSDP)
		}
		fields := strings.Fields(fp)
		if len(fields) != 2 || !strings.EqualFold(fields[0], "sha-256") {
			return nil, fmt.Errorf("%w: SHA-256 fingerprint required", errDTLSSDP)
		}
		v := strings.ReplaceAll(fields[1], ":", "")
		decoded, err := hex.DecodeString(v)
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("%w: malformed fingerprint", errDTLSSDP)
		}
		setup, ok := mediaAttribute(m, "setup")
		if !ok {
			setup, ok = sessionAttribute(&s, "setup")
		}
		if !ok {
			return nil, fmt.Errorf("%w: setup missing", errDTLSSDP)
		}
		if _, ok = mediaAttribute(m, "rtcp-mux"); !ok {
			return nil, fmt.Errorf("%w: rtcp-mux required", errDTLSSDP)
		}
		out := &dtlsMediaConfig{remoteFingerprint: strings.ToUpper(fields[1]), remoteSetup: strings.ToLower(setup), certificate: cert}
		if ufrag, hasUfrag := mediaAttribute(m, "ice-ufrag"); hasUfrag {
			pwd, hasPwd := mediaAttribute(m, "ice-pwd")
			if !hasPwd || ufrag == "" || pwd == "" {
				return nil, fmt.Errorf("%w: incomplete ICE credentials", errDTLSSDP)
			}
			remoteCandidates, err := iceRemoteCandidates(m)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errDTLSSDP, err)
			}
			lu, err := iceCredential(8)
			if err != nil {
				return nil, err
			}
			lp, err := iceCredential(24)
			if err != nil {
				return nil, err
			}
			out.ice = &dtlsICEConfig{remoteUfrag: ufrag, remotePwd: pwd, remoteCandidates: remoteCandidates, localUfrag: lu, localPwd: lp}
		}
		switch strings.ToLower(setup) {
		case "actpass", "active":
			out.localSetup, out.isClient = "passive", false
		case "passive":
			out.localSetup, out.isClient = "active", true
		default:
			return nil, fmt.Errorf("%w: unsupported setup role %q", errDTLSSDP, setup)
		}
		return out, nil
	}
	return nil, nil
}

func iceCredential(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func iceRemoteCandidates(m *psdp.MediaDescription) ([]dtlsICECandidate, error) {
	var out []dtlsICECandidate
	seen := make(map[string]struct{})
	for _, a := range m.Attributes {
		if a.Key != "candidate" {
			continue
		}
		candidate, err := pice.UnmarshalCandidate(a.Value)
		if err != nil || candidate.Component() != pice.ComponentRTP || candidate.NetworkType() != pice.NetworkTypeUDP4 {
			continue
		}
		ip, err := netip.ParseAddr(candidate.Address())
		if err != nil || !ip.Is4() || candidate.Port() <= 0 || candidate.Port() > 65535 {
			continue
		}
		raw := candidate.Marshal()
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, dtlsICECandidate{
			foundation: candidate.Foundation(),
			component:  candidate.Component(),
			priority:   candidate.Priority(),
			address:    netip.AddrPortFrom(ip, uint16(candidate.Port())),
			typ:        candidate.Type(),
			related:    candidate.RelatedAddress(),
			extensions: candidate.Extensions(),
			raw:        raw,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("ICE candidate missing")
	}
	return out, nil
}

func sameDTLSRemoteTransport(a, b *dtlsMediaConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.remoteFingerprint != b.remoteFingerprint || a.remoteSetup != b.remoteSetup || a.localSetup != b.localSetup || a.isClient != b.isClient {
		return false
	}
	if a.ice == nil || b.ice == nil {
		return a.ice == b.ice
	}
	if a.ice.remoteUfrag != b.ice.remoteUfrag || a.ice.remotePwd != b.ice.remotePwd || len(a.ice.remoteCandidates) != len(b.ice.remoteCandidates) {
		return false
	}
	for i := range a.ice.remoteCandidates {
		if a.ice.remoteCandidates[i].raw != b.ice.remoteCandidates[i].raw {
			return false
		}
	}
	return true
}

func reuseDTLSLocalTransport(dst, active *dtlsMediaConfig) {
	if dst == nil || dst.ice == nil || active == nil || active.ice == nil {
		return
	}
	dst.ice.localUfrag = active.ice.localUfrag
	dst.ice.localPwd = active.ice.localPwd
	dst.ice.local = active.ice.local
}

func addDTLSAnswer(answer *psdp.SessionDescription, d *dtlsMediaConfig) error {
	for _, m := range answer.MediaDescriptions {
		if m.MediaName.Media != "audio" {
			continue
		}
		m.MediaName.Protos = []string{"UDP", "TLS", "RTP", "SAVPF"}
		attrs := m.Attributes[:0]
		for _, a := range m.Attributes {
			if a.Key != "crypto" && a.Key != "fingerprint" && a.Key != "setup" && a.Key != "rtcp-mux" {
				attrs = append(attrs, a)
			}
		}
		m.Attributes = append(attrs, psdp.Attribute{Key: "fingerprint", Value: "sha-256 " + d.certificate.fingerprint}, psdp.Attribute{Key: "setup", Value: d.localSetup}, psdp.Attribute{Key: "rtcp-mux"})
		if d.ice != nil {
			if answer.ConnectionInformation == nil || answer.ConnectionInformation.Address == nil {
				return fmt.Errorf("%w: answer has no connection address", errDTLSSDP)
			}
			ip, err := netip.ParseAddr(answer.ConnectionInformation.Address.Address)
			if err != nil {
				return fmt.Errorf("%w: invalid answer address", errDTLSSDP)
			}
			d.ice.local = netip.AddrPortFrom(ip, uint16(m.MediaName.Port.Value))
			m.Attributes = append(m.Attributes,
				psdp.Attribute{Key: "ice-ufrag", Value: d.ice.localUfrag},
				psdp.Attribute{Key: "ice-pwd", Value: d.ice.localPwd},
				psdp.Attribute{Key: "candidate", Value: fmt.Sprintf("1 1 udp 2130706431 %s %d typ host", d.ice.local.Addr(), d.ice.local.Port())},
			)
		}
		return nil
	}
	return fmt.Errorf("%w: answer has no audio media", errDTLSSDP)
}
