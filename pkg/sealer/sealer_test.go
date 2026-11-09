// Copyright 2026 LiveKit, Inc.
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

package sealer_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/sealer"
)

var _ sealer.Sealer = (*sealer.AESGCM)(nil)

func newAESGCM(t testing.TB, keys ...string) *sealer.AESGCM {
	t.Helper()
	s, err := sealer.NewAESGCM(keys)
	require.NoError(t, err)
	return s
}

func seal(t testing.TB, s sealer.Sealer, plain, aad []byte) []byte {
	t.Helper()
	out, err := s.Seal(nil, plain, aad)
	require.NoError(t, err)
	return out
}

func requireInvalid(t testing.TB, s sealer.Sealer, sealed, aad []byte) {
	t.Helper()
	out, err := s.Open(nil, sealed, aad)
	require.ErrorIs(t, err, sealer.ErrInvalid)
	require.Nil(t, out)
}

// sealerFactory returns Sealers that share keys when given the same keys, as separate servers would.
type sealerFactory func(t testing.TB, keys ...string) sealer.Sealer

// testSealerContract checks the behavior the Sealer interface documents, for any implementation.
func testSealerContract(t *testing.T, newSealer sealerFactory) {
	plains := map[string][]byte{
		"nil":   nil,
		"empty": {},
		"one":   {0x42},
		"short": []byte("hello"),
		"large": bytes.Repeat([]byte("0123456789"), 1000),
	}
	aads := map[string][]byte{
		"nil":  nil,
		"text": []byte("call-id@example.com"),
		"nul":  {0, 1, 0},
	}

	t.Run("round trip", func(t *testing.T) {
		s := newSealer(t, "k")
		for pn, plain := range plains {
			for an, aad := range aads {
				t.Run(pn+"/"+an, func(t *testing.T) {
					sealed := seal(t, s, plain, aad)
					require.Len(t, sealed, len(plain)+s.Overhead())
					out, err := s.Open(nil, sealed, aad)
					require.NoError(t, err)
					require.Equal(t, len(plain), len(out))
					require.True(t, bytes.Equal(plain, out))
				})
			}
		}
	})

	t.Run("opens on another instance with the same key", func(t *testing.T) {
		sealed := seal(t, newSealer(t, "k"), []byte("p"), []byte("a"))
		out, err := newSealer(t, "k").Open(nil, sealed, []byte("a"))
		require.NoError(t, err)
		require.Equal(t, []byte("p"), out)
	})

	t.Run("appends to dst", func(t *testing.T) {
		s := newSealer(t, "k")
		prefix := []byte("prefix")
		sealed, err := s.Seal(bytes.Clone(prefix), []byte("p"), nil)
		require.NoError(t, err)
		require.Equal(t, prefix, sealed[:len(prefix)])

		// The sealed value is what follows the prefix.
		out, err := s.Open(bytes.Clone(prefix), sealed[len(prefix):], nil)
		require.NoError(t, err)
		require.Equal(t, []byte("prefixp"), out)

		// Spare capacity in dst is used without disturbing its contents.
		dst := make([]byte, 2, 256)
		dst[0], dst[1] = 'x', 'y'
		sealed, err = s.Seal(dst, []byte("p"), nil)
		require.NoError(t, err)
		require.Equal(t, []byte("xy"), sealed[:2])
		out, err = s.Open(nil, sealed[2:], nil)
		require.NoError(t, err)
		require.Equal(t, []byte("p"), out)
	})

	t.Run("does not modify inputs", func(t *testing.T) {
		s := newSealer(t, "k")
		plain, aad := []byte("plain"), []byte("aad")
		sealed := seal(t, s, plain, aad)
		require.Equal(t, []byte("plain"), plain)
		require.Equal(t, []byte("aad"), aad)
		orig := bytes.Clone(sealed)
		_, err := s.Open(nil, sealed, aad)
		require.NoError(t, err)
		require.Equal(t, orig, sealed)
	})

	t.Run("sealing is randomized", func(t *testing.T) {
		s := newSealer(t, "k")
		seen := make(map[string]bool)
		for range 100 {
			sealed := seal(t, s, []byte("same"), []byte("same"))
			require.False(t, seen[string(sealed)], "repeated sealed value")
			seen[string(sealed)] = true
		}
	})

	t.Run("hides the plaintext", func(t *testing.T) {
		plain := []byte("a recognizable plaintext")
		sealed := seal(t, newSealer(t, "k"), plain, nil)
		require.False(t, bytes.Contains(sealed, plain))
	})

	t.Run("rejects other aad", func(t *testing.T) {
		s := newSealer(t, "k")
		sealed := seal(t, s, []byte("p"), []byte("call-1"))
		requireInvalid(t, s, sealed, []byte("call-2"))
		requireInvalid(t, s, sealed, []byte("call-1\x00"))
		requireInvalid(t, s, sealed, nil)
	})

	t.Run("rejects other key", func(t *testing.T) {
		sealed := seal(t, newSealer(t, "k"), []byte("p"), nil)
		requireInvalid(t, newSealer(t, "other"), sealed, nil)
	})

	t.Run("rejects any modified byte", func(t *testing.T) {
		s := newSealer(t, "k")
		sealed := seal(t, s, []byte("plaintext"), []byte("aad"))
		for i := range sealed {
			for _, bit := range []byte{0x01, 0x80} {
				m := bytes.Clone(sealed)
				m[i] ^= bit
				requireInvalid(t, s, m, []byte("aad"))
			}
		}
	})

	t.Run("rejects truncated and extended", func(t *testing.T) {
		s := newSealer(t, "k")
		sealed := seal(t, s, []byte("plaintext"), nil)
		for n := range len(sealed) {
			requireInvalid(t, s, sealed[:n], nil)
			requireInvalid(t, s, sealed[n+1:], nil)
		}
		requireInvalid(t, s, append(bytes.Clone(sealed), 0), nil)
		requireInvalid(t, s, nil, nil)
	})

	t.Run("concurrent use", func(t *testing.T) {
		s := newSealer(t, "k")
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				aad := []byte{byte(i)}
				for range 200 {
					sealed, err := s.Seal(nil, []byte("p"), aad)
					if !assertNoError(t, err) {
						return
					}
					out, err := s.Open(nil, sealed, aad)
					if !assertNoError(t, err) || !bytes.Equal(out, []byte("p")) {
						t.Errorf("round trip failed: %q", out)
						return
					}
				}
			})
		}
		wg.Wait()
	})
}

func assertNoError(t testing.TB, err error) bool {
	if err != nil {
		t.Error(err)
		return false
	}
	return true
}

func TestAESGCMContract(t *testing.T) {
	testSealerContract(t, func(t testing.TB, keys ...string) sealer.Sealer {
		return newAESGCM(t, keys...)
	})
}

func TestNewAESGCMRejectsBadKeys(t *testing.T) {
	for name, keys := range map[string][]string{
		"nil":          nil,
		"empty":        {},
		"empty key":    {""},
		"empty second": {"k", ""},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := sealer.NewAESGCM(keys)
			require.Error(t, err)
			require.Nil(t, s)
		})
	}
}

func TestAESGCMOverhead(t *testing.T) {
	require.Equal(t, 12+16, newAESGCM(t, "k").Overhead())
}

func TestAESGCMRotation(t *testing.T) {
	plain, aad := []byte("p"), []byte("a")
	oldOnly := newAESGCM(t, "old")
	newOnly := newAESGCM(t, "new")
	added := newAESGCM(t, "old", "new")    // step 1: new key accepted
	promoted := newAESGCM(t, "new", "old") // step 2: new key seals

	opens := func(s sealer.Sealer, sealed []byte) bool {
		_, err := s.Open(nil, sealed, aad)
		return err == nil
	}

	// Every step of the rotation opens what the previous and next steps seal.
	steps := []sealer.Sealer{oldOnly, added, promoted, newOnly}
	for i := range steps {
		for j := max(i-1, 0); j <= min(i+1, len(steps)-1); j++ {
			require.True(t, opens(steps[j], seal(t, steps[i], plain, aad)), "step %d sealed, step %d opened", i, j)
		}
	}

	// Seal uses the first key only.
	require.True(t, opens(oldOnly, seal(t, added, plain, aad)))
	require.False(t, opens(newOnly, seal(t, added, plain, aad)))
	require.True(t, opens(newOnly, seal(t, promoted, plain, aad)))
	require.False(t, opens(oldOnly, seal(t, promoted, plain, aad)))

	// Dropping a key stops it from opening.
	requireInvalid(t, newOnly, seal(t, oldOnly, plain, aad), aad)
}

// The wire format is a contract with values already sealed and with other implementations,
// e.g. servers on an older version during a rollout: nonce | ciphertext | tag,
// AES-256-GCM keyed with SHA-256 of the key string.
const (
	goldenKey   = "golden-key"
	goldenPlain = "golden plaintext"
	goldenAAD   = "golden aad"
	// Sealed by plain crypto/cipher with nonce "0123456789ab".
	goldenSealed = "303132333435363738396162eef5c33ee9bdb039a7a1c0831a70131bf4227836b0a6151104ee33f45dfab4a1"
)

func goldenGCM(t testing.TB) cipher.AEAD {
	k := sha256.Sum256([]byte(goldenKey))
	block, err := aes.NewCipher(k[:])
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return aead
}

func TestAESGCMWireFormatOpen(t *testing.T) {
	sealed, err := hex.DecodeString(goldenSealed)
	require.NoError(t, err)
	out, err := newAESGCM(t, goldenKey).Open(nil, sealed, []byte(goldenAAD))
	require.NoError(t, err)
	require.Equal(t, goldenPlain, string(out))
}

func TestAESGCMWireFormatSeal(t *testing.T) {
	sealed := seal(t, newAESGCM(t, goldenKey), []byte(goldenPlain), []byte(goldenAAD))
	aead := goldenGCM(t)
	ns := aead.NonceSize()
	out, err := aead.Open(nil, sealed[:ns], sealed[ns:], []byte(goldenAAD))
	require.NoError(t, err)
	require.Equal(t, goldenPlain, string(out))
}
