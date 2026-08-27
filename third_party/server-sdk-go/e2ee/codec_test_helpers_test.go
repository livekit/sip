package e2ee_test

import (
	"crypto/sha256"

	"golang.org/x/crypto/pbkdf2"

	"github.com/livekit/server-sdk-go/v2/e2ee/types"
)

// pbkdf2Key16 mirrors lksdk.DeriveKeyFromString, kept local to the e2ee tests
// to avoid importing the lksdk package (which pulls in CGO media-sdk deps).
func pbkdf2Key16(passphrase string) []byte {
	return pbkdf2.Key(
		[]byte(passphrase),
		[]byte(types.SDKSalt),
		types.PBKDFIterations,
		types.KeySizeBytes,
		sha256.New,
	)
}
