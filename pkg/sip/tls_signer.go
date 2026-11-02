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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/livekit/sip/pkg/config"
)

// KeySignerFunc opens the private key that a TLS key_uri points to.
//
// The returned signer may also implement:
//   - io.Closer, called when the service stops;
//   - SignatureAlgorithms() []tls.SignatureScheme, restricting what TLS asks it to sign,
//     for keys that only support one algorithm (as in most KMS and HSM backends).
type KeySignerFunc func(ctx context.Context, uri string) (crypto.Signer, error)

var keySigners = map[string]KeySignerFunc{
	"gcpkms": newGCPKMSSigner,
}

// RegisterKeySigner makes key_uri values starting with "<scheme>://" load through fn.
// Call it before the service starts.
func RegisterKeySigner(scheme string, fn KeySignerFunc) {
	keySigners[scheme] = fn
}

func loadTLSCertificate(ctx context.Context, c config.TLSCert) (tls.Certificate, error) {
	if c.KeyURI == "" {
		return tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	}
	if c.KeyFile != "" {
		return tls.Certificate{}, errors.New("TLS cert: set either key_file or key_uri, not both")
	}
	scheme, _, _ := strings.Cut(c.KeyURI, "://")
	open := keySigners[scheme]
	if open == nil {
		return tls.Certificate{}, fmt.Errorf("TLS cert: unsupported key_uri scheme %q", scheme)
	}
	certPEM, err := os.ReadFile(c.CertFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	var cert tls.Certificate
	for block, rest := pem.Decode(certPEM); block != nil; block, rest = pem.Decode(rest) {
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("TLS cert: no certificate in %s", c.CertFile)
	}
	if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return tls.Certificate{}, err
	}
	signer, err := open(ctx, c.KeyURI)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("TLS cert: open %s: %w", c.KeyURI, err)
	}
	if pub, ok := cert.Leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(signer.Public()) {
		if cl, ok := signer.(io.Closer); ok {
			_ = cl.Close()
		}
		return tls.Certificate{}, fmt.Errorf("TLS cert: public key in %s does not match %s", c.CertFile, c.KeyURI)
	}
	cert.PrivateKey = signer
	if a, ok := signer.(interface{ SignatureAlgorithms() []tls.SignatureScheme }); ok {
		cert.SupportedSignatureAlgorithms = a.SignatureAlgorithms()
	}
	return cert, nil
}

// checkTLS13Signers fails when TLS 1.3 is allowed but a key can only sign PKCS#1 v1.5,
// which TLS 1.3 forbids: every TLS 1.3 handshake on that certificate would fail.
func checkTLS13Signers(c *tls.Config) error {
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS13 {
		return nil
	}
	tls12Only := []tls.SignatureScheme{tls.PKCS1WithSHA1, tls.PKCS1WithSHA256, tls.PKCS1WithSHA384, tls.PKCS1WithSHA512, tls.ECDSAWithSHA1}
	for i, cert := range c.Certificates {
		algs := cert.SupportedSignatureAlgorithms
		if len(algs) != 0 && !slices.ContainsFunc(algs, func(s tls.SignatureScheme) bool { return !slices.Contains(tls12Only, s) }) {
			return fmt.Errorf("TLS cert %d: its key only signs PKCS#1 v1.5, which TLS 1.3 does not allow; set tls.max_version: tls1.2", i)
		}
	}
	return nil
}
