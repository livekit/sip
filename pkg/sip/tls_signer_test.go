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


package sip

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"hash/crc32"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/livekit/sip/pkg/config"
)

// fakeKMS answers like Cloud KMS from a local key.
type fakeKMS struct {
	key    crypto.Signer
	alg    kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
	signs  int
	closed bool
}

func (f *fakeKMS) GetPublicKey(_ context.Context, req *kmspb.GetPublicKeyRequest, _ ...gax.CallOption) (*kmspb.PublicKey, error) {
	der, err := x509.MarshalPKIXPublicKey(f.key.Public())
	if err != nil {
		return nil, err
	}
	return &kmspb.PublicKey{Name: req.Name, Algorithm: f.alg, Pem: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}, nil
}

func (f *fakeKMS) AsymmetricSign(_ context.Context, req *kmspb.AsymmetricSignRequest, _ ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	f.signs++
	alg := gcpKMSAlgorithms[f.alg]
	var digest []byte
	switch d := req.Digest.Digest.(type) {
	case *kmspb.Digest_Sha256:
		digest = d.Sha256
	case *kmspb.Digest_Sha384:
		digest = d.Sha384
	case *kmspb.Digest_Sha512:
		digest = d.Sha512
	}
	var opts crypto.SignerOpts = alg.hash
	if alg.scheme == tls.PSSWithSHA256 || alg.scheme == tls.PSSWithSHA512 {
		opts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: alg.hash}
	}
	sig, err := f.key.Sign(rand.Reader, digest, opts)
	if err != nil {
		return nil, err
	}
	return &kmspb.AsymmetricSignResponse{
		Name:                 req.Name,
		Signature:            sig,
		SignatureCrc32C:      wrapperspb.Int64(int64(crc32.Checksum(sig, crc32c))),
		VerifiedDigestCrc32C: req.DigestCrc32C.GetValue() == int64(crc32.Checksum(digest, crc32c)),
	}, nil
}

func (f *fakeKMS) Close() error { f.closed = true; return nil }

// writeCert writes a self-signed certificate for key and returns its path and pool.
func writeCert(t *testing.T, key crypto.Signer) (string, *x509.CertPool) {
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sip.example.com"},
		DNSNames:              []string{"sip.example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "fullchain.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return path, pool
}

func registerFakeKMS(t *testing.T, f *fakeKMS) {
	RegisterKeySigner("fakekms", func(ctx context.Context, uri string) (crypto.Signer, error) {
		return newGCPKMSSignerWithClient(ctx, f, uri)
	})
	t.Cleanup(func() { delete(keySigners, "fakekms") })
}

func handshake(t *testing.T, cert tls.Certificate, maxVersion uint16, roots *x509.CertPool) (tls.ConnectionState, error) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	go func() {
		_ = tls.Server(srv, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: maxVersion}).Handshake()
	}()
	c := tls.Client(cli, &tls.Config{ServerName: "sip.example.com", RootCAs: roots})
	err := c.Handshake()
	return c.ConnectionState(), err
}

func TestTLSKeySigner(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	t.Run("pkcs1 key signs a TLS 1.2 handshake and refuses TLS 1.3", func(t *testing.T) {
		f := &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256}
		registerFakeKMS(t, f)
		certFile, roots := writeCert(t, rsaKey)
		cert, err := loadTLSCertificate(context.Background(), config.TLSCert{CertFile: certFile, KeyURI: "fakekms://k/1"})
		require.NoError(t, err)

		cs, err := handshake(t, cert, tls.VersionTLS12, roots)
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), cs.Version)
		require.Equal(t, 1, f.signs)

		require.Error(t, checkTLS13Signers(&tls.Config{Certificates: []tls.Certificate{cert}}))
		require.NoError(t, checkTLS13Signers(&tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tls.VersionTLS12}))

		require.NoError(t, cert.PrivateKey.(interface{ Close() error }).Close())
		require.True(t, f.closed)
	})

	t.Run("pss and ecdsa keys sign a TLS 1.3 handshake", func(t *testing.T) {
		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		for _, f := range []*fakeKMS{
			{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256},
			{key: ecKey, alg: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256},
		} {
			registerFakeKMS(t, f)
			certFile, roots := writeCert(t, f.key)
			cert, err := loadTLSCertificate(context.Background(), config.TLSCert{CertFile: certFile, KeyURI: "fakekms://k/1"})
			require.NoError(t, err)
			require.NoError(t, checkTLS13Signers(&tls.Config{Certificates: []tls.Certificate{cert}}))

			cs, err := handshake(t, cert, 0, roots)
			require.NoError(t, err, f.alg)
			require.Equal(t, uint16(tls.VersionTLS13), cs.Version)
			require.Equal(t, 1, f.signs)
		}
	})

	t.Run("rejects a certificate that does not match the key", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		f := &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256}
		registerFakeKMS(t, f)
		certFile, _ := writeCert(t, other)
		_, err = loadTLSCertificate(context.Background(), config.TLSCert{CertFile: certFile, KeyURI: "fakekms://k/1"})
		require.ErrorContains(t, err, "does not match")
		require.True(t, f.closed)
	})

	t.Run("rejects bad key config", func(t *testing.T) {
		_, err := loadTLSCertificate(context.Background(), config.TLSCert{CertFile: "c", KeyFile: "k", KeyURI: "gcpkms://x"})
		require.ErrorContains(t, err, "not both")
		_, err = loadTLSCertificate(context.Background(), config.TLSCert{CertFile: "c", KeyURI: "nope://x"})
		require.ErrorContains(t, err, "unsupported key_uri scheme")
	})

	t.Run("refuses a corrupted KMS response", func(t *testing.T) {
		f := &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256}
		s, err := newGCPKMSSignerWithClient(context.Background(), corruptKMS{f}, "k/1")
		require.NoError(t, err)
		_, err = s.Sign(nil, make([]byte, 32), crypto.SHA256)
		require.ErrorContains(t, err, "corrupted")
		_, err = s.Sign(nil, make([]byte, 64), crypto.SHA512)
		require.ErrorContains(t, err, "cannot sign")
	})
}

func TestTLSKeySignerErrors(t *testing.T) {
	ctx := context.Background()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	t.Run("p384 and sha512 keys sign a handshake", func(t *testing.T) {
		p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		require.NoError(t, err)
		for _, f := range []*fakeKMS{
			{key: p384, alg: kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384},
			{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512},
		} {
			registerFakeKMS(t, f)
			certFile, roots := writeCert(t, f.key)
			cert, err := loadTLSCertificate(ctx, config.TLSCert{CertFile: certFile, KeyURI: "fakekms://k/1"})
			require.NoError(t, err)
			_, err = handshake(t, cert, tls.VersionTLS12, roots)
			require.NoError(t, err, f.alg)
			require.Equal(t, 1, f.signs)
		}
	})

	t.Run("KMS key the signer cannot use", func(t *testing.T) {
		_, err := newGCPKMSSignerWithClient(ctx, &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION}, "k/1")
		require.ErrorContains(t, err, "unsupported key algorithm")
		_, err = newGCPKMSSignerWithClient(ctx, failingKMS{&fakeKMS{key: rsaKey}}, "k/1")
		require.ErrorContains(t, err, "unavailable")
	})

	t.Run("KMS sign error fails the handshake", func(t *testing.T) {
		s, err := newGCPKMSSignerWithClient(ctx, &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256}, "k/1")
		require.NoError(t, err)
		s.client = failingKMS{s.client.(*fakeKMS)}
		_, err = s.Sign(nil, make([]byte, 32), crypto.SHA256)
		require.ErrorContains(t, err, "unavailable")
	})

	t.Run("bad certificate file", func(t *testing.T) {
		registerFakeKMS(t, &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256})
		_, err := loadTLSCertificate(ctx, config.TLSCert{CertFile: filepath.Join(t.TempDir(), "missing.pem"), KeyURI: "fakekms://k/1"})
		require.Error(t, err)
		empty := filepath.Join(t.TempDir(), "empty.pem")
		require.NoError(t, os.WriteFile(empty, []byte("not a pem"), 0600))
		_, err = loadTLSCertificate(ctx, config.TLSCert{CertFile: empty, KeyURI: "fakekms://k/1"})
		require.ErrorContains(t, err, "no certificate")
	})

	t.Run("signer that fails to open", func(t *testing.T) {
		registerFakeKMS(t, &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION})
		certFile, _ := writeCert(t, rsaKey)
		_, err := loadTLSCertificate(ctx, config.TLSCert{CertFile: certFile, KeyURI: "fakekms://k/1"})
		require.ErrorContains(t, err, "open fakekms://k/1")
	})
}

type failingKMS struct{ *fakeKMS }

func (failingKMS) GetPublicKey(context.Context, *kmspb.GetPublicKeyRequest, ...gax.CallOption) (*kmspb.PublicKey, error) {
	return nil, errors.New("unavailable")
}

func (failingKMS) AsymmetricSign(context.Context, *kmspb.AsymmetricSignRequest, ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	return nil, errors.New("unavailable")
}

type corruptKMS struct{ *fakeKMS }

func (c corruptKMS) AsymmetricSign(ctx context.Context, req *kmspb.AsymmetricSignRequest, opts ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	resp, err := c.fakeKMS.AsymmetricSign(ctx, req, opts...)
	if err == nil {
		resp.Signature[0] ^= 0xff
	}
	return resp, err
}
