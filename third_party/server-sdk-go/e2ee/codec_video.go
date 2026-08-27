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
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

// Shared H.264/H.265 frame format. Compatible with the JS SDK FrameCryptor
// (FrameCryptor.ts + naluUtils.ts):
//
//	[frameHeader (unencrypted, AAD)][ciphertext+tag][IV][IV_LENGTH][KID]
//
// The encrypted portion is RBSP-escaped to prevent accidental H.264/H.265
// start-code patterns inside the ciphertext. Samples that contain no slice
// NALUs (e.g. SPS/PPS only) pass through unmodified.

const (
	// H.264 NALU type constants.
	h264NaluTypeMask    = 0x1f
	h264NaluSliceNonIDR = 1
	h264NaluSliceIDR    = 5

	// H.265 NALU type mask (6-bit type in bits [6:1] of the first header byte).
	h265NaluTypeMask = 0x3f
)

// encryptVideoSample is the shared encryption core for H.264 and H.265.
// findUnencrypted determines how many header bytes to leave as AAD.
func encryptVideoSample(sample []byte, kid uint8, cipherBlock cipher.Block, findUnencrypted func([]byte) int) ([]byte, error) {
	if cipherBlock == nil {
		return nil, types.ErrBlockCipherRequired
	}
	if len(sample) < 4 {
		return sample, nil
	}

	unencryptedBytes := findUnencrypted(sample)
	if unencryptedBytes <= 0 || unencryptedBytes >= len(sample) {
		return sample, nil
	}

	frameHeader := make([]byte, unencryptedBytes)
	copy(frameHeader, sample[:unencryptedBytes])

	plainText := make([]byte, len(sample)-unencryptedBytes)
	copy(plainText, sample[unencryptedBytes:])

	iv := make([]byte, types.IVLength)
	if _, err := rand.Read(iv); err != nil {
		return nil, errors.Join(types.ErrUnableGenerateIV, err)
	}

	frameTrailer := []byte{types.IVLength, kid}

	aesGCM, err := cipher.NewGCMWithNonceSize(cipherBlock, types.IVLength)
	if err != nil {
		return nil, err
	}
	cipherText := aesGCM.Seal(nil, iv, plainText, frameHeader)

	encPortion := make([]byte, len(cipherText)+types.IVLength+2)
	copy(encPortion, cipherText)
	copy(encPortion[len(cipherText):], iv)
	copy(encPortion[len(cipherText)+types.IVLength:], frameTrailer)

	escaped := writeRBSP(encPortion)

	result := make([]byte, unencryptedBytes+len(escaped))
	copy(result, frameHeader)
	copy(result[unencryptedBytes:], escaped)
	return result, nil
}

// decryptVideoSample is the shared decryption core for H.264 and H.265.
func decryptVideoSample(sample, sifTrailer []byte, cipherBlock cipher.Block, findUnencrypted func([]byte) int) ([]byte, error) {
	if cipherBlock == nil {
		return nil, types.ErrBlockCipherRequired
	}
	if len(sample) < 4 {
		return sample, nil
	}

	if sifTrailer != nil && len(sample) >= len(sifTrailer) {
		if bytes.Equal(sample[len(sample)-len(sifTrailer):], sifTrailer) {
			return nil, nil
		}
	}

	unencryptedBytes := findUnencrypted(sample)
	if unencryptedBytes <= 0 || unencryptedBytes >= len(sample) {
		return sample, nil
	}

	frameHeader := sample[:unencryptedBytes]
	encryptedPortion := sample[unencryptedBytes:]

	if needsRBSPUnescaping(encryptedPortion) {
		encryptedPortion = parseRBSP(encryptedPortion)
	}

	if len(encryptedPortion) < 2+types.IVLength {
		return sample, nil
	}

	frameTrailer := encryptedPortion[len(encryptedPortion)-2:]
	ivLength := int(frameTrailer[0])
	if ivLength > len(encryptedPortion)-2 {
		return nil, types.ErrIncorrectIVLength
	}
	ivStart := len(encryptedPortion) - 2 - ivLength
	iv := make([]byte, ivLength)
	copy(iv, encryptedPortion[ivStart:ivStart+ivLength])

	cipherText := encryptedPortion[:ivStart]

	aesGCM, err := cipher.NewGCMWithNonceSize(cipherBlock, ivLength)
	if err != nil {
		return nil, err
	}
	plainText, err := aesGCM.Open(nil, iv, cipherText, frameHeader)
	if err != nil {
		return nil, err
	}

	result := make([]byte, len(frameHeader)+len(plainText))
	copy(result, frameHeader)
	copy(result[len(frameHeader):], plainText)
	return result, nil
}

// findNALUIndices finds all NALU payload start positions (after the start code).
// Supports 3-byte (0x00 0x00 0x01) and 4-byte (0x00 0x00 0x00 0x01) start codes.
// Works for both H.264 and H.265 (same start code format).
func findNALUIndices(data []byte) []int {
	var indices []int
	i := 0
	for i < len(data)-3 {
		if i < len(data)-4 && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			indices = append(indices, i+4)
			i += 4
			continue
		}
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			indices = append(indices, i+3)
			i += 3
			continue
		}
		i++
	}
	return indices
}

// findH264UnencryptedBytes returns the byte offset up to and including the first
// 2 bytes of the first slice NALU (type 1=non-IDR or 5=IDR).
// Returns -1 if no slice NALU is found.
func findH264UnencryptedBytes(data []byte) int {
	for _, idx := range findNALUIndices(data) {
		if idx >= len(data) {
			continue
		}
		naluType := data[idx] & h264NaluTypeMask
		if naluType == h264NaluSliceNonIDR || naluType == h264NaluSliceIDR {
			return idx + 2
		}
	}
	return -1
}

// findH265UnencryptedBytes returns the byte offset up to and including the first
// 2 bytes of the first VCL slice NALU.
// H.265 NALU header is 2 bytes; type is in bits [6:1] of byte 0.
// Returns -1 if no slice NALU is found.
func findH265UnencryptedBytes(data []byte) int {
	for _, idx := range findNALUIndices(data) {
		if idx >= len(data) {
			continue
		}
		naluType := (data[idx] >> 1) & h265NaluTypeMask
		if isH265SliceNALU(naluType) {
			return idx + 2
		}
	}
	return -1
}

// isH265SliceNALU returns true if the NALU type is a VCL slice.
// Matches JS SDK isH265SliceNALU in naluUtils.ts exactly:
//   - Types 0-9:   TRAIL_N/R, TSA_N/R, STSA_N/R, RADL_N/R, RASL_N/R
//   - Types 16-21: BLA_W_LP, BLA_W_RADL, BLA_N_LP, IDR_W_RADL, IDR_N_LP, CRA_NUT
func isH265SliceNALU(naluType uint8) bool {
	return naluType <= 9 || (naluType >= 16 && naluType <= 21)
}

// writeRBSP applies RBSP byte-stuffing: inserts 0x03 emulation prevention bytes
// to avoid patterns 0x00 0x00 {0x00..0x03} in encrypted data.
func writeRBSP(data []byte) []byte {
	numConsecutiveZeros := 0
	var out []byte
	for _, b := range data {
		if b <= 3 && numConsecutiveZeros >= 2 {
			out = append(out, 3)
			numConsecutiveZeros = 0
		}
		out = append(out, b)
		if b == 0 {
			numConsecutiveZeros++
		} else {
			numConsecutiveZeros = 0
		}
	}
	return out
}

// parseRBSP removes RBSP emulation prevention bytes (0x03 after 0x00 0x00).
func parseRBSP(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); {
		if i+2 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 3 {
			out = append(out, 0, 0)
			i += 3
		} else {
			out = append(out, data[i])
			i++
		}
	}
	return out
}

// needsRBSPUnescaping checks if data contains any RBSP emulation prevention bytes.
func needsRBSPUnescaping(data []byte) bool {
	for i := 0; i < len(data)-2; i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 3 {
			return true
		}
	}
	return false
}
