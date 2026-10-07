// Copyright 2023 LiveKit, Inc.
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

package config

import (
	"crypto/tls"
	"errors"
	"slices"

	"github.com/livekit/protocol/logger"
)

// ApplyPolicy applies MinVersion, MaxVersion and CipherSuites to c.
//
// If WarnOnly is set, they are not applied. Instead, c keeps Go's defaults and logs a warning
// for each handshake that negotiates a version or cipher suite outside of them.
func (conf *TLSConfig) ApplyPolicy(log logger.Logger, c *tls.Config) error {
	suites, err := parseCipherSuites(log, conf.CipherSuites)
	if err != nil {
		return err
	}
	minVer, err := parseTLSVersion(conf.MinVersion)
	if err != nil {
		return err
	}
	maxVer, err := parseTLSVersion(conf.MaxVersion)
	if err != nil {
		return err
	}
	if !conf.WarnOnly {
		c.CipherSuites = suites
		c.MinVersion = minVer
		c.MaxVersion = maxVer
		return nil
	}
	if len(suites) == 0 && minVer == 0 && maxVer == 0 {
		return nil
	}
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		// NOTE: In warn-only mode, the cipher is negotiated based on Go's
		// default list of cipher suites, and so it's possible for us to log
		// false positve warning messages if the list of cipher suites from the
		// config contains policies that aren't in the default list.
		if !tlsPolicyAllows(cs, suites, minVer, maxVer) {
			log.Warnw("TLS connection outside of the configured policy", nil,
				"serverName", cs.ServerName,
				"version", tls.VersionName(cs.Version),
				"cipherSuite", tls.CipherSuiteName(cs.CipherSuite),
			)
		}
		return nil // only warn, never fail the handshake
	}
	return nil
}

// tlsPolicyAllows reports whether the negotiated version and cipher suite are within the policy.
func tlsPolicyAllows(cs tls.ConnectionState, suites []uint16, minVer, maxVer uint16) bool {
	if minVer != 0 && cs.Version < minVer {
		return false
	}
	if maxVer != 0 && cs.Version > maxVer {
		return false
	}
	// TLS 1.3 cipher suites are not configurable.
	if cs.Version >= tls.VersionTLS13 || len(suites) == 0 {
		return true
	}
	return slices.Contains(suites, cs.CipherSuite)
}

func makeTLSCipherMap(CipherSuites []*tls.CipherSuite) map[string]*tls.CipherSuite {
	cipherSuitesMap := make(map[string]*tls.CipherSuite)
	for _, c := range CipherSuites {
		cipherSuitesMap[c.Name] = c
	}
	return cipherSuitesMap
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
