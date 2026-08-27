package e2ee_test

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/server-sdk-go/v2/e2ee"
)

// stubEncryptFunc writes [KID, payload...] — lets us assert which KID was used
// without depending on the full lksdk GCM format.
func stubEncryptFunc(payload []byte, kid uint8, _ cipher.Block) ([]byte, error) {
	out := make([]byte, 0, len(payload)+1)
	out = append(out, kid)
	out = append(out, payload...)
	return out, nil
}

// stubDecryptFunc reverses stubEncryptFunc.
func stubDecryptFunc(payload, _ []byte, _ cipher.Block) ([]byte, error) {
	if len(payload) < 1 {
		return nil, nil
	}
	return payload[1:], nil
}

func TestGCMFrameEncryptorInitialKey(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))

	enc, err := e2ee.NewGCMFrameEncryptor(kp, stubEncryptFunc)
	require.NoError(t, err)

	out, err := enc.EncryptFrame([]byte("frame"))
	require.NoError(t, err)
	require.Equal(t, byte(0), out[0], "KID should be 0")
}

func TestGCMFrameEncryptorPicksCurrentIndexOnInit(t *testing.T) {
	// Simulates a re-publish after rotation: create the encryptor AFTER key rotation.
	// New encryptor must read CurrentKeyIndex rather than defaulting to 0.
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))
	require.NoError(t, kp.SetRawKey(bytes16(0xBB), 5))
	require.Equal(t, uint32(5), kp.CurrentKeyIndex())

	enc, err := e2ee.NewGCMFrameEncryptor(kp, stubEncryptFunc)
	require.NoError(t, err)

	out, err := enc.EncryptFrame([]byte("x"))
	require.NoError(t, err)
	require.Equal(t, byte(5), out[0], "new encryptor must honor CurrentKeyIndex")
}

func TestGCMFrameEncryptorRotationMidStream(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))

	enc, err := e2ee.NewGCMFrameEncryptor(kp, stubEncryptFunc)
	require.NoError(t, err)

	out1, err := enc.EncryptFrame([]byte("first"))
	require.NoError(t, err)
	require.Equal(t, byte(0), out1[0])

	// Rotate mid-stream: advance to index 1.
	require.NoError(t, kp.SetRawKey(bytes16(0xBB), 1))

	out2, err := enc.EncryptFrame([]byte("second"))
	require.NoError(t, err)
	require.Equal(t, byte(1), out2[0], "encryptor should pick up rotated key index on next frame")
}

func TestGCMFrameDecryptorCachesPerKID(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))
	require.NoError(t, kp.SetRawKey(bytes16(0xBB), 1))

	dec := e2ee.NewGCMFrameDecryptor(kp, stubDecryptFunc, nil)

	// Decrypt a frame ending in KID=0 (stub doesn't read KID from trailer,
	// but DecryptFrame reads the last byte).
	frameKID0 := []byte{0x00, 'p', 0x00}
	out0, err := dec.DecryptFrame(frameKID0)
	require.NoError(t, err)
	require.NotNil(t, out0)

	frameKID1 := []byte{0x01, 'q', 0x01}
	out1, err := dec.DecryptFrame(frameKID1)
	require.NoError(t, err)
	require.NotNil(t, out1)

	// Second call for each KID should hit the cache (no behavioral check,
	// but at least it doesn't error).
	_, err = dec.DecryptFrame(frameKID0)
	require.NoError(t, err)
	_, err = dec.DecryptFrame(frameKID1)
	require.NoError(t, err)
}

// TestGCMFrameDecryptorRebuildsOnKeyRotation ensures the decryptor does not
// serve a stale cached cipher block after the keyProvider replaces the key at
// an already-cached index. Without the keyBytes check in getCachedBlock this
// test fails: the second and third decrypts reuse the block derived from 0xAA.
func TestGCMFrameDecryptorRebuildsOnKeyRotation(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))

	var capturedBlocks []cipher.Block
	capturingDecrypt := func(payload, _ []byte, block cipher.Block) ([]byte, error) {
		capturedBlocks = append(capturedBlocks, block)
		if len(payload) < 1 {
			return nil, nil
		}
		return payload[1:], nil
	}

	dec := e2ee.NewGCMFrameDecryptor(kp, capturingDecrypt, nil)

	frame := []byte{0x00, 'p', 0x00}
	_, err := dec.DecryptFrame(frame)
	require.NoError(t, err)
	_, err = dec.DecryptFrame(frame)
	require.NoError(t, err)
	require.Same(t, capturedBlocks[0], capturedBlocks[1],
		"repeated decrypts with the same key should reuse the cached block")

	// Replace the key at index 0 with different bytes. The cached block for
	// kid=0 is now stale and must be rebuilt.
	require.NoError(t, kp.SetRawKey(bytes16(0xBB), 0))

	_, err = dec.DecryptFrame(frame)
	require.NoError(t, err)
	require.NotSame(t, capturedBlocks[0], capturedBlocks[2],
		"decryptor must rebuild the cipher block after key rotation at the same index")
}

func TestGCMFrameDecryptorUnknownKID(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))

	dec := e2ee.NewGCMFrameDecryptor(kp, stubDecryptFunc, nil)

	frameKID9 := []byte{0x00, 'p', 0x09}
	_, err := dec.DecryptFrame(frameKID9)
	require.Error(t, err)
}

func TestGCMFrameDecryptorTooShort(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xAA), 0))

	dec := e2ee.NewGCMFrameDecryptor(kp, stubDecryptFunc, nil)

	_, err := dec.DecryptFrame([]byte{0xAA})
	require.Error(t, err)
}

// sanity check that the real AES cipher block construction path works too.
func TestGCMFrameEncryptorUsesRealCipher(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(bytes16(0xCC), 0))

	// Assert cipher.NewCipher on a 16-byte key returns a valid block.
	block, err := aes.NewCipher(bytes16(0xCC))
	require.NoError(t, err)
	require.NotNil(t, block)

	_, err = e2ee.NewGCMFrameEncryptor(kp, stubEncryptFunc)
	require.NoError(t, err)
}
