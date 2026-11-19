// Copyright 2026 LiveKit, Inc.
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

package tlskey

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
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// memKey stands in for a key held outside the process, limited to one scheme like a KMS key.
type memKey struct {
	crypto.Signer
	schemes []tls.SignatureScheme
	signs   int
	closed  bool
}

func (k *memKey) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	k.signs++
	return k.Signer.Sign(r, digest, opts)
}

func (k *memKey) SignatureSchemes() []tls.SignatureScheme { return k.schemes }

func (k *memKey) Close() error {
	k.closed = true
	return nil
}

var memKeys sync.Map

func init() {
	Register("mem", func(_ context.Context, uri string) (crypto.Signer, error) {
		if s, ok := memKeys.Load(uri); ok {
			return s.(crypto.Signer), nil
		}
		return nil, errors.New("no such key")
	})
	// A scheme whose URIs have no "//", like PKCS#11 URIs: the opener gets the URI whole.
	Register("echo", func(_ context.Context, uri string) (crypto.Signer, error) {
		return nil, errors.New("opened " + uri)
	})
}

func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func selfSigned(t *testing.T, key crypto.Signer) []byte {
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
	return der
}

func writeFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fullchain.pem")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func writePEM(t *testing.T, ders ...[]byte) string {
	t.Helper()
	var buf []byte
	for _, der := range ders {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return writeFile(t, buf)
}

func roots(t *testing.T, der []byte) *x509.CertPool {
	t.Helper()
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return pool
}

func handshake(t *testing.T, cert tls.Certificate, maxVersion uint16, roots *x509.CertPool) (tls.ConnectionState, error) {
	t.Helper()
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tls.Server(srv, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: maxVersion}).Handshake()
	}()
	c := tls.Client(cli, &tls.Config{ServerName: "sip.example.com", RootCAs: roots})
	err := c.Handshake()
	if err == nil {
		err = <-srvErr
	}
	return c.ConnectionState(), err
}

func TestRegister(t *testing.T) {
	require.Contains(t, Schemes(), "mem")
	require.PanicsWithValue(t, "tlskey: Register called twice for scheme mem", func() {
		Register("mem", func(context.Context, string) (crypto.Signer, error) { return nil, nil })
	})
	require.Panics(t, func() { Register("", func(context.Context, string) (crypto.Signer, error) { return nil, nil }) })
	require.Panics(t, func() { Register("x", nil) })
}

func TestOpen(t *testing.T) {
	ctx := context.Background()
	_, err := Open(ctx, "not-a-uri")
	require.ErrorContains(t, err, "not a URI")
	_, err = Open(ctx, "://x")
	require.ErrorContains(t, err, "not a URI")

	_, err = Open(ctx, "awskms://alias/x")
	require.ErrorContains(t, err, `no backend for key URI scheme "awskms"`)
	require.ErrorContains(t, err, "mem")

	// The scheme ends at the first ':', so a URI without "//" reaches its backend whole.
	_, err = Open(ctx, "echo:token=a;object=b")
	require.ErrorContains(t, err, "open echo key: opened echo:token=a;object=b")
}

func TestLoad(t *testing.T) {
	ctx := context.Background()
	rsaKey, other := mustRSA(t), mustRSA(t)

	t.Run("chain, key and schemes", func(t *testing.T) {
		k := &memKey{Signer: rsaKey, schemes: []tls.SignatureScheme{tls.PKCS1WithSHA256}}
		memKeys.Store("mem:chain", k)
		leaf := selfSigned(t, rsaKey)
		cert, err := Load(ctx, writePEM(t, leaf, selfSigned(t, other)), "mem:chain")
		require.NoError(t, err)
		require.Len(t, cert.Certificate, 2)
		require.Equal(t, leaf, cert.Certificate[0])
		require.Equal(t, leaf, cert.Leaf.Raw)
		require.Same(t, k, cert.PrivateKey)
		require.Equal(t, k.schemes, cert.SupportedSignatureAlgorithms)
	})

	t.Run("signer without schemes leaves the choice to TLS", func(t *testing.T) {
		memKeys.Store("mem:plain", rsaKey)
		cert, err := Load(ctx, writePEM(t, selfSigned(t, rsaKey)), "mem:plain")
		require.NoError(t, err)
		require.Same(t, rsaKey, cert.PrivateKey)
		require.Nil(t, cert.SupportedSignatureAlgorithms)
	})

	t.Run("certificate of another key", func(t *testing.T) {
		k := &memKey{Signer: rsaKey}
		memKeys.Store("mem:mismatch", k)
		_, err := Load(ctx, writePEM(t, selfSigned(t, other)), "mem:mismatch")
		require.ErrorContains(t, err, "is not the key's")
		require.True(t, k.closed, "signer left open")
	})

	t.Run("certificate file", func(t *testing.T) {
		memKeys.Store("mem:ok", rsaKey)
		_, err := Load(ctx, filepath.Join(t.TempDir(), "missing.pem"), "mem:ok")
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = Load(ctx, writeFile(t, []byte("no pem here")), "mem:ok")
		require.ErrorContains(t, err, "no certificate in")
		_, err = Load(ctx, writePEM(t, []byte("junk")), "mem:ok")
		require.ErrorContains(t, err, "x509")
	})

	t.Run("key that does not open", func(t *testing.T) {
		_, err := Load(ctx, writePEM(t, selfSigned(t, rsaKey)), "mem:none")
		require.ErrorContains(t, err, "open mem key: no such key")
		_, err = Load(ctx, writePEM(t, selfSigned(t, rsaKey)), "nope://x")
		require.ErrorContains(t, err, "no backend")
	})
}

func TestHandshake(t *testing.T) {
	ctx := context.Background()
	rsaKey := mustRSA(t)
	leaf := selfSigned(t, rsaKey)
	certFile, pool := writePEM(t, leaf), roots(t, leaf)

	t.Run("PKCS#1 key signs TLS 1.2 and cannot do TLS 1.3", func(t *testing.T) {
		k := &memKey{Signer: rsaKey, schemes: []tls.SignatureScheme{tls.PKCS1WithSHA256}}
		memKeys.Store("mem:pkcs1", k)
		cert, err := Load(ctx, certFile, "mem:pkcs1")
		require.NoError(t, err)

		cs, err := handshake(t, cert, tls.VersionTLS12, pool)
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), cs.Version)
		require.Equal(t, 1, k.signs)

		// Without the cap, TLS 1.3 is negotiated and no scheme fits: this is what CheckTLS13 prevents.
		_, err = handshake(t, cert, 0, pool)
		require.Error(t, err)
		conf := &tls.Config{Certificates: []tls.Certificate{cert}}
		require.ErrorContains(t, CheckTLS13(conf), "TLS 1.3 is enabled")
		conf.MaxVersion = tls.VersionTLS12
		require.NoError(t, CheckTLS13(conf))
	})

	t.Run("unrestricted key signs TLS 1.3", func(t *testing.T) {
		k := &memKey{Signer: rsaKey}
		memKeys.Store("mem:any", k)
		cert, err := Load(ctx, certFile, "mem:any")
		require.NoError(t, err)
		require.NoError(t, CheckTLS13(&tls.Config{Certificates: []tls.Certificate{cert}}))

		cs, err := handshake(t, cert, 0, pool)
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS13), cs.Version)
		require.Equal(t, 1, k.signs)
	})
}

func TestCheckTLS13(t *testing.T) {
	cert := func(schemes ...tls.SignatureScheme) tls.Certificate {
		return tls.Certificate{SupportedSignatureAlgorithms: schemes}
	}
	for name, tc := range map[string]struct {
		conf    *tls.Config
		wantErr bool
	}{
		"no certificates":    {&tls.Config{}, false},
		"unrestricted key":   {&tls.Config{Certificates: []tls.Certificate{cert()}}, false},
		"pkcs1 only":         {&tls.Config{Certificates: []tls.Certificate{cert(tls.PKCS1WithSHA256)}}, true},
		"sha1 only":          {&tls.Config{Certificates: []tls.Certificate{cert(tls.ECDSAWithSHA1)}}, true},
		"pkcs1 only, capped": {&tls.Config{Certificates: []tls.Certificate{cert(tls.PKCS1WithSHA256)}, MaxVersion: tls.VersionTLS12}, false},
		"pkcs1 only, tls1.3": {&tls.Config{Certificates: []tls.Certificate{cert(tls.PKCS1WithSHA256)}, MaxVersion: tls.VersionTLS13}, true},
		"pkcs1 and pss":      {&tls.Config{Certificates: []tls.Certificate{cert(tls.PKCS1WithSHA256, tls.PSSWithSHA256)}}, false},
		"second certificate": {&tls.Config{Certificates: []tls.Certificate{cert(), cert(tls.PKCS1WithSHA512)}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckTLS13(tc.conf)
			if tc.wantErr {
				require.ErrorContains(t, err, "TLS 1.3")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
