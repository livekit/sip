// Copyright 2024 LiveKit, Inc.
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

package security

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSConfig holds TLS/mTLS configuration for the admin and health HTTP servers.
type TLSConfig struct {
	// Enabled enables TLS on the health/admin HTTP server.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// CertFile is the path to the server certificate PEM file.
	CertFile string `yaml:"cert_file" json:"cert_file"`
	// KeyFile is the path to the server private key PEM file.
	KeyFile string `yaml:"key_file" json:"key_file"`
	// ClientCAFile is the path to the CA certificate PEM for verifying client certs (mTLS).
	// If empty, client certificates are not required.
	ClientCAFile string `yaml:"client_ca_file" json:"client_ca_file,omitempty"`
	// RequireClientCert enforces mTLS — clients must present a valid certificate.
	RequireClientCert bool `yaml:"require_client_cert" json:"require_client_cert"`
	// MinVersion is the minimum TLS version (default: TLS 1.2).
	MinVersion string `yaml:"min_version" json:"min_version,omitempty"`
}

// BuildTLSConfig creates a *tls.Config from the configuration.
// Returns nil if TLS is not enabled.
func BuildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("TLS enabled but cert_file or key_file not set")
	}

	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS certificate: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   parseTLSVersion(cfg.MinVersion),
	}

	// mTLS: require and verify client certificates
	if cfg.ClientCAFile != "" {
		caCert, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("reading client CA file: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse client CA certificate")
		}
		tlsCfg.ClientCAs = caPool
		if cfg.RequireClientCert {
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}

	return tlsCfg, nil
}

func parseTLSVersion(v string) uint16 {
	switch v {
	case "1.3", "tls1.3", "TLS1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}
