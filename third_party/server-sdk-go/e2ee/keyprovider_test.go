package e2ee_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

func TestPassphraseDerivationMatchesJS(t *testing.T) {
	// Derived key must match the JS SDK's PBKDF2 parameters
	// (salt="LKFrameEncryptionKey", SHA-256, 100000 iterations, 128-bit output).
	// Expected bytes are the same as the legacy DeriveKeyFromString("12345") test.
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetKeyFromPassphrase("12345", 0))

	key, err := kp.GetKey(0)
	require.NoError(t, err)
	require.Equal(t, []byte{15, 94, 198, 66, 93, 211, 116, 46, 55, 97, 232, 121, 189, 233, 224, 22}, key)
}

func TestPassphraseEmptyRejected(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.Error(t, kp.SetKeyFromPassphrase("", 0))
}

func TestSetRawKeyLengthValidation(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()

	// Wrong length: 3 bytes.
	err := kp.SetRawKey([]byte{1, 2, 3}, 0)
	require.ErrorIs(t, err, types.ErrIncorrectKeyLength)

	// Correct length: 16 bytes.
	require.NoError(t, kp.SetRawKey(make([]byte, 16), 0))
}

func TestGetKeyUnknownIndex(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.NoError(t, kp.SetRawKey(make([]byte, 16), 0))

	_, err := kp.GetKey(42)
	require.Error(t, err)
}

func TestCurrentKeyIndexAdvances(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()
	require.Equal(t, uint32(0), kp.CurrentKeyIndex())

	require.NoError(t, kp.SetRawKey(make([]byte, 16), 3))
	require.Equal(t, uint32(3), kp.CurrentKeyIndex())

	// Setting a new key at a lower index still updates CurrentKeyIndex
	// (the "current" is whatever was set most recently).
	require.NoError(t, kp.SetRawKey(make([]byte, 16), 1))
	require.Equal(t, uint32(1), kp.CurrentKeyIndex())
}

func TestMultipleKeysRetained(t *testing.T) {
	kp := e2ee.NewExternalKeyProvider()

	k0 := bytes16(0xAA)
	k1 := bytes16(0xBB)
	require.NoError(t, kp.SetRawKey(k0, 0))
	require.NoError(t, kp.SetRawKey(k1, 1))

	got0, err := kp.GetKey(0)
	require.NoError(t, err)
	require.Equal(t, k0, got0)

	got1, err := kp.GetKey(1)
	require.NoError(t, err)
	require.Equal(t, k1, got1)
}

func bytes16(b byte) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = b
	}
	return out
}
