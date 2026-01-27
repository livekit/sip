package sip

import (
	"crypto/tls"
	"testing"

	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

func TestParseCipherSuites(t *testing.T) {
	log := logger.GetLogger()

	t.Run("valid cipher suites - secure", func(t *testing.T) {
		cipherSuites := []string{
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		}

		suites, err := parseCipherSuites(log, cipherSuites)

		require.NoError(t, err)
		require.Equal(t, len(cipherSuites), len(suites))
		require.Equal(t, uint16(tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256), suites[0])
		require.Equal(t, uint16(tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA), suites[1])
		require.Equal(t, uint16(tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256), suites[2])
	})

	t.Run("valid cipher suites - insecure", func(t *testing.T) {
		cipherSuites := []string{
			"TLS_RSA_WITH_RC4_128_SHA",
			"TLS_RSA_WITH_3DES_EDE_CBC_SHA",
			"TLS_ECDHE_RSA_WITH_RC4_128_SHA",
		}

		suites, err := parseCipherSuites(log, cipherSuites)

		require.NoError(t, err)
		require.Equal(t, len(cipherSuites), len(suites))
		require.Equal(t, uint16(tls.TLS_RSA_WITH_RC4_128_SHA), suites[0])
		require.Equal(t, uint16(tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA), suites[1])
		require.Equal(t, uint16(tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA), suites[2])
	})

	t.Run("cipher suite - mixed", func(t *testing.T) {
		cipherSuites := []string{
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_RC4_128_SHA",
		}

		suites, err := parseCipherSuites(log, cipherSuites)

		require.NoError(t, err)
		require.Equal(t, len(cipherSuites), len(suites))
		require.Equal(t, uint16(tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256), suites[0])
		require.Equal(t, uint16(tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA), suites[1])
	})

	t.Run("invalid cipher site", func(t *testing.T) {
		cipherSuites := []string{
			"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA",
			"INVALID_CIPHER_SUITE",
		}

		_, err := parseCipherSuites(log, cipherSuites)

		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown cipher suite: INVALID_CIPHER_SUITE")
	})
}
func TestParseTLSVersion(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		version, err := parseTLSVersion("")
		require.NoError(t, err)
		require.Equal(t, uint16(0), version)
	})

	t.Run("TLS 1.0 - lowercase format", func(t *testing.T) {
		version, err := parseTLSVersion("tls1.0")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS10), version)
	})

	t.Run("TLS 1.0 - uppercase format", func(t *testing.T) {
		version, err := parseTLSVersion("TLS1.0")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS10), version)
	})

	t.Run("TLS 1.1 - lowercase format", func(t *testing.T) {
		version, err := parseTLSVersion("tls1.1")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS11), version)
	})

	t.Run("TLS 1.1 - uppercase format", func(t *testing.T) {
		version, err := parseTLSVersion("TLS1.1")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS11), version)
	})

	t.Run("TLS 1.2 - lowercase format", func(t *testing.T) {
		version, err := parseTLSVersion("tls1.2")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), version)
	})

	t.Run("TLS 1.2 - uppercase format", func(t *testing.T) {
		version, err := parseTLSVersion("TLS1.2")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), version)
	})

	t.Run("TLS 1.3 - lowercase format", func(t *testing.T) {
		version, err := parseTLSVersion("tls1.3")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS13), version)
	})

	t.Run("TLS 1.3 - uppercase format", func(t *testing.T) {
		version, err := parseTLSVersion("TLS1.3")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS13), version)
	})

	t.Run("invalid version", func(t *testing.T) {
		_, err := parseTLSVersion("tls1.4")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown TLS version: tls1.4")
	})

	t.Run("invalid format", func(t *testing.T) {
		_, err := parseTLSVersion("TLS 1.2")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown TLS version: TLS 1.2")
	})
}
