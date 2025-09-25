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
)

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
