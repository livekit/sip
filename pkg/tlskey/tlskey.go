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

// Package tlskey loads a TLS certificate whose private key stays outside the process, in a
// KMS, an HSM or any other signer that never hands the key out.
//
// A certificate names its key with a URI, and the URI scheme selects the backend. A backend
// registers itself with [Register], usually from an init function that a blank import runs:
//
//	import _ "github.com/livekit/sip/pkg/tlskey/gcpkms"
//
// A backend only has to return a [crypto.Signer]: crypto/tls accepts any signer as a private
// key, so nothing else in the TLS stack changes.
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

// Opener opens the private key that a key URI names. The returned signer may also implement
//   - [io.Closer], called when the service stops;
//   - [SignatureSchemer], which a KMS or HSM backend must implement.
//
// A key URI can carry a secret (an RFC 7512 PKCS#11 URI has pin-value), so this package never
// puts the URI in an error; a backend should name the key some other way.
type Opener func(ctx context.Context, uri string) (crypto.Signer, error)

// SignatureSchemer is implemented by signers that can produce only some TLS signature
// schemes. A KMS or HSM key version is created for one algorithm, so its backend must
// implement it: otherwise TLS picks RSA-PSS for any RSA key, and a PKCS#1 key fails every
// handshake. [Load] copies the schemes into tls.Certificate.SupportedSignatureAlgorithms.
type SignatureSchemer interface {
	SignatureSchemes() []tls.SignatureScheme
}

var (
	mu      sync.RWMutex
	openers = map[string]Opener{}
)

// Register makes [Open] and [Load] handle key URIs of the given scheme. Like
// database/sql.Register, it panics on an empty scheme, a nil opener or a scheme registered twice.
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

// Open opens the private key uri names through the backend registered for its scheme. The
// scheme ends at the first ':', so "gcpkms://..." and "pkcs11:..." both reach their backend.
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

// Load reads the PEM certificate chain in certFile and opens its private key at keyURI, as
// tls.LoadX509KeyPair does for a key file. The chain's public key must match the signer's.
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

// CheckTLS13 returns an error when c allows TLS 1.3 but a certificate's key can produce only
// signature schemes TLS 1.3 removed: every TLS 1.3 handshake on that certificate would fail,
// so c has to cap the version at TLS 1.2 instead. The caller knows how its configuration sets
// that cap and adds the hint.
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
