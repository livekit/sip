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
	"os"
	"time"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/tlskey"
)

type keyLogFile struct {
	*os.File
	done chan struct{}
}

func openKeyLog(path string) (*keyLogFile, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	k := &keyLogFile{File: f, done: make(chan struct{})}
	go k.syncEvery(30 * time.Second)
	return k, nil
}

func (k *keyLogFile) syncEvery(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = k.File.Sync()
		case <-k.done:
			return
		}
	}
}

func (k *keyLogFile) Close() error {
	close(k.done)
	return k.File.Close()
}

// checkTLS13 adds the configuration fix to tlskey.CheckTLS13: with warn_only, ApplyPolicy leaves max_version unenforced.
func checkTLS13(tconf *config.TLSConfig, c *tls.Config) error {
	err := tlskey.CheckTLS13(c)
	if err == nil {
		return nil
	}
	if tconf.WarnOnly {
		return fmt.Errorf("%w; set tls.max_version: tls1.2 and unset tls.warn_only, which leaves it unenforced", err)
	}
	return fmt.Errorf("%w; set tls.max_version: tls1.2", err)
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
