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

package srtp

import (
	"crypto/rand"
	"fmt"
	"net"

	prtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/media/rtp"
)

var defaultProfiles = []ProtectionProfile{
	"AES_CM_128_HMAC_SHA1_80",
	"AES_CM_128_HMAC_SHA1_32",
	"AES_256_CM_HMAC_SHA1_80",
	"AES_256_CM_HMAC_SHA1_32",
}

func DefaultProfiles() ([]Profile, error) {
	out := make([]Profile, 0, len(defaultProfiles))
	for i, p := range defaultProfiles {
		sp, err := p.Parse()
		if err != nil {
			return nil, err
		}
		keyLen, err := sp.KeyLen()
		if err != nil {
			return nil, err
		}
		saltLen, err := sp.SaltLen()
		if err != nil {
			return nil, err
		}
		key := make([]byte, keyLen)
		salt := make([]byte, saltLen)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		out = append(out, Profile{
			Index:   i + 1,
			Profile: p,
			Key:     key,
			Salt:    salt,
		})
	}
	return out, nil
}

type Options struct {
	Profiles []Profile
}

type ProtectionProfile string

func (p ProtectionProfile) Parse() (srtp.ProtectionProfile, error) {
	switch p {
	case "AES_CM_128_HMAC_SHA1_80":
		return srtp.ProtectionProfileAes128CmHmacSha1_80, nil
	case "AES_CM_128_HMAC_SHA1_32":
		return srtp.ProtectionProfileAes128CmHmacSha1_32, nil
	case "AES_256_CM_HMAC_SHA1_80":
		return srtp.ProtectionProfileAes256CmHmacSha1_80, nil
	case "AES_256_CM_HMAC_SHA1_32":
		return srtp.ProtectionProfileAes256CmHmacSha1_32, nil
	default:
		return 0, fmt.Errorf("unsupported profile %q", p)
	}
}

type Profile struct {
	Index   int
	Profile ProtectionProfile
	Key     []byte
	Salt    []byte
}

type Config = srtp.Config
type SessionKeys = srtp.SessionKeys

func NewSession(log logger.Logger, conn net.Conn, conf *Config) (rtp.Session, error) {
	s, err := srtp.NewSessionSRTP(conn, conf)
	if err != nil {
		return nil, err
	}
	return &session{log: log, s: s}, nil
}

type session struct {
	log logger.Logger
	s   *srtp.SessionSRTP
}

func (s *session) OpenWriteStream() (rtp.WriteStream, error) {
	w, err := s.s.OpenWriteStream()
	if err != nil {
		return nil, err
	}
	return writeStream{w: w}, nil
}

func (s *session) AcceptStream() (rtp.ReadStream, uint32, error) {
	r, ssrc, err := s.s.AcceptStream()
	if err != nil {
		return nil, 0, err
	}
	return readStream{r: r}, ssrc, nil
}

func (s *session) Close() error {
	return s.s.Close()
}

type writeStream struct {
	w *srtp.WriteStreamSRTP
}

func (w writeStream) WriteRTP(h *prtp.Header, payload []byte) (int, error) {
	return w.w.WriteRTP(h, payload)
}

type readStream struct {
	r *srtp.ReadStreamSRTP
}

func (r readStream) ReadRTP(h *prtp.Header, payload []byte) (int, error) {
	buf := payload
	n, err := r.r.Read(buf)
	if err != nil {
		return 0, err
	}
	var p rtp.Packet
	if err = p.Unmarshal(buf[:n]); err != nil {
		return 0, err
	}
	*h = p.Header
	n = copy(payload, p.Payload)
	return n, nil
}
