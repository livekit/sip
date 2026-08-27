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
	"crypto/aes"
	"crypto/cipher"

	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

// EncryptGCMH264Sample encrypts an H.264 video sample with AES-128-GCM.
// Use EncryptGCMH264SampleCustomCipher with a cached cipher.Block for better performance.
func EncryptGCMH264Sample(sample, key []byte, kid uint8) ([]byte, error) {
	if len(key) != types.KeySizeBytes {
		return nil, types.ErrIncorrectKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return EncryptGCMH264SampleCustomCipher(sample, kid, block)
}

// EncryptGCMH264SampleCustomCipher encrypts an H.264 video sample using a cached cipher.Block.
func EncryptGCMH264SampleCustomCipher(sample []byte, kid uint8, cipherBlock cipher.Block) ([]byte, error) {
	return encryptVideoSample(sample, kid, cipherBlock, findH264UnencryptedBytes)
}

// DecryptGCMH264Sample decrypts an H.264 video sample encrypted by
// EncryptGCMH264Sample or the JS SDK FrameCryptor.
func DecryptGCMH264Sample(sample, key, sifTrailer []byte) ([]byte, error) {
	if len(key) != types.KeySizeBytes {
		return nil, types.ErrIncorrectKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return DecryptGCMH264SampleCustomCipher(sample, sifTrailer, block)
}

// DecryptGCMH264SampleCustomCipher decrypts an H.264 video sample using a cached cipher.Block.
func DecryptGCMH264SampleCustomCipher(sample, sifTrailer []byte, cipherBlock cipher.Block) ([]byte, error) {
	return decryptVideoSample(sample, sifTrailer, cipherBlock, findH264UnencryptedBytes)
}
