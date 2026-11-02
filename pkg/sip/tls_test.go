package sip

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/pkg/tlskey"
)

func TestTLSALPNProtocols(t *testing.T) {
	t.Run("nil returns default sip", func(t *testing.T) {
		protos := tlsALPNProtocols(nil)
		require.Equal(t, []string{"sip"}, protos)
	})

	t.Run("empty slice disables ALPN", func(t *testing.T) {
		empty := []string{}
		protos := tlsALPNProtocols(empty)
		require.Empty(t, protos)
	})

	t.Run("custom protocols", func(t *testing.T) {
		custom := []string{"h2", "http/1.1"}
		protos := tlsALPNProtocols(custom)
		require.Equal(t, []string{"h2", "http/1.1"}, protos)
	})

	t.Run("single custom protocol", func(t *testing.T) {
		custom := []string{"sip"}
		protos := tlsALPNProtocols(custom)
		require.Equal(t, []string{"sip"}, protos)
	})
}

// testSigner is a key the "testkey" URI scheme opens; it records whether it was closed.
type testSigner struct {
	crypto.Signer
	closed atomic.Bool
}

func (s *testSigner) Close() error {
	s.closed.Store(true)
	return nil
}

var testSigners sync.Map

func init() {
	tlskey.Register("testkey", func(_ context.Context, uri string) (crypto.Signer, error) {
		if s, ok := testSigners.Load(uri); ok {
			return s.(crypto.Signer), nil
		}
		return nil, errors.New("no such key")
	})
}

// writeServerCert writes a self-signed certificate for key and returns its path.
func writeServerCert(t *testing.T, key crypto.Signer) string {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sip.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "cert.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func TestCheckTLS13Hint(t *testing.T) {
	pkcs1 := tls.Certificate{SupportedSignatureAlgorithms: []tls.SignatureScheme{tls.PKCS1WithSHA256}}
	for name, tc := range map[string]struct {
		tconf   config.TLSConfig
		conf    *tls.Config
		wantErr string
	}{
		"capped":           {config.TLSConfig{MaxVersion: "tls1.2"}, &tls.Config{Certificates: []tls.Certificate{pkcs1}, MaxVersion: tls.VersionTLS12}, ""},
		"not capped":       {config.TLSConfig{}, &tls.Config{Certificates: []tls.Certificate{pkcs1}}, "set tls.max_version: tls1.2"},
		"warn-only policy": {config.TLSConfig{MaxVersion: "tls1.2", WarnOnly: true}, &tls.Config{Certificates: []tls.Certificate{pkcs1}}, "unset tls.warn_only"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkTLS13(&tc.tconf, tc.conf)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

// TestServiceStartClosesSigners: a signer opened for one certificate is closed when Start fails
// on a later one, and Stop does not close it again.
func TestServiceStartClosesSigners(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer := &testSigner{Signer: key}
	testSigners.Store("testkey:service", signer)

	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)
	s, err := NewService("", &config.Config{
		SIPPort:       testPortSIPMin,
		SIPPortListen: testPortSIPMin,
		TLS: &config.TLSConfig{
			Port:       testPortSIPMin + 1,
			ListenPort: testPortSIPMin + 1,
			Certs: []config.TLSCert{
				{CertFile: writeServerCert(t, key), KeyURI: "testkey:service"},
				{CertFile: filepath.Join(t.TempDir(), "missing.pem"), KeyFile: "missing.pem"},
			},
		},
	}, mon, logger.NewTestLogger(t), func(string, *rpc.SIPCallObservability, *livekit.SIPCallInfo) StateHandler {
		return NewRPCStateHandler(nil)
	})
	require.NoError(t, err)
	t.Cleanup(s.Stop)

	require.ErrorIs(t, s.Start(), os.ErrNotExist)
	require.True(t, signer.closed.Load(), "signer left open after a failed Start")
	require.Empty(t, s.closers)
}
