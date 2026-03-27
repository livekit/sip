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
	"fmt"
	"strings"
)

// SRTPConfig holds SRTP enforcement configuration.
type SRTPConfig struct {
	// Enforce requires all incoming SDP to use RTP/SAVP or RTP/SAVPF.
	// When false, both RTP/AVP and RTP/SAVP are accepted.
	Enforce bool `yaml:"enforce" json:"enforce"`
	// AllowedProfiles lists acceptable media profiles.
	// Default: ["RTP/SAVP", "RTP/SAVPF"] when enforce is true.
	AllowedProfiles []string `yaml:"allowed_profiles" json:"allowed_profiles,omitempty"`
}

// SRTPEnforcer validates SDP offers for SRTP compliance.
type SRTPEnforcer struct {
	enforce         bool
	allowedProfiles map[string]bool
}

// NewSRTPEnforcer creates an SRTP enforcer from config.
func NewSRTPEnforcer(cfg SRTPConfig) *SRTPEnforcer {
	profiles := cfg.AllowedProfiles
	if len(profiles) == 0 && cfg.Enforce {
		profiles = []string{"RTP/SAVP", "RTP/SAVPF"}
	}

	allowed := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		allowed[strings.ToUpper(p)] = true
	}

	return &SRTPEnforcer{
		enforce:         cfg.Enforce,
		allowedProfiles: allowed,
	}
}

// ValidateSDP checks SDP content for SRTP compliance.
// It scans m= lines for their transport profile.
// Returns nil if compliant, error if a non-SRTP profile is found and enforcement is on.
func (e *SRTPEnforcer) ValidateSDP(sdpBody string) error {
	if !e.enforce {
		return nil
	}

	lines := strings.Split(sdpBody, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "m=") {
			continue
		}

		profile := extractMediaProfile(line)
		if profile == "" {
			continue
		}

		if !e.allowedProfiles[strings.ToUpper(profile)] {
			return fmt.Errorf("SRTP required: media line uses %q but only %v are allowed",
				profile, e.allowedProfilesList())
		}
	}

	return nil
}

// IsEnforcing returns true if SRTP enforcement is active.
func (e *SRTPEnforcer) IsEnforcing() bool {
	return e.enforce
}

// extractMediaProfile parses the transport profile from an SDP m= line.
// Format: m=<media> <port> <proto> <fmt> ...
// Example: m=video 49170 RTP/SAVP 96 → returns "RTP/SAVP"
func extractMediaProfile(mLine string) string {
	// Remove "m=" prefix
	rest := strings.TrimPrefix(mLine, "m=")
	parts := strings.Fields(rest)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func (e *SRTPEnforcer) allowedProfilesList() []string {
	out := make([]string, 0, len(e.allowedProfiles))
	for p := range e.allowedProfiles {
		out = append(out, p)
	}
	return out
}
