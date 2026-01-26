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

		suites, err := ParseCipherSuites(cipherSuites, log)

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

		suites, err := ParseCipherSuites(cipherSuites, log)

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

		suites, err := ParseCipherSuites(cipherSuites, log)

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

		_, err := ParseCipherSuites(cipherSuites, log)

		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown cipher suite: INVALID_CIPHER_SUITE")
	})
}
func TestParseTLSVersion(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		version, err := ParseTLSVersion("")
		require.NoError(t, err)
		require.Equal(t, uint16(0), version)
	})

	t.Run("TLS 1.0 - short format", func(t *testing.T) {
		version, err := ParseTLSVersion("1.0")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS10), version)
	})

	t.Run("TLS 1.0 - long format", func(t *testing.T) {
		version, err := ParseTLSVersion("TLS 1.0")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS10), version)
	})

	t.Run("TLS 1.1 - short format", func(t *testing.T) {
		version, err := ParseTLSVersion("1.1")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS11), version)
	})

	t.Run("TLS 1.1 - long format", func(t *testing.T) {
		version, err := ParseTLSVersion("TLS 1.1")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS11), version)
	})

	t.Run("TLS 1.2 - short format", func(t *testing.T) {
		version, err := ParseTLSVersion("1.2")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), version)
	})

	t.Run("TLS 1.2 - long format", func(t *testing.T) {
		version, err := ParseTLSVersion("TLS 1.2")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), version)
	})

	t.Run("TLS 1.3 - short format", func(t *testing.T) {
		version, err := ParseTLSVersion("1.3")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS13), version)
	})

	t.Run("TLS 1.3 - long format", func(t *testing.T) {
		version, err := ParseTLSVersion("TLS 1.3")
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS13), version)
	})

	t.Run("invalid version", func(t *testing.T) {
		_, err := ParseTLSVersion("1.4")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown TLS version: 1.4")
	})

	t.Run("invalid format", func(t *testing.T) {
		_, err := ParseTLSVersion("TLS1.2")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown TLS version: TLS1.2")
	})
}

