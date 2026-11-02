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
	"fmt"
	"io"
	"math/big"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
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

// testSigner is a key the "testkey" URI scheme opens: it signs with a local key, counts the
// signatures, can be limited to one scheme like a KMS key, and records whether it was closed.
type testSigner struct {
	crypto.Signer
	schemes []tls.SignatureScheme
	signs   atomic.Int32
	closed  atomic.Bool
}

func (s *testSigner) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	s.signs.Add(1)
	return s.Signer.Sign(r, digest, opts)
}

func (s *testSigner) SignatureSchemes() []tls.SignatureScheme { return s.schemes }

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

// writeServerCert writes a self-signed certificate for key and returns its path and a pool
// trusting it.
func writeServerCert(t *testing.T, key crypto.Signer) (string, *x509.CertPool) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sip.example.com"},
		DNSNames:              []string{"sip.example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "cert.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return path, pool
}

// newTLSService builds a Service on random test ports with the given TLS config; tconf.Port
// and ListenPort are filled in. Nothing is started.
func newTLSService(t *testing.T, tconf *config.TLSConfig) *Service {
	t.Helper()
	sipPort := mrand.Intn(testPortSIPMax-testPortSIPMin-1) + testPortSIPMin
	tconf.Port, tconf.ListenPort = sipPort+1, sipPort+1
	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)
	s, err := NewService("", &config.Config{
		SIPPort:       sipPort,
		SIPPortListen: sipPort,
		RTPPort:       rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
		TLS:           tconf,
	}, mon, logger.NewTestLogger(t), func(string, *rpc.SIPCallObservability, *livekit.SIPCallInfo) StateHandler {
		return NewRPCStateHandler(nil)
	})
	require.NoError(t, err)
	return s
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
	testSigners.Store("testkey:later-fails", signer)
	certFile, _ := writeServerCert(t, key)

	s := newTLSService(t, &config.TLSConfig{Certs: []config.TLSCert{
		{CertFile: certFile, KeyURI: "testkey:later-fails"},
		{CertFile: filepath.Join(t.TempDir(), "missing.pem"), KeyFile: "missing.pem"},
	}})
	t.Cleanup(s.Stop)

	require.ErrorIs(t, s.Start(), os.ErrNotExist)
	require.True(t, signer.closed.Load(), "signer left open after a failed Start")
	require.Empty(t, s.closers)
}

// TestServiceStartTLS runs the service with a certificate whose key is external and limited to
// PKCS#1, the Cloud KMS case: Start refuses it while TLS 1.3 is enabled, and with the cap the
// TLS listener serves a handshake signed by that key.
func TestServiceStartTLS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	certFile, pool := writeServerCert(t, key)
	pkcs1 := func(uri string) *testSigner {
		signer := &testSigner{Signer: key, schemes: []tls.SignatureScheme{tls.PKCS1WithSHA256}}
		testSigners.Store(uri, signer)
		return signer
	}

	t.Run("PKCS#1 key with TLS 1.3 enabled", func(t *testing.T) {
		signer := pkcs1("testkey:uncapped")
		s := newTLSService(t, &config.TLSConfig{Certs: []config.TLSCert{{CertFile: certFile, KeyURI: "testkey:uncapped"}}})
		t.Cleanup(s.Stop)

		require.ErrorContains(t, s.Start(), "set tls.max_version: tls1.2")
		require.True(t, signer.closed.Load(), "signer left open after a failed Start")
	})

	t.Run("serves TLS 1.2 signed by the external key", func(t *testing.T) {
		signer := pkcs1("testkey:capped")
		keyLog := filepath.Join(t.TempDir(), "keys.log")
		tconf := &config.TLSConfig{MaxVersion: "tls1.2", KeyLog: keyLog, Certs: []config.TLSCert{{CertFile: certFile, KeyURI: "testkey:capped"}}}
		s := newTLSService(t, tconf)
		stop := sync.OnceFunc(s.Stop)
		t.Cleanup(stop)
		require.NoError(t, s.Start())

		localIP, err := config.GetLocalIP()
		require.NoError(t, err)
		conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", localIP, tconf.ListenPort), &tls.Config{ServerName: "sip.example.com", RootCAs: pool})
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), conn.ConnectionState().Version)
		require.NoError(t, conn.Close())
		require.EqualValues(t, 1, signer.signs.Load())
		require.False(t, signer.closed.Load())

		stop()
		require.True(t, signer.closed.Load(), "Stop must close the signer")
		logged, err := os.ReadFile(keyLog)
		require.NoError(t, err)
		require.Contains(t, string(logged), "CLIENT_RANDOM ")
	})
}

// TestKeyLogFile: closing the key log stops its sync loop instead of leaving it syncing a
// closed file every 30s.
func TestKeyLogFile(t *testing.T) {
	k, err := openKeyLog(filepath.Join(t.TempDir(), "keys.log"))
	require.NoError(t, err)
	exited := make(chan struct{})
	go func() {
		k.syncEvery(time.Millisecond)
		close(exited)
	}()
	time.Sleep(5 * time.Millisecond)

	require.NoError(t, k.Close())
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("sync loop still running after Close")
	}
	_, err = k.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrClosed)
}
