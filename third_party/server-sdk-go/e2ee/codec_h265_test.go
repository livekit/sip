package e2ee_test

import (
	"crypto/aes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

// makeH265Frame builds a minimal H.265 Annex-B frame with the given NALU types.
// H.265 NALU header is 2 bytes: type in byte[0] bits [6:1], LayerID+TID in byte[1].
func makeH265Frame(naluTypes ...uint8) []byte {
	var frame []byte
	for _, t := range naluTypes {
		frame = append(frame, 0x00, 0x00, 0x00, 0x01)  // 4-byte start code
		frame = append(frame, (t<<1)&0x7E, 0x01)       // NALU header (2 bytes): type in bits [6:1], TID=1
		frame = append(frame, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22)
	}
	return frame
}

func TestEncryptH265Sample(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)

	// Frame with VPS (32) + SPS (33) + PPS (34) + IDR_W_RADL (19).
	frame := makeH265Frame(32, 33, 34, 19)

	encrypted, err := e2ee.EncryptGCMH265Sample(frame, key, 0)
	require.NoError(t, err)
	require.NotEqual(t, frame, encrypted)

	decrypted, err := e2ee.DecryptGCMH265Sample(encrypted, key, nil)
	require.NoError(t, err)
	require.Equal(t, frame, decrypted)
}

func TestH265VpsOnlyPassthrough(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)

	// Frame with only VPS (32) + SPS (33) + PPS (34) — no slice.
	frame := makeH265Frame(32, 33, 34)

	encrypted, err := e2ee.EncryptGCMH265Sample(frame, key, 0)
	require.NoError(t, err)
	require.Equal(t, frame, encrypted)
}

func TestEncryptH265CustomCipher(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	frame := makeH265Frame(33, 0) // SPS + TRAIL_N slice

	encrypted, err := e2ee.EncryptGCMH265SampleCustomCipher(frame, 0, block)
	require.NoError(t, err)

	decrypted, err := e2ee.DecryptGCMH265SampleCustomCipher(encrypted, nil, block)
	require.NoError(t, err)
	require.Equal(t, frame, decrypted)
}

func TestH265AllSliceTypes(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	// All valid VCL types from JS SDK isH265SliceNALU.
	sliceTypes := []uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 16, 17, 18, 19, 20, 21}
	for _, st := range sliceTypes {
		frame := makeH265Frame(32, st) // VPS + slice
		encrypted, err := e2ee.EncryptGCMH265SampleCustomCipher(frame, 0, block)
		require.NoError(t, err, "type %d", st)
		require.NotEqual(t, frame, encrypted, "type %d should encrypt", st)

		decrypted, err := e2ee.DecryptGCMH265SampleCustomCipher(encrypted, nil, block)
		require.NoError(t, err, "type %d", st)
		require.Equal(t, frame, decrypted, "type %d round-trip", st)
	}

	// Reserved types (10-15, 22-31) — should NOT be treated as slices.
	reservedTypes := []uint8{10, 11, 12, 13, 14, 15, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}
	for _, rt := range reservedTypes {
		frame := makeH265Frame(rt) // only this NALU, no other slices
		encrypted, err := e2ee.EncryptGCMH265SampleCustomCipher(frame, 0, block)
		require.NoError(t, err, "reserved type %d", rt)
		require.Equal(t, frame, encrypted, "reserved type %d should passthrough", rt)
	}
}

func TestEncryptH265NilCipher(t *testing.T) {
	_, err := e2ee.EncryptGCMH265SampleCustomCipher(makeH265Frame(32, 19), 0, nil)
	require.ErrorIs(t, err, types.ErrBlockCipherRequired)
}

func TestDecryptH265NilCipher(t *testing.T) {
	_, err := e2ee.DecryptGCMH265SampleCustomCipher(makeH265Frame(32, 19), nil, nil)
	require.ErrorIs(t, err, types.ErrBlockCipherRequired)
}

// TestH265MixedStartCodes is the H.265 variant of TestH264MixedStartCodes.
func TestH265MixedStartCodes(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)

	// H.265 NALU header is 2 bytes; type in bits [6:1] of byte 0.
	// Type 32 = VPS_NUT, 33 = SPS_NUT, 19 = IDR_W_RADL (a VCL slice).
	vpsHeader := []byte{32 << 1, 0x01}
	spsHeader := []byte{33 << 1, 0x01}
	idrHeader := []byte{19 << 1, 0x01}

	frame := []byte{}
	frame = append(frame, 0x00, 0x00, 0x01) // 3-byte
	frame = append(frame, vpsHeader...)
	frame = append(frame, 0xAA, 0xBB)
	frame = append(frame, 0x00, 0x00, 0x00, 0x01) // 4-byte
	frame = append(frame, spsHeader...)
	frame = append(frame, 0xCC, 0xDD)
	frame = append(frame, 0x00, 0x00, 0x01) // 3-byte
	frame = append(frame, idrHeader...)
	frame = append(frame, 0x11, 0x22, 0x33, 0x44, 0x55)

	encrypted, err := e2ee.EncryptGCMH265Sample(frame, key, 0)
	require.NoError(t, err)
	require.NotEqual(t, frame, encrypted)

	// VPS/SPS and the first 2 bytes of the IDR NALU header must pass through
	// unmodified so an SFU can still parse the stream without the key.
	// Layout: 3 (start) + 2 (VPS hdr) + 2 + 4 (start) + 2 (SPS hdr) + 2 + 3 (start) + 2 (IDR hdr) = 20.
	const unencryptedEnd = 20
	require.Equal(t, frame[:unencryptedEnd], encrypted[:unencryptedEnd],
		"VPS/SPS and IDR NALU header must remain cleartext")

	decrypted, err := e2ee.DecryptGCMH265Sample(encrypted, key, nil)
	require.NoError(t, err)
	require.Equal(t, frame, decrypted, "mixed start codes must round-trip byte-exact")
}

func BenchmarkEncryptH265CachedCipher(b *testing.B) {
	key := pbkdf2Key16(videoTestPassphrase)
	block, _ := aes.NewCipher(key)
	frame := makeH265Frame(32, 33, 34, 19)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e2ee.EncryptGCMH265SampleCustomCipher(frame, 0, block)
	}
}
