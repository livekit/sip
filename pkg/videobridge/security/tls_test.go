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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func generateTestCert(t *testing.T, dir string, isCA bool) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Test"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         isCA,
	}
	if !isCA {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
		template.DNSNames = []string{"localhost"}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certFile, _ := os.Create(certPath)
	pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certFile.Close()

	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyFile, _ := os.Create(keyPath)
	pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyFile.Close()

	return certPath, keyPath
}

func TestBuildTLSConfig_Disabled(t *testing.T) {
	cfg := TLSConfig{Enabled: false}
	tlsCfg, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg != nil {
		t.Error("expected nil TLS config when disabled")
	}
}

func TestBuildTLSConfig_MissingCert(t *testing.T) {
	cfg := TLSConfig{Enabled: true}
	_, err := BuildTLSConfig(cfg)
	if err == nil {
		t.Error("expected error when cert_file is missing")
	}
}

func TestBuildTLSConfig_ValidCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, false)

	cfg := TLSConfig{
		Enabled:  true,
		CertFile: certPath,
		KeyFile:  keyPath,
	}

	tlsCfg, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("expected 1 certificate, got %d", len(tlsCfg.Certificates))
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected TLS 1.2 min version")
	}
}

func TestBuildTLSConfig_TLS13(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, false)

	cfg := TLSConfig{
		Enabled:    true,
		CertFile:   certPath,
		KeyFile:    keyPath,
		MinVersion: "1.3",
	}

	tlsCfg, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected TLS 1.3")
	}
}

func TestBuildTLSConfig_mTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, false)

	// Create a CA cert for client verification
	caDir := t.TempDir()
	caPath, _ := generateTestCert(t, caDir, true)

	cfg := TLSConfig{
		Enabled:           true,
		CertFile:          certPath,
		KeyFile:           keyPath,
		ClientCAFile:      caPath,
		RequireClientCert: true,
	}

	tlsCfg, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %v", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Error("expected ClientCAs to be set")
	}
}

func TestBuildTLSConfig_mTLS_Optional(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, false)
	caDir := t.TempDir()
	caPath, _ := generateTestCert(t, caDir, true)

	cfg := TLSConfig{
		Enabled:           true,
		CertFile:          certPath,
		KeyFile:           keyPath,
		ClientCAFile:      caPath,
		RequireClientCert: false,
	}

	tlsCfg, err := BuildTLSConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("expected VerifyClientCertIfGiven, got %v", tlsCfg.ClientAuth)
	}
}

func TestBuildTLSConfig_BadCertPath(t *testing.T) {
	cfg := TLSConfig{
		Enabled:  true,
		CertFile: "/nonexistent/cert.pem",
		KeyFile:  "/nonexistent/key.pem",
	}
	_, err := BuildTLSConfig(cfg)
	if err == nil {
		t.Error("expected error for nonexistent cert files")
	}
}

func TestBuildTLSConfig_BadCAPath(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, false)

	cfg := TLSConfig{
		Enabled:      true,
		CertFile:     certPath,
		KeyFile:      keyPath,
		ClientCAFile: "/nonexistent/ca.pem",
	}
	_, err := BuildTLSConfig(cfg)
	if err == nil {
		t.Error("expected error for nonexistent CA file")
	}
}

func TestParseTLSVersion(t *testing.T) {
	cases := []struct {
		input    string
		expected uint16
	}{
		{"1.3", tls.VersionTLS13},
		{"tls1.3", tls.VersionTLS13},
		{"TLS1.3", tls.VersionTLS13},
		{"1.2", tls.VersionTLS12},
		{"", tls.VersionTLS12},
		{"invalid", tls.VersionTLS12},
	}
	for _, tc := range cases {
		got := parseTLSVersion(tc.input)
		if got != tc.expected {
			t.Errorf("parseTLSVersion(%q) = %d, want %d", tc.input, got, tc.expected)
		}
	}
}
