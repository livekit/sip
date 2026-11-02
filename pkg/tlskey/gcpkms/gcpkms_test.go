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

package gcpkms

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
	"io"
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

	"github.com/livekit/sip/pkg/tlskey"
)

const (
	testName = "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"
	testURI  = "gcpkms://" + testName
)

// fakeKMS answers like Cloud KMS from a local key.
type fakeKMS struct {
	key     crypto.Signer
	alg     kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
	pem     string // replaces the real public key when set
	badCRC  bool   // corrupts the public key's checksum
	pubErr  error
	signErr error
	mangle  func(*kmspb.AsymmetricSignResponse)

	name   string // the key version asked for
	signs  int
	closed bool
}

func (f *fakeKMS) GetPublicKey(_ context.Context, req *kmspb.GetPublicKeyRequest, _ ...gax.CallOption) (*kmspb.PublicKey, error) {
	f.name = req.Name
	if f.pubErr != nil {
		return nil, f.pubErr
	}
	p := f.pem
	if p == "" {
		der, err := x509.MarshalPKIXPublicKey(f.key.Public())
		if err != nil {
			return nil, err
		}
		p = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	}
	crc := crc32c([]byte(p))
	if f.badCRC {
		crc++
	}
	return &kmspb.PublicKey{Name: req.Name, Pem: p, PemCrc32C: wrapperspb.Int64(crc), Algorithm: f.alg}, nil
}

func (f *fakeKMS) AsymmetricSign(_ context.Context, req *kmspb.AsymmetricSignRequest, _ ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error) {
	f.signs++
	if f.signErr != nil {
		return nil, f.signErr
	}
	alg := algorithms[f.alg]
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
	if alg.pss {
		opts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: alg.hash}
	}
	sig, err := f.key.Sign(rand.Reader, digest, opts)
	if err != nil {
		return nil, err
	}
	resp := &kmspb.AsymmetricSignResponse{
		Name:                 req.Name,
		Signature:            sig,
		SignatureCrc32C:      wrapperspb.Int64(crc32c(sig)),
		VerifiedDigestCrc32C: req.DigestCrc32C.GetValue() == crc32c(digest),
	}
	if f.mangle != nil {
		f.mangle(resp)
	}
	return resp, nil
}

func (f *fakeKMS) Close() error {
	f.closed = true
	return nil
}

// useFake makes Open talk to f instead of Cloud KMS.
func useFake(t *testing.T, f *fakeKMS) {
	t.Helper()
	prev := newClient
	newClient = func(context.Context) (client, error) { return f, nil }
	t.Cleanup(func() { newClient = prev })
}

func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func mustEC(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	return key
}

// writeCert writes a self-signed server certificate for key and returns its path and a pool
// trusting it.
func writeCert(t *testing.T, key crypto.Signer) (string, *x509.CertPool) {
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
	path := filepath.Join(t.TempDir(), "fullchain.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return path, pool
}

func handshake(t *testing.T, cert tls.Certificate, maxVersion uint16, roots *x509.CertPool) tls.ConnectionState {
	t.Helper()
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tls.Server(srv, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: maxVersion}).Handshake()
	}()
	c := tls.Client(cli, &tls.Config{ServerName: "sip.example.com", RootCAs: roots})
	require.NoError(t, c.Handshake())
	require.NoError(t, <-srvErr)
	return c.ConnectionState()
}

func TestRegistered(t *testing.T) {
	require.Contains(t, tlskey.Schemes(), Scheme)
}

func TestOpen(t *testing.T) {
	ctx := context.Background()
	rsaKey := mustRSA(t)

	t.Run("URI to key version", func(t *testing.T) {
		f := &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256}
		useFake(t, f)
		k, err := Open(ctx, testURI)
		require.NoError(t, err)
		require.Equal(t, testName, f.name)
		require.True(t, rsaKey.PublicKey.Equal(k.Public()))
		require.Equal(t, []tls.SignatureScheme{tls.PKCS1WithSHA256}, k.(tlskey.SignatureSchemer).SignatureSchemes())
		require.NoError(t, k.(io.Closer).Close())
		require.True(t, f.closed)
	})

	t.Run("URI that is not a key version", func(t *testing.T) {
		useFake(t, &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256})
		for _, uri := range []string{
			"gcpkms://k/1",
			"gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
			"gcpkms:" + testName,
			testName,
		} {
			_, err := Open(ctx, uri)
			require.ErrorContains(t, err, "want gcpkms://projects/P/", uri)
		}
	})

	t.Run("no client", func(t *testing.T) {
		prev := newClient
		newClient = func(context.Context) (client, error) { return nil, errors.New("no credentials") }
		t.Cleanup(func() { newClient = prev })
		_, err := Open(ctx, testURI)
		require.ErrorContains(t, err, "no credentials")
	})

	t.Run("key KMS cannot describe", func(t *testing.T) {
		pkcs1 := kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256
		for name, tc := range map[string]struct {
			f       *fakeKMS
			wantErr string
		}{
			"lookup error":       {&fakeKMS{key: rsaKey, alg: pkcs1, pubErr: errors.New("permission denied")}, "permission denied"},
			"corrupted checksum": {&fakeKMS{key: rsaKey, alg: pkcs1, badCRC: true}, "corrupted in transit"},
			"not PEM":            {&fakeKMS{key: rsaKey, alg: pkcs1, pem: "garbage"}, "is not PEM"},
			"not a public key":   {&fakeKMS{key: rsaKey, alg: pkcs1, pem: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("junk")}))}, "asn1"},
			"symmetric key":      {&fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION}, "cannot sign TLS handshakes"},
			"ed25519 key":        {&fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_EC_SIGN_ED25519}, "cannot sign TLS handshakes"},
		} {
			t.Run(name, func(t *testing.T) {
				useFake(t, tc.f)
				_, err := Open(ctx, testURI)
				require.ErrorContains(t, err, tc.wantErr)
				require.True(t, tc.f.closed, "client left open")
			})
		}
	})
}

// TestHandshake loads a certificate through tlskey with a gcpkms:// key for every supported
// algorithm and completes a real TLS handshake with one KMS signature.
func TestHandshake(t *testing.T) {
	ctx := context.Background()
	rsaKey, p256, p384 := mustRSA(t), mustEC(t, elliptic.P256()), mustEC(t, elliptic.P384())
	for _, tc := range []struct {
		alg        kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
		key        crypto.Signer
		maxVersion uint16 // PKCS#1 keys need the TLS 1.2 cap; 0 lets TLS 1.3 through
		scheme     tls.SignatureScheme
	}{
		{kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256, rsaKey, tls.VersionTLS12, tls.PKCS1WithSHA256},
		{kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512, rsaKey, tls.VersionTLS12, tls.PKCS1WithSHA512},
		{kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256, rsaKey, 0, tls.PSSWithSHA256},
		{kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512, rsaKey, 0, tls.PSSWithSHA512},
		{kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, p256, 0, tls.ECDSAWithP256AndSHA256},
		{kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384, p384, 0, tls.ECDSAWithP384AndSHA384},
	} {
		t.Run(tc.alg.String(), func(t *testing.T) {
			f := &fakeKMS{key: tc.key, alg: tc.alg}
			useFake(t, f)
			certFile, pool := writeCert(t, tc.key)
			cert, err := tlskey.Load(ctx, certFile, testURI)
			require.NoError(t, err)
			require.Equal(t, []tls.SignatureScheme{tc.scheme}, cert.SupportedSignatureAlgorithms)
			conf := &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: tc.maxVersion}
			require.NoError(t, tlskey.CheckTLS13(conf))

			cs := handshake(t, cert, tc.maxVersion, pool)
			if tc.maxVersion == 0 {
				require.Equal(t, uint16(tls.VersionTLS13), cs.Version)
			} else {
				require.Equal(t, tc.maxVersion, cs.Version)
			}
			require.Equal(t, 1, f.signs)
		})
	}
}

func TestSign(t *testing.T) {
	ctx := context.Background()
	rsaKey := mustRSA(t)
	pkcs1 := func() *fakeKMS {
		return &fakeKMS{key: rsaKey, alg: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256}
	}
	mustOpen := func(t *testing.T, f *fakeKMS) *key {
		t.Helper()
		k, err := open(ctx, f, testName)
		require.NoError(t, err)
		return k
	}
	digest := make([]byte, crypto.SHA256.Size())

	t.Run("the key's own algorithm only", func(t *testing.T) {
		f := pkcs1()
		k := mustOpen(t, f)
		sig, err := k.Sign(nil, digest, crypto.SHA256)
		require.NoError(t, err)
		require.NoError(t, rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, digest, sig))

		for name, call := range map[string]func() ([]byte, error){
			"pss padding": func() ([]byte, error) {
				return k.Sign(nil, digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
			},
			"other hash":   func() ([]byte, error) { return k.Sign(nil, make([]byte, crypto.SHA384.Size()), crypto.SHA384) },
			"short digest": func() ([]byte, error) { return k.Sign(nil, digest[:16], crypto.SHA256) },
		} {
			_, err := call()
			require.ErrorContains(t, err, "cannot sign", name)
		}
		require.Equal(t, 1, f.signs, "refused requests must not reach KMS")
	})

	t.Run("KMS errors and corrupted replies", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mangle  func(*kmspb.AsymmetricSignResponse)
			signErr error
			wantErr string
		}{
			"unavailable":           {signErr: errors.New("unavailable"), wantErr: "unavailable"},
			"digest did not arrive": {mangle: func(r *kmspb.AsymmetricSignResponse) { r.VerifiedDigestCrc32C = false }, wantErr: "corrupted"},
			"signature changed":     {mangle: func(r *kmspb.AsymmetricSignResponse) { r.Signature[0] ^= 1 }, wantErr: "corrupted"},
			"no signature checksum": {mangle: func(r *kmspb.AsymmetricSignResponse) { r.SignatureCrc32C = nil }, wantErr: "corrupted"},
			"another key answered":  {mangle: func(r *kmspb.AsymmetricSignResponse) { r.Name = "projects/other" }, wantErr: "corrupted"},
		} {
			t.Run(name, func(t *testing.T) {
				f := pkcs1()
				f.mangle, f.signErr = tc.mangle, tc.signErr
				_, err := mustOpen(t, f).Sign(nil, digest, crypto.SHA256)
				require.ErrorContains(t, err, tc.wantErr)
			})
		}
	})
}
