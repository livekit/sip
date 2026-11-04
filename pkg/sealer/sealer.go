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

// Package sealer encrypts and authenticates small payloads with keys shared by a group of
// servers, so that a value sealed by one server can be opened by any other server holding
// one of the same keys.
package sealer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ErrInvalid is returned by Open for any sealed value it cannot open: truncated, tampered with,
// sealed with an unknown key, or opened with different associated data.
// The causes are deliberately indistinguishable.
var ErrInvalid = errors.New("sealer: invalid sealed value")

// Sealer encrypts and authenticates data.
//
// Implementations are safe for concurrent use.
type Sealer interface {
	// Seal encrypts and authenticates plain, authenticates aad, and appends the result to dst.
	// aad is not part of the output: Open must be given the same aad.
	// The result is self-contained: it carries everything Open needs besides the key and aad.
	// Sealing the same input twice gives different results.
	// dst and plain must not overlap.
	Seal(dst, plain, aad []byte) ([]byte, error)

	// Open authenticates and decrypts sealed, a value returned by Seal, and appends the plaintext to dst.
	// It returns ErrInvalid if sealed was not produced by Seal with an accepted key and the same aad.
	// dst and sealed must not overlap.
	Open(dst, sealed, aad []byte) ([]byte, error)

	// Overhead is the number of bytes Seal adds to the plaintext.
	Overhead() int
}

var _ Sealer = (*AESGCM)(nil)

// AESGCM is a Sealer using AES-256-GCM with a random 96-bit nonce per Seal.
//
// Wire format: nonce (12 bytes) | ciphertext (len(plain) bytes) | tag (16 bytes).
// Each AES key is the SHA-256 of a configured key string.
//
// It holds a list of keys to allow rotation: Seal uses the first key, and Open tries each key in order.
// To rotate, deploy with the new key added last, then move it first, then drop the old one,
// waiting at each step for the rollout and for values sealed with the old key to expire.
//
// Each Seal draws a fresh random nonce, which keeps a key safe for about 2^32 seals.
type AESGCM struct {
	aeads []cipher.AEAD // in key order, aeads[0] seals
	rand  io.Reader     // nonce source
}

// NewAESGCM returns an AESGCM sealing with keys[0] and opening with any of keys.
// It fails if keys is empty or holds an empty key.
//
// Keys are secrets: use high-entropy values. To use one secret for several purposes,
// bind the purpose into aad, so that a value sealed for one purpose doesn't open for another.
func NewAESGCM(keys []string) (*AESGCM, error) {
	if len(keys) == 0 {
		return nil, errors.New("sealer: no keys")
	}
	s := &AESGCM{aeads: make([]cipher.AEAD, 0, len(keys)), rand: rand.Reader}
	for i, key := range keys {
		if key == "" {
			return nil, fmt.Errorf("sealer: key %d is empty", i)
		}
		k := sha256.Sum256([]byte(key))
		block, err := aes.NewCipher(k[:])
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		s.aeads = append(s.aeads, aead)
	}
	return s, nil
}

const (
	nonceSize = 12
	tagSize   = 16
)

// Overhead implements Sealer.
func (s *AESGCM) Overhead() int {
	return nonceSize + tagSize
}

// Seal implements Sealer. It fails only if the system random source fails.
func (s *AESGCM) Seal(dst, plain, aad []byte) ([]byte, error) {
	n := len(dst)
	dst = slices.Grow(dst, nonceSize+len(plain)+tagSize)[:n+nonceSize]
	nonce := dst[n:]
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return nil, fmt.Errorf("sealer: reading nonce: %w", err)
	}
	return s.aeads[0].Seal(dst, nonce, plain, aad), nil
}

// Open implements Sealer.
func (s *AESGCM) Open(dst, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < nonceSize+tagSize {
		return nil, ErrInvalid
	}
	nonce, body := sealed[:nonceSize], sealed[nonceSize:]
	for _, aead := range s.aeads {
		if out, err := aead.Open(dst, nonce, body, aad); err == nil {
			return out, nil
		}
	}
	return nil, ErrInvalid
}
