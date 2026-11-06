// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

func TestParseCipherSuites(t *testing.T) {
	log := logger.NewTestLogger(t)

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

func TestApplyPolicy(t *testing.T) {
	conf := TLSConfig{
		MinVersion:   "tls1.2",
		CipherSuites: []string{"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384"},
	}

	t.Run("enforced", func(t *testing.T) {
		c := &tls.Config{}
		require.NoError(t, conf.ApplyPolicy(logger.NewTestLogger(t), c))
		require.Equal(t, []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384}, c.CipherSuites)
		require.Equal(t, uint16(tls.VersionTLS12), c.MinVersion)
		require.Zero(t, c.MaxVersion)
		require.Nil(t, c.VerifyConnection)
	})

	t.Run("warn only not configured", func(t *testing.T) {
		c := &tls.Config{}
		require.NoError(t, (&TLSConfig{WarnOnly: true}).ApplyPolicy(logger.NewTestLogger(t), c))
		require.Nil(t, c.VerifyConnection)
	})

	t.Run("invalid", func(t *testing.T) {
		require.Error(t, (&TLSConfig{CipherSuites: []string{"TLS_NOPE"}}).ApplyPolicy(logger.NewTestLogger(t), &tls.Config{}))
		require.Error(t, (&TLSConfig{MinVersion: "ssl3", WarnOnly: true}).ApplyPolicy(logger.NewTestLogger(t), &tls.Config{}))
	})

	warnOnly := conf
	warnOnly.WarnOnly = true
	cases := []struct {
		name   string
		client *tls.Config
		warn   string // expected cipher suite in the warning, if any
	}{
		{"allowed suite", &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384}}, ""},
		{"tls1.3", &tls.Config{}, ""},
		// Go prefers AES-128-GCM, which is a modern suite, but not in this policy.
		{"suite not in policy", &tls.Config{MaxVersion: tls.VersionTLS12}, "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"},
		{"disallowed suite", &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA}}, "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA"},
	}
	for _, tc := range cases {
		t.Run("warn only "+tc.name, func(t *testing.T) {
			logs := &logCapture{}
			c := &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
			require.NoError(t, warnOnly.ApplyPolicy(logger.NewTestLogger(logs), c))
			// Go's defaults are kept, so the handshake succeeds either way.
			require.Nil(t, c.CipherSuites)
			require.Zero(t, c.MinVersion)
			tc.client.ServerName = "abc123.sip.example.com"
			require.NoError(t, tlsHandshake(t, c, tc.client))
			warning := logs.find("TLS connection outside of the configured policy")
			if tc.warn == "" {
				require.Empty(t, warning)
				return
			}
			require.Contains(t, warning, `"cipherSuite"="`+tc.warn+`"`)
			require.Contains(t, warning, `"version"="TLS 1.2"`)
			require.Contains(t, warning, `"serverName"="abc123.sip.example.com"`)
		})
	}
}

// TestApplyPolicyOutbound checks that the warning is also logged when we are the client.
func TestApplyPolicyOutbound(t *testing.T) {
	conf := TLSConfig{
		MinVersion:   "tls1.2",
		CipherSuites: []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"},
		WarnOnly:     true,
	}
	logs := &logCapture{}
	cli := &tls.Config{ServerName: "pbx.example.com"}
	require.NoError(t, conf.ApplyPolicy(logger.NewTestLogger(logs), cli))

	srv := &tls.Config{
		Certificates: []tls.Certificate{selfSignedCert(t)},
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA},
	}
	require.NoError(t, tlsHandshake(t, srv, cli))
	warning := logs.find("TLS connection outside of the configured policy")
	require.Contains(t, warning, `"cipherSuite"="TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA"`)
	require.Contains(t, warning, `"serverName"="pbx.example.com"`)
}

func TestTLSPolicyAllows(t *testing.T) {
	gcm := uint16(tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
	cbc := uint16(tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA)
	cases := []struct {
		name     string
		version  uint16
		suite    uint16
		suites   []uint16
		min, max uint16
		allowed  bool
	}{
		{"allowed suite", tls.VersionTLS12, gcm, []uint16{gcm}, tls.VersionTLS12, 0, true},
		{"other suite", tls.VersionTLS12, cbc, []uint16{gcm}, tls.VersionTLS12, 0, false},
		{"tls1.3 ignores suites", tls.VersionTLS13, tls.TLS_AES_128_GCM_SHA256, []uint16{gcm}, tls.VersionTLS12, 0, true},
		{"above max", tls.VersionTLS13, tls.TLS_AES_128_GCM_SHA256, nil, 0, tls.VersionTLS12, false},
		{"below min", tls.VersionTLS11, gcm, []uint16{gcm}, tls.VersionTLS12, 0, false},
		{"no suite list", tls.VersionTLS12, cbc, nil, tls.VersionTLS12, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := tls.ConnectionState{Version: c.version, CipherSuite: c.suite}
			require.Equal(t, c.allowed, tlsPolicyAllows(cs, c.suites, c.min, c.max))
		})
	}
}

// tlsHandshake runs a TLS handshake over loopback TCP.
func tlsHandshake(t *testing.T, srv, cli *tls.Config) error {
	lis, err := tls.Listen("tcp4", "127.0.0.1:0", srv)
	require.NoError(t, err)
	defer lis.Close()
	go func() {
		c, err := lis.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.(*tls.Conn).Handshake()
	}()
	cli.InsecureSkipVerify = true
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp4", lis.Addr().String(), cli)
	if err != nil {
		return err
	}
	return conn.Close()
}

// logCapture collects log lines from logger.NewTestLogger.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) Logf(format string, args ...any) { l.add(fmt.Sprintf(format, args...)) }
func (l *logCapture) Log(args ...any)                 { l.add(fmt.Sprint(args...)) }
func (l *logCapture) Cleanup(func())                  {}

func (l *logCapture) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}

// find returns the first log line containing sub, or "" if there is none.
func (l *logCapture) find(sub string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return s
		}
	}
	return ""
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sip-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
