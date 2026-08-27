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

package types

// KeyProvider manages encryption keys for E2EE.
type KeyProvider interface {
	// GetKey returns the derived AES key for the given index.
	GetKey(keyIndex uint32) ([]byte, error)
	// CurrentKeyIndex returns the active key index for encryption.
	CurrentKeyIndex() uint32
}

// FrameEncryptor encrypts a complete media frame (audio or video access unit)
// before RTP packetization. Implementations are codec-aware: the codec is
// configured at construction time, not at each call.
type FrameEncryptor interface {
	EncryptFrame(payload []byte) ([]byte, error)
}

// FrameDecryptor decrypts a reassembled media frame after RTP depacketization.
// Returns (nil, nil) for server-injected frames (SIF) that should be dropped.
type FrameDecryptor interface {
	DecryptFrame(payload []byte) ([]byte, error)
}
