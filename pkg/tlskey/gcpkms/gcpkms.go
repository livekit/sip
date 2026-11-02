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

// Package gcpkms signs TLS handshakes with a Google Cloud KMS asymmetric key. The key never
// leaves KMS, and the service account only needs roles/cloudkms.signer on the key version.
//
// Importing the package registers the "gcpkms" key URI scheme with tlskey:
//
//	gcpkms://projects/P/locations/L/keyRings/R/cryptoKeys/K/cryptoKeyVersions/N
//
// The client uses Application Default Credentials. Each new TLS connection makes one
// AsymmetricSign call; established connections make none.
package gcpkms

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/livekit/sip/pkg/tlskey"
)

// Scheme is the key URI scheme this package registers.
const Scheme = "gcpkms"

// signTimeout bounds one AsymmetricSign call. The SDK's default is 60s, far longer than any
// peer waits for a TLS handshake.
const signTimeout = 5 * time.Second

func init() {
	tlskey.Register(Scheme, Open)
}

// client is the part of kms.KeyManagementClient a key uses; tests supply a fake.
type client interface {
	GetPublicKey(ctx context.Context, req *kmspb.GetPublicKeyRequest, opts ...gax.CallOption) (*kmspb.PublicKey, error)
	AsymmetricSign(ctx context.Context, req *kmspb.AsymmetricSignRequest, opts ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error)
	Close() error
}

var newClient = func(ctx context.Context) (client, error) {
	return kms.NewKeyManagementClient(ctx)
}

type algorithm struct {
	hash   crypto.Hash
	pss    bool
	scheme tls.SignatureScheme
}

// algorithms maps each KMS signing algorithm a TLS certificate can use to the digest KMS
// expects and the one signature scheme the key produces. Ed25519, secp256k1, raw PKCS#1 and
// the post-quantum algorithms are left out.
var algorithms = map[kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm]algorithm{
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256: {crypto.SHA256, false, tls.PKCS1WithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256: {crypto.SHA256, false, tls.PKCS1WithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256: {crypto.SHA256, false, tls.PKCS1WithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512: {crypto.SHA512, false, tls.PKCS1WithSHA512},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256:   {crypto.SHA256, true, tls.PSSWithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256:   {crypto.SHA256, true, tls.PSSWithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256:   {crypto.SHA256, true, tls.PSSWithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:   {crypto.SHA512, true, tls.PSSWithSHA512},
	kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:        {crypto.SHA256, false, tls.ECDSAWithP256AndSHA256},
	kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:        {crypto.SHA384, false, tls.ECDSAWithP384AndSHA384},
}

// key is a Cloud KMS key version used as a TLS private key.
type key struct {
	client client
	name   string
	pub    crypto.PublicKey
	alg    algorithm
}

// Open opens the key version a gcpkms:// URI names, with Application Default Credentials.
func Open(ctx context.Context, uri string) (crypto.Signer, error) {
	name, err := parseURI(uri)
	if err != nil {
		return nil, err
	}
	// The client outlives ctx: it signs every later handshake.
	c, err := newClient(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	k, err := open(ctx, c, name)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return k, nil
}

func parseURI(uri string) (string, error) {
	name, ok := strings.CutPrefix(uri, Scheme+"://")
	if !ok || !strings.HasPrefix(name, "projects/") || !strings.Contains(name, "/cryptoKeyVersions/") {
		return "", fmt.Errorf("gcpkms: key URI must be %s://projects/P/locations/L/keyRings/R/cryptoKeys/K/cryptoKeyVersions/N", Scheme)
	}
	return name, nil
}

func open(ctx context.Context, c client, name string) (*key, error) {
	pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("gcpkms: get public key of %s: %w", name, err)
	}
	if pk.PemCrc32C != nil && pk.PemCrc32C.Value != crc32c([]byte(pk.Pem)) {
		return nil, fmt.Errorf("gcpkms: public key of %s: response corrupted in transit", name)
	}
	alg, ok := algorithms[pk.Algorithm]
	if !ok {
		return nil, fmt.Errorf("gcpkms: %s: algorithm %s cannot sign TLS handshakes", name, pk.Algorithm)
	}
	block, _ := pem.Decode([]byte(pk.Pem))
	if block == nil {
		return nil, fmt.Errorf("gcpkms: public key of %s is not PEM", name)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: public key of %s: %w", name, err)
	}
	return &key{client: c, name: name, pub: pub, alg: alg}, nil
}

func (k *key) Public() crypto.PublicKey { return k.pub }

// SignatureSchemes returns the one scheme the key version was created for.
func (k *key) SignatureSchemes() []tls.SignatureScheme { return []tls.SignatureScheme{k.alg.scheme} }

// Close closes the KMS client.
func (k *key) Close() error { return k.client.Close() }

// Sign asks KMS to sign digest. TLS passes the opts of the scheme it negotiated, which
// SignatureSchemes limited to the key's own; anything else is refused before the call.
func (k *key) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	_, pss := opts.(*rsa.PSSOptions)
	if opts.HashFunc() != k.alg.hash || pss != k.alg.pss || len(digest) != k.alg.hash.Size() {
		return nil, fmt.Errorf("gcpkms: %s cannot sign a %d-byte %v digest (pss=%v)", k.name, len(digest), opts.HashFunc(), pss)
	}
	req := &kmspb.AsymmetricSignRequest{
		Name:         k.name,
		Digest:       &kmspb.Digest{},
		DigestCrc32C: wrapperspb.Int64(crc32c(digest)),
	}
	switch k.alg.hash {
	case crypto.SHA256:
		req.Digest.Digest = &kmspb.Digest_Sha256{Sha256: digest}
	case crypto.SHA384:
		req.Digest.Digest = &kmspb.Digest_Sha384{Sha384: digest}
	case crypto.SHA512:
		req.Digest.Digest = &kmspb.Digest_Sha512{Sha512: digest}
	}
	ctx, cancel := context.WithTimeout(context.Background(), signTimeout)
	defer cancel()
	resp, err := k.client.AsymmetricSign(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: sign with %s: %w", k.name, err)
	}
	// The checks Cloud KMS documents: the digest arrived intact, the right key answered, and
	// the signature came back intact.
	if !resp.VerifiedDigestCrc32C || resp.Name != k.name || resp.SignatureCrc32C.GetValue() != crc32c(resp.Signature) {
		return nil, fmt.Errorf("gcpkms: sign with %s: request or response corrupted in transit", k.name)
	}
	return resp.Signature, nil
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) int64 { return int64(crc32.Checksum(b, castagnoli)) }
