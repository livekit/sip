package e2ee_test

import (
	"crypto/aes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

const videoTestPassphrase = "12345"

// deriveAESKey matches lksdk.DeriveKeyFromString for tests that don't want to
// import the lksdk package (avoids pulling CGO-laden media-sdk deps).
func deriveAESKey(t *testing.T, passphrase string) []byte {
	t.Helper()
	return pbkdf2Key16(passphrase)
}

// makeH264Frame builds a minimal H.264 Annex-B frame with the given NALU types.
func makeH264Frame(naluTypes ...byte) []byte {
	var frame []byte
	for _, t := range naluTypes {
		frame = append(frame, 0x00, 0x00, 0x00, 0x01) // 4-byte start code
		frame = append(frame, t)                      // NALU header (1 byte)
		frame = append(frame, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22)
	}
	return frame
}

func TestEncryptH264Sample(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)

	// Frame with SPS (7) + PPS (8) + IDR slice (5).
	frame := makeH264Frame(7, 8, 5)

	encrypted, err := e2ee.EncryptGCMH264Sample(frame, key, 0)
	require.NoError(t, err)
	require.NotEqual(t, frame, encrypted)

	// Round-trip decrypt.
	decrypted, err := e2ee.DecryptGCMH264Sample(encrypted, key, nil)
	require.NoError(t, err)
	require.Equal(t, frame, decrypted)
}

func TestH264SpsOnlyPassthrough(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)

	// Frame with only SPS (7) + PPS (8) — no slice.
	frame := makeH264Frame(7, 8)

	encrypted, err := e2ee.EncryptGCMH264Sample(frame, key, 0)
	require.NoError(t, err)
	// Should pass through unmodified.
	require.Equal(t, frame, encrypted)
}

func TestEncryptH264CustomCipher(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	frame := makeH264Frame(7, 1) // SPS + non-IDR slice

	encrypted, err := e2ee.EncryptGCMH264SampleCustomCipher(frame, 0, block)
	require.NoError(t, err)

	decrypted, err := e2ee.DecryptGCMH264SampleCustomCipher(encrypted, nil, block)
	require.NoError(t, err)
	require.Equal(t, frame, decrypted)
}

func TestEncryptH264NilCipher(t *testing.T) {
	_, err := e2ee.EncryptGCMH264SampleCustomCipher(makeH264Frame(7, 5), 0, nil)
	require.ErrorIs(t, err, types.ErrBlockCipherRequired)
}

func TestDecryptH264NilCipher(t *testing.T) {
	_, err := e2ee.DecryptGCMH264SampleCustomCipher(makeH264Frame(7, 5), nil, nil)
	require.ErrorIs(t, err, types.ErrBlockCipherRequired)
}

func TestDecryptH264TooShort(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)
	block, _ := aes.NewCipher(key)

	// Samples shorter than 4 bytes pass through.
	short := []byte{0x01, 0x02}
	result, err := e2ee.DecryptGCMH264SampleCustomCipher(short, nil, block)
	require.NoError(t, err)
	require.Equal(t, short, result)
}

// TestH264MixedStartCodes verifies frames containing a mix of 3-byte
// (00 00 01) and 4-byte (00 00 00 01) Annex-B start codes round-trip correctly.
// FFmpeg on Linux is known to emit mixed start codes; findNALUIndices tolerates
// both forms (no normalization pass needed).
func TestH264MixedStartCodes(t *testing.T) {
	key := deriveAESKey(t, videoTestPassphrase)

	frame := []byte{
		0x00, 0x00, 0x01, 7, 0xAA, 0xBB, 0xCC, // 3-byte + SPS
		0x00, 0x00, 0x00, 0x01, 8, 0xDD, 0xEE, // 4-byte + PPS
		0x00, 0x00, 0x01, 5, 0x11, 0x22, 0x33, 0x44, 0x55, // 3-byte + IDR slice
	}

	encrypted, err := e2ee.EncryptGCMH264Sample(frame, key, 0)
	require.NoError(t, err)
	require.NotEqual(t, frame, encrypted)

	// SPS/PPS and the first 2 bytes of the IDR NALU (1-byte header + 1 byte)
	// must pass through unmodified so an SFU can still parse the stream.
	// Layout: 3 (start) + 1 (SPS) + 3 + 4 (start) + 1 (PPS) + 2 + 3 (start) + 2 (IDR hdr+1) = 19.
	const unencryptedEnd = 19
	require.Equal(t, frame[:unencryptedEnd], encrypted[:unencryptedEnd],
		"SPS/PPS and IDR NALU prefix must remain cleartext")

	decrypted, err := e2ee.DecryptGCMH264Sample(encrypted, key, nil)
	require.NoError(t, err)
	require.Equal(t, frame, decrypted, "mixed start codes must round-trip byte-exact")
}

func BenchmarkEncryptH264CachedCipher(b *testing.B) {
	key := pbkdf2Key16(videoTestPassphrase)
	block, _ := aes.NewCipher(key)
	frame := makeH264Frame(7, 8, 5)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e2ee.EncryptGCMH264SampleCustomCipher(frame, 0, block)
	}
}
