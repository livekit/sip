// Copyright 2023 LiveKit, Inc.
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

package e2ee

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

// EncryptFunc matches the signature of lksdk.EncryptGCM*SampleCustomCipher functions.
// Used to inject codec-specific encryption without importing lksdk (avoids circular dep).
type EncryptFunc func(payload []byte, kid uint8, cipherBlock cipher.Block) ([]byte, error)

// DecryptFunc matches the signature of lksdk.DecryptGCM*SampleCustomCipher functions.
type DecryptFunc func(payload, sifTrailer []byte, cipherBlock cipher.Block) ([]byte, error)

// cipherState holds a cached cipher block and the key material it was derived from.
// keyBytes is used by GCMFrameDecryptor to detect when the keyProvider has refreshed
// a key at a given index and the cached block needs to be rebuilt.
type cipherState struct {
	block    cipher.Block
	keyIndex uint32
	keyBytes []byte
}

// GCMFrameEncryptor encrypts media frames using AES-128-GCM.
// It wraps a KeyProvider and a codec-specific EncryptFunc, caching the
// cipher block via atomic.Pointer and only taking a mutex during key rotation.
type GCMFrameEncryptor struct {
	keyProvider types.KeyProvider
	encryptFn   EncryptFunc
	state       atomic.Pointer[cipherState]
	rotateMu    sync.Mutex // only held during key rotation
}

// NewGCMFrameEncryptor creates a frame encryptor for the given key provider and
// codec-specific encrypt function.
func NewGCMFrameEncryptor(keyProvider types.KeyProvider, encryptFn EncryptFunc) (*GCMFrameEncryptor, error) {
	idx := keyProvider.CurrentKeyIndex()
	key, err := keyProvider.GetKey(idx)
	if err != nil {
		return nil, fmt.Errorf("get initial key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	e := &GCMFrameEncryptor{
		keyProvider: keyProvider,
		encryptFn:   encryptFn,
	}
	e.state.Store(&cipherState{block: block, keyIndex: idx})
	return e, nil
}

// EncryptFrame encrypts a complete media frame.
func (e *GCMFrameEncryptor) EncryptFrame(payload []byte) ([]byte, error) {
	idx := e.keyProvider.CurrentKeyIndex()
	st := e.state.Load()

	// Fast path: key index unchanged, use cached cipher block (no lock).
	if st.keyIndex == idx {
		return e.encryptFn(payload, uint8(idx), st.block)
	}

	// Slow path: key rotated, re-derive cipher block under mutex.
	return e.rotateAndEncrypt(payload, idx)
}

func (e *GCMFrameEncryptor) rotateAndEncrypt(payload []byte, idx uint32) ([]byte, error) {
	e.rotateMu.Lock()
	defer e.rotateMu.Unlock()

	// Double-check: another goroutine may have already rotated.
	st := e.state.Load()
	if st.keyIndex == idx {
		return e.encryptFn(payload, uint8(idx), st.block)
	}

	key, err := e.keyProvider.GetKey(idx)
	if err != nil {
		return nil, fmt.Errorf("get key for index %d: %w", idx, err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	e.state.Store(&cipherState{block: block, keyIndex: idx})
	return e.encryptFn(payload, uint8(idx), block)
}

// GCMFrameDecryptor decrypts media frames using AES-128-GCM.
// It reads the KID (key index) from each frame's trailer to support
// multi-key scenarios and key rotation.
type GCMFrameDecryptor struct {
	keyProvider types.KeyProvider
	decryptFn   DecryptFunc
	sifTrailer  []byte
	cache       sync.Map // map[uint32]*cipherState
}

// NewGCMFrameDecryptor creates a frame decryptor for the given key provider,
// codec-specific decrypt function, and SIF trailer bytes.
func NewGCMFrameDecryptor(keyProvider types.KeyProvider, decryptFn DecryptFunc, sifTrailer []byte) *GCMFrameDecryptor {
	return &GCMFrameDecryptor{
		keyProvider: keyProvider,
		decryptFn:   decryptFn,
		sifTrailer:  sifTrailer,
	}
}

// DecryptFrame decrypts a complete media frame. Returns (nil, nil) for
// server-injected frames (SIF) that should be dropped.
func (d *GCMFrameDecryptor) DecryptFrame(payload []byte) ([]byte, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("frame too short to decrypt")
	}

	// Check for a SIF (server-injected frame) trailer before resolving a
	// cipher block: SIFs are unencrypted, and their last byte is part of the
	// sentinel trailer — not a key index. Trying to look up a key for that
	// byte would fail with "no key at index N".
	if len(d.sifTrailer) > 0 && len(payload) >= len(d.sifTrailer) {
		if bytes.Equal(payload[len(payload)-len(d.sifTrailer):], d.sifTrailer) {
			return nil, nil
		}
	}

	// Read KID from the last byte of the frame trailer.
	kid := uint32(payload[len(payload)-1])

	block, err := d.getCachedBlock(kid)
	if err != nil {
		return nil, err
	}

	return d.decryptFn(payload, d.sifTrailer, block)
}

func (d *GCMFrameDecryptor) getCachedBlock(kid uint32) (cipher.Block, error) {
	currentKey, err := d.keyProvider.GetKey(kid)
	if err != nil {
		return nil, fmt.Errorf("get key for index %d: %w", kid, err)
	}
	if v, ok := d.cache.Load(kid); ok {
		if s := v.(*cipherState); bytes.Equal(s.keyBytes, currentKey) {
			return s.block, nil
		}
	}
	block, err := aes.NewCipher(currentKey)
	if err != nil {
		return nil, err
	}
	d.cache.Store(kid, &cipherState{
		block:    block,
		keyIndex: kid,
		keyBytes: append([]byte(nil), currentKey...),
	})
	return block, nil
}
