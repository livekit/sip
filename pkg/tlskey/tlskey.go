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

// Package tlskey loads a TLS certificate whose private key stays in a KMS or HSM that signs for
// it. The key URI's scheme selects a backend registered with Register, usually from an init
// function that a blank import runs: _ "github.com/livekit/sip/pkg/tlskey/gcpkms".
package tlskey

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
)

// Opener opens the key a URI names. The signer may also implement io.Closer (closed when the
// service stops) and SignatureSchemer. Errors never echo the URI: a PKCS#11 URI can carry a PIN.
type Opener func(ctx context.Context, uri string) (crypto.Signer, error)

// SignatureSchemer limits a key to the TLS signature schemes it can produce. A KMS or HSM key
// must implement it, or TLS picks RSA-PSS for an RSA key and a PKCS#1 key fails every handshake.
type SignatureSchemer interface {
	SignatureSchemes() []tls.SignatureScheme
}

var (
	mu      sync.RWMutex
	openers = map[string]Opener{}
)

// Register adds the backend for a key URI scheme; like database/sql.Register, duplicates and nils panic.
func Register(scheme string, open Opener) {
	if scheme == "" || open == nil {
		panic("tlskey: Register with an empty scheme or a nil opener")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := openers[scheme]; dup {
		panic("tlskey: Register called twice for scheme " + scheme)
	}
	openers[scheme] = open
}

// Schemes returns the registered key URI schemes, sorted.
func Schemes() []string {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Sorted(maps.Keys(openers))
}

// Open opens the key uri names. The scheme ends at the first ':', so "pkcs11:..." works like "gcpkms://...".
func Open(ctx context.Context, uri string) (crypto.Signer, error) {
	scheme, _, ok := strings.Cut(uri, ":")
	if !ok || scheme == "" {
		return nil, fmt.Errorf("tlskey: key URI is not a URI")
	}
	mu.RLock()
	open := openers[scheme]
	mu.RUnlock()
	if open == nil {
		return nil, fmt.Errorf("tlskey: no backend for key URI scheme %q (registered: %s)", scheme, strings.Join(Schemes(), ", "))
	}
	signer, err := open(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("tlskey: open %s key: %w", scheme, err)
	}
	return signer, nil
}

// Load reads the PEM chain in certFile and opens its key at keyURI, like tls.LoadX509KeyPair for a key file.
func Load(ctx context.Context, certFile, keyURI string) (tls.Certificate, error) {
	cert, err := loadChain(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	signer, err := Open(ctx, keyURI)
	if err != nil {
		return tls.Certificate{}, err
	}
	if pub, ok := cert.Leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(signer.Public()) {
		if c, ok := signer.(io.Closer); ok {
			_ = c.Close()
		}
		return tls.Certificate{}, fmt.Errorf("tlskey: the public key in %s is not the key's", certFile)
	}
	cert.PrivateKey = signer
	if s, ok := signer.(SignatureSchemer); ok {
		cert.SupportedSignatureAlgorithms = s.SignatureSchemes()
	}
	return cert, nil
}

func loadChain(certFile string) (tls.Certificate, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlskey: %w", err)
	}
	var cert tls.Certificate
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("tlskey: no certificate in %s", certFile)
	}
	if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlskey: %s: %w", certFile, err)
	}
	return cert, nil
}

// tls12Only are the signature schemes TLS 1.3 removed: PKCS#1 v1.5 and SHA-1.
var tls12Only = []tls.SignatureScheme{
	tls.PKCS1WithSHA1, tls.PKCS1WithSHA256, tls.PKCS1WithSHA384, tls.PKCS1WithSHA512, tls.ECDSAWithSHA1,
}

// CheckTLS13 fails when c allows TLS 1.3 but a certificate's key signs only tls12Only schemes:
// every TLS 1.3 handshake on it would fail. The caller adds the configuration hint.
func CheckTLS13(c *tls.Config) error {
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS13 {
		return nil
	}
	for i, cert := range c.Certificates {
		schemes := cert.SupportedSignatureAlgorithms
		if len(schemes) == 0 {
			continue
		}
		if !slices.ContainsFunc(schemes, func(s tls.SignatureScheme) bool { return !slices.Contains(tls12Only, s) }) {
			return fmt.Errorf("tlskey: certificate %d: its key only signs %v, which TLS 1.3 does not allow, but TLS 1.3 is enabled", i, schemes)
		}
	}
	return nil
}
