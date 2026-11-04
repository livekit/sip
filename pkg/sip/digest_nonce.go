// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/livekit/sip/pkg/sealer"
)

// digestNonceTTL bounds how long a digest challenge can be answered.
// It matches how long we wait for the authenticated retry before finalizing the call.
const digestNonceTTL = authChallengeTimeout

var errDigestNonceExpired = errors.New("digest nonce expired")

// digestNonceAAD binds a nonce to its purpose, so a sealer key shared with another use can't
// open it, and to the Call-ID, which RFC 3261 §8.1.1.4 keeps for the authenticated retry.
func digestNonceAAD(sipCallID string) []byte {
	return append([]byte("sip-digest-nonce\x00"), sipCallID...)
}

// issueDigestNonce returns a nonce for a digest challenge: the creation time sealed for the Call-ID.
// Any server sharing the sealer's keys can verify it, so the retry may reach another server.
func issueDigestNonce(s sealer.Sealer, sipCallID string, now time.Time) (string, error) {
	plain := binary.BigEndian.AppendUint64(nil, uint64(now.UnixMicro()))
	b, err := s.Seal(nil, plain, digestNonceAAD(sipCallID))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// checkDigestNonce verifies that nonce was issued for the Call-ID within digestNonceTTL.
func checkDigestNonce(s sealer.Sealer, nonce, sipCallID string, now time.Time) error {
	b, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil {
		return fmt.Errorf("%w: %w", sealer.ErrInvalid, err)
	}
	plain, err := s.Open(nil, b, digestNonceAAD(sipCallID))
	if err != nil {
		return err
	}
	if len(plain) != 8 {
		return fmt.Errorf("%w: timestamp length %d", sealer.ErrInvalid, len(plain))
	}
	created := time.UnixMicro(int64(binary.BigEndian.Uint64(plain)))
	// Symmetric bound: the issuer may be another server whose clock runs ahead.
	if age := now.Sub(created); age > digestNonceTTL || age < -digestNonceTTL {
		return fmt.Errorf("%w: age %v", errDigestNonceExpired, age)
	}
	return nil
}
