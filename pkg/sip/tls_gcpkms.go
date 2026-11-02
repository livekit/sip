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
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const gcpKMSSignTimeout = 5 * time.Second

type gcpKMSClient interface {
	GetPublicKey(ctx context.Context, req *kmspb.GetPublicKeyRequest, opts ...gax.CallOption) (*kmspb.PublicKey, error)
	AsymmetricSign(ctx context.Context, req *kmspb.AsymmetricSignRequest, opts ...gax.CallOption) (*kmspb.AsymmetricSignResponse, error)
	Close() error
}

type gcpKMSAlgorithm struct {
	hash   crypto.Hash
	scheme tls.SignatureScheme
}

var gcpKMSAlgorithms = map[kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm]gcpKMSAlgorithm{
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256: {crypto.SHA256, tls.PKCS1WithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256: {crypto.SHA256, tls.PKCS1WithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256: {crypto.SHA256, tls.PKCS1WithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512: {crypto.SHA512, tls.PKCS1WithSHA512},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256:   {crypto.SHA256, tls.PSSWithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256:   {crypto.SHA256, tls.PSSWithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256:   {crypto.SHA256, tls.PSSWithSHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:   {crypto.SHA512, tls.PSSWithSHA512},
	kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:        {crypto.SHA256, tls.ECDSAWithP256AndSHA256},
	kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:        {crypto.SHA384, tls.ECDSAWithP384AndSHA384},
}

// gcpKMSSigner signs TLS handshakes with a Cloud KMS asymmetric key version.
// The private key never leaves KMS; the service account only needs roles/cloudkms.signer on it.
type gcpKMSSigner struct {
	client gcpKMSClient
	name   string
	pub    crypto.PublicKey
	alg    gcpKMSAlgorithm
}

var crc32c = crc32.MakeTable(crc32.Castagnoli)

// newGCPKMSSigner opens "gcpkms://projects/.../cryptoKeys/<key>/cryptoKeyVersions/<n>"
// with Application Default Credentials.
func newGCPKMSSigner(ctx context.Context, uri string) (crypto.Signer, error) {
	client, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, err
	}
	s, err := newGCPKMSSignerWithClient(ctx, client, strings.TrimPrefix(uri, "gcpkms://"))
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return s, nil
}

func newGCPKMSSignerWithClient(ctx context.Context, client gcpKMSClient, name string) (*gcpKMSSigner, error) {
	pk, err := client.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: name})
	if err != nil {
		return nil, err
	}
	alg, ok := gcpKMSAlgorithms[pk.Algorithm]
	if !ok {
		return nil, fmt.Errorf("unsupported key algorithm %s", pk.Algorithm)
	}
	block, _ := pem.Decode([]byte(pk.Pem))
	if block == nil {
		return nil, errors.New("no PEM public key returned")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return &gcpKMSSigner{client: client, name: name, pub: pub, alg: alg}, nil
}

func (s *gcpKMSSigner) Public() crypto.PublicKey { return s.pub }

func (s *gcpKMSSigner) SignatureAlgorithms() []tls.SignatureScheme {
	return []tls.SignatureScheme{s.alg.scheme}
}

func (s *gcpKMSSigner) Close() error { return s.client.Close() }

func (s *gcpKMSSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	_, pss := opts.(*rsa.PSSOptions)
	wantPSS := s.alg.scheme == tls.PSSWithSHA256 || s.alg.scheme == tls.PSSWithSHA512
	if opts.HashFunc() != s.alg.hash || pss != wantPSS {
		return nil, fmt.Errorf("KMS key %s cannot sign with %v (pss=%v)", s.name, opts.HashFunc(), pss)
	}
	d := &kmspb.Digest{}
	switch s.alg.hash {
	case crypto.SHA256:
		d.Digest = &kmspb.Digest_Sha256{Sha256: digest}
	case crypto.SHA384:
		d.Digest = &kmspb.Digest_Sha384{Sha384: digest}
	case crypto.SHA512:
		d.Digest = &kmspb.Digest_Sha512{Sha512: digest}
	}
	ctx, cancel := context.WithTimeout(context.Background(), gcpKMSSignTimeout)
	defer cancel()
	resp, err := s.client.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{
		Name:         s.name,
		Digest:       d,
		DigestCrc32C: wrapperspb.Int64(int64(crc32.Checksum(digest, crc32c))),
	})
	if err != nil {
		return nil, err
	}
	// Integrity checks recommended by Cloud KMS: the request arrived intact and the signature came back intact.
	if !resp.VerifiedDigestCrc32C || resp.Name != s.name ||
		resp.SignatureCrc32C.GetValue() != int64(crc32.Checksum(resp.Signature, crc32c)) {
		return nil, fmt.Errorf("KMS key %s: corrupted sign request or response", s.name)
	}
	return resp.Signature, nil
}
