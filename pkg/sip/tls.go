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

	"github.com/livekit/protocol/logger"
)

func makeTLSCipherMap(CipherSuites []*tls.CipherSuite) map[string]*tls.CipherSuite {
	cipherSuitesMap := make(map[string]*tls.CipherSuite)
	for _, c := range CipherSuites {
		cipherSuitesMap[c.Name] = c
	}
	return cipherSuitesMap
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

// parseCipherSuites parses cipher suite names to uint16 IDs.
// Logs a warning for each insecure cipher suite configured.
func parseCipherSuites(log logger.Logger, suites []string) ([]uint16, error) {
	if len(suites) == 0 {
		return nil, nil
	}

	parsedCipherSuites := []uint16{}
	cipherSuite := makeTLSCipherMap(tls.CipherSuites())
	insecureCipherSuite := makeTLSCipherMap(tls.InsecureCipherSuites())

	for _, suite := range suites {
		if cipher, ok := cipherSuite[suite]; ok {
			parsedCipherSuites = append(parsedCipherSuites, cipher.ID)
		} else if cipher, ok := insecureCipherSuite[suite]; ok {
			parsedCipherSuites = append(parsedCipherSuites, cipher.ID)
			log.Warnw("using insecure TLS cipher suite", nil, "cipherSuite", suite)
		} else {
			return nil, errors.New("unknown cipher suite: " + suite)
		}
	}

	return parsedCipherSuites, nil
}

// parseTLSVersion parses a TLS version string to its uint16 constant.
// Accepts formats: "tls1.0", "tls1.1", "tls1.2", "tls1.3" or "TLS1.0", "TLS1.1", "TLS1.2", "TLS1.3".
func parseTLSVersion(version string) (uint16, error) {
	switch version {
	case "":
		return 0, nil
	case "tls1.0", "TLS1.0":
		return tls.VersionTLS10, nil
	case "tls1.1", "TLS1.1":
		return tls.VersionTLS11, nil
	case "tls1.2", "TLS1.2":
		return tls.VersionTLS12, nil
	case "tls1.3", "TLS1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, errors.New("unknown TLS version: " + version)
	}
}
