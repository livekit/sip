package lksdk

import (
	"crypto/aes"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	opusEncryptedFrame = []byte{120, 145, 24, 159, 76, 65, 130, 48, 144, 249, 17, 112, 134, 78, 250, 129, 171, 194, 16, 173, 73, 196, 5, 152, 69, 225, 28, 210, 196, 241, 226, 139, 231, 172, 51, 38, 139, 179, 245, 182, 170, 8, 122, 117, 98, 144, 123, 95, 73, 89, 119, 39, 205, 20, 191, 55, 121, 59, 239, 192, 85, 224, 228, 143, 10, 113, 195, 223, 118, 42, 2, 32, 22, 17, 77, 227, 109, 160, 245, 202, 189, 63, 162, 164, 5, 241, 24, 151, 45, 42, 165, 131, 171, 243, 141, 53, 35, 131, 141, 52, 253, 188, 12, 0}
	opusDecryptedFrame = []byte{120, 11, 109, 82, 113, 132, 189, 156, 220, 173, 30, 109, 87, 54, 173, 99, 26, 126, 166, 37, 127, 234, 110, 211, 230, 152, 181, 235, 197, 19, 140, 230, 179, 35, 131, 132, 29, 192, 97, 247, 108, 53, 183, 214, 77, 181, 173, 206, 175, 7, 228, 145, 93, 155, 155, 142, 14, 27, 111, 64, 96, 196, 229, 189, 142, 59, 149, 169, 99, 225, 216, 85, 186, 182}
	opusSilenceFrame   = []byte{
		0xf8, 0xff, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	sifTrailer         = []byte{50, 86, 10, 220, 108, 185, 57, 211}
	testPassphrase     = "12345"
	keyIncorrectLength = []byte{1, 2, 3}
)

func TestDeriveKeyFromString(t *testing.T) {

	password := "12345"

	key, err := DeriveKeyFromString(password)
	expectedKey := []byte{15, 94, 198, 66, 93, 211, 116, 46, 55, 97, 232, 121, 189, 233, 224, 22}

	require.Nil(t, err)
	require.Equal(t, key, expectedKey)
}

func TestDeriveKeyFromBytes(t *testing.T) {

	inputSecret := []byte{34, 21, 187, 202, 134, 204, 168, 62, 5, 105, 40, 244, 88}
	expectedKey := []byte{129, 224, 93, 62, 17, 203, 99, 136, 101, 35, 149, 128, 189, 152, 251, 76}

	key, err := DeriveKeyFromBytes(inputSecret)
	require.Nil(t, err)
	require.Equal(t, expectedKey, key)

}

func TestDecryptAudioSample(t *testing.T) {

	key, err := DeriveKeyFromString(testPassphrase)
	require.Nil(t, err)

	_, err = DecryptGCMAudioSample(opusEncryptedFrame, keyIncorrectLength, sifTrailer)
	require.ErrorIs(t, err, ErrIncorrectKeyLength)

	decryptedFrame, err := DecryptGCMAudioSample(nil, key, sifTrailer)
	require.Nil(t, err)
	require.Nil(t, decryptedFrame)

	decryptedFrame, err = DecryptGCMAudioSample(opusEncryptedFrame, key, sifTrailer)

	require.Nil(t, err)
	require.Equal(t, opusDecryptedFrame, decryptedFrame)

	var sifFrame []byte
	sifFrame = append(sifFrame, opusSilenceFrame...)
	sifFrame = append(sifFrame, sifTrailer...)

	decryptedFrame, err = DecryptGCMAudioSample(sifFrame, key, sifTrailer)
	require.Nil(t, err)
	require.Nil(t, decryptedFrame)

}

func TestEncryptAudioSample(t *testing.T) {

	key, err := DeriveKeyFromString(testPassphrase)
	require.Nil(t, err)

	_, err = EncryptGCMAudioSample(opusDecryptedFrame, keyIncorrectLength, 0)
	require.ErrorIs(t, err, ErrIncorrectKeyLength)

	encryptedFrame, err := EncryptGCMAudioSample(nil, key, 0)
	require.Nil(t, err)
	require.Nil(t, encryptedFrame)

	encryptedFrame, err = EncryptGCMAudioSample(opusDecryptedFrame, key, 0)

	require.Nil(t, err)

	// IV is generated randomly so to verify we decrypt and make sure that we got the expected plain text frame
	decryptedFrame, err := DecryptGCMAudioSample(encryptedFrame, key, sifTrailer)
	require.Nil(t, err)
	require.Equal(t, opusDecryptedFrame, decryptedFrame)

}

func BenchmarkDecryptAudioNewCipher(b *testing.B) {

	key, _ := DeriveKeyFromString(testPassphrase)
	for i := 0; i < b.N; i++ {
		cipherBlock, _ := aes.NewCipher(key)
		DecryptGCMAudioSampleCustomCipher(opusEncryptedFrame, sifTrailer, cipherBlock)

	}

}

func BenchmarkDecryptAudioCachedCipher(b *testing.B) {

	key, _ := DeriveKeyFromString(testPassphrase)
	cipherBlock, _ := aes.NewCipher(key)
	for i := 0; i < b.N; i++ {
		DecryptGCMAudioSampleCustomCipher(opusEncryptedFrame, sifTrailer, cipherBlock)

	}

}

func BenchmarkEncryptAudioCachedCipher(b *testing.B) {

	key, _ := DeriveKeyFromString(testPassphrase)
	cipherBlock, _ := aes.NewCipher(key)
	for i := 0; i < b.N; i++ {
		EncryptGCMAudioSampleCustomCipher(opusDecryptedFrame, 0, cipherBlock)

	}

}

func BenchmarkEncryptAudioNewCipher(b *testing.B) {

	key, _ := DeriveKeyFromString(testPassphrase)
	for i := 0; i < b.N; i++ {
		cipherBlock, _ := aes.NewCipher(key)
		EncryptGCMAudioSampleCustomCipher(opusDecryptedFrame, 0, cipherBlock)

	}

}

func TestDeriveKeyFromStringEmpty(t *testing.T) {
	_, err := DeriveKeyFromString("")
	require.ErrorIs(t, err, ErrIncorrectSecretLength)
}
