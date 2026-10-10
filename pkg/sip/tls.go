// Copyright 2025 LiveKit, Inc.
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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/livekit/sip/pkg/config"
)

// loadTLSCertificates loads PEM certificate/key pairs from disk.
func loadTLSCertificates(certs []config.TLSCert) ([]tls.Certificate, error) {
	out := make([]tls.Certificate, 0, len(certs))
	for _, c := range certs {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS cert %q: %w", c.CertFile, err)
		}
		out = append(out, cert)
	}
	return out, nil
}

// clientCertificateFunc returns a GetClientCertificate hook that always
// presents the first configured certificate when the peer requests client
// auth. Go's default selection can return no certificate when the peer's
// acceptable CAs don't match our leaf, which breaks outbound SIP trunks that
// require mTLS (livekit/sip#530).
func clientCertificateFunc(certs []tls.Certificate) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		if len(certs) == 0 {
			return nil, nil
		}
		return &certs[0], nil
	}
}

func ConfigureTLS(c *tls.Config) {
	// We can't use default cert verification, because SIP headers usually specify IP address instead of a hostname.
	// At least, we could validate certificate chain using VerifyPeerCertificate and ignore the server name for now.
	//
	// Code from crypto/tls.Conn.verifyServerCertificate.
	c.InsecureSkipVerify = true
	c.VerifyPeerCertificate = func(certificates [][]byte, verifiedChains [][]*x509.Certificate) error {
		certs := make([]*x509.Certificate, len(certificates))
		for i, asn1Data := range certificates {
			cert, err := x509.ParseCertificate(asn1Data)
			if err != nil {
				return errors.New("failed to parse certificate from server: " + err.Error())
			}
			certs[i] = cert
		}
		opts := x509.VerifyOptions{
			Roots:         c.RootCAs,
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
		_, err := certs[0].Verify(opts)
		if err != nil {
			return err
		}
		return nil
	}
}

// defaultALPNProtocols is the default set of ALPN protocols for SIP over TLS.
var defaultALPNProtocols = []string{"sip"}

// tlsALPNProtocols returns the ALPN protocols to use for TLS negotiation.
// If alpn is nil (not configured), it returns the default ["sip"].
// If alpn is non-nil, it returns the configured value (which may be empty to disable ALPN).
func tlsALPNProtocols(alpn []string) []string {
	if alpn == nil {
		return defaultALPNProtocols
	}
	return alpn
}
