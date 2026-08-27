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

package lksdk

import (
	"fmt"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	e2eetypes "github.com/livekit/server-sdk-go/v2/e2ee/types"
)

// EncryptionOptions configures end-to-end encryption for data channel messages.
// Pass to [WithDataEncryption] to enable: all outgoing data packets are
// encrypted and incoming EncryptedPacket messages are decrypted automatically.
//
// Video/audio track encryption is configured separately per-track via
// [WithFrameEncryptor] and [NewFrameEncryptor] / [NewFrameDecryptor] below.
type EncryptionOptions struct {
	KeyProvider e2eetypes.KeyProvider
}

// Codec identifies the media codec for a frame encryptor or decryptor.
// Mirrors the JS SDK's single FrameCryptor-with-codec-field model: one
// constructor handles all supported codecs, with behavior selected by this enum.
type Codec int

const (
	// CodecH264 selects the H.264 frame encryption format (NALU-aware with
	// RBSP byte-stuffing).
	CodecH264 Codec = iota
	// CodecH265 selects the H.265 frame encryption format.
	CodecH265
	// CodecOpus selects the Opus audio frame encryption format.
	CodecOpus
)

// NewFrameEncryptor creates a [e2eetypes.FrameEncryptor] for the given codec.
// The encryptor uses the provided [e2eetypes.KeyProvider] for keys and
// automatically picks up key rotations when the provider's CurrentKeyIndex
// advances (see LiveKit docs on E2EE key rotation).
//
// Returns an error for unsupported codecs.
func NewFrameEncryptor(kp e2eetypes.KeyProvider, codec Codec) (e2eetypes.FrameEncryptor, error) {
	fn, err := encryptFuncForCodec(codec)
	if err != nil {
		return nil, err
	}
	return e2ee.NewGCMFrameEncryptor(kp, fn)
}

// NewFrameDecryptor creates a [e2ee.GCMFrameDecryptor] for the given codec.
// sifTrailer is the server-injected-frame sentinel (obtained from
// [Room.SifTrailer] after connection); SIF frames return (nil, nil) from
// DecryptFrame so callers can drop them.
//
// Returns an error for unsupported codecs.
func NewFrameDecryptor(kp e2eetypes.KeyProvider, codec Codec, sifTrailer []byte) (*e2ee.GCMFrameDecryptor, error) {
	fn, err := decryptFuncForCodec(codec)
	if err != nil {
		return nil, err
	}
	return e2ee.NewGCMFrameDecryptor(kp, fn, sifTrailer), nil
}

func encryptFuncForCodec(codec Codec) (e2ee.EncryptFunc, error) {
	switch codec {
	case CodecH264:
		return e2ee.EncryptGCMH264SampleCustomCipher, nil
	case CodecH265:
		return e2ee.EncryptGCMH265SampleCustomCipher, nil
	case CodecOpus:
		return EncryptGCMAudioSampleCustomCipher, nil
	}
	return nil, fmt.Errorf("e2ee: unsupported codec %d", codec)
}

func decryptFuncForCodec(codec Codec) (e2ee.DecryptFunc, error) {
	switch codec {
	case CodecH264:
		return e2ee.DecryptGCMH264SampleCustomCipher, nil
	case CodecH265:
		return e2ee.DecryptGCMH265SampleCustomCipher, nil
	case CodecOpus:
		return DecryptGCMAudioSampleCustomCipher, nil
	}
	return nil, fmt.Errorf("e2ee: unsupported codec %d", codec)
}

// ---------------------------------------------------------------------------
// Internal helpers delegated to e2ee package — used by engine.go
// ---------------------------------------------------------------------------

func decryptedPayloadToDataPacketValue(pck *livekit.DataPacket, payload *livekit.EncryptedPacketPayload) {
	e2ee.DecryptedPayloadToDataPacketValue(pck, payload)
}
