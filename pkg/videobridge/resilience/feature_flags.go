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

package resilience

import (
	"encoding/json"
	"hash/fnv"
	"sync"
	"sync/atomic"

	"github.com/livekit/protocol/logger"
)

// FlagRule defines a granular rule for a feature flag.
// Rules are evaluated in priority order: tenant override > region > percentage > global.
type FlagRule struct {
	Flag       string `json:"flag"`
	Enabled    bool   `json:"enabled"`
	TenantID   string `json:"tenant_id,omitempty"`  // if set, applies only to this tenant
	Region     string `json:"region,omitempty"`     // if set, applies only to this region
	Percentage int    `json:"percentage,omitempty"` // 0-100, evaluated via consistent hash
}

// FeatureFlags provides runtime toggles for video bridge capabilities.
// Supports: boolean on/off, percentage rollout, tenant-based overrides, and region targeting.
// All reads are lock-free via atomic operations for global toggles.
type FeatureFlags struct {
	log    logger.Logger
	region string // node's deployment region

	videoEnabled       atomic.Bool
	audioEnabled       atomic.Bool
	transcodeEnabled   atomic.Bool
	bidirectional      atomic.Bool
	newSessionsEnabled atomic.Bool

	mu              sync.RWMutex
	rollout         map[string]int             // flag name → percentage (0-100)
	tenantOverrides map[string]map[string]bool // tenant → flag name → enabled
	regionOverrides map[string]map[string]bool // region → flag name → enabled
	rules           []FlagRule                 // ordered rules for structured evaluation
}

// NewFeatureFlags creates feature flags with everything enabled by default.
func NewFeatureFlags(log logger.Logger) *FeatureFlags {
	return NewFeatureFlagsWithRegion(log, "")
}

// NewFeatureFlagsWithRegion creates feature flags with region awareness.
func NewFeatureFlagsWithRegion(log logger.Logger, region string) *FeatureFlags {
	ff := &FeatureFlags{
		log:             log,
		region:          region,
		rollout:         make(map[string]int),
		tenantOverrides: make(map[string]map[string]bool),
		regionOverrides: make(map[string]map[string]bool),
	}
	ff.videoEnabled.Store(true)
	ff.audioEnabled.Store(true)
	ff.transcodeEnabled.Store(true)
	ff.bidirectional.Store(true)
	ff.newSessionsEnabled.Store(true)
	return ff
}

// --- Read flags (hot path, lock-free) ---

func (ff *FeatureFlags) VideoEnabled() bool       { return ff.videoEnabled.Load() }
func (ff *FeatureFlags) AudioEnabled() bool       { return ff.audioEnabled.Load() }
func (ff *FeatureFlags) TranscodeEnabled() bool   { return ff.transcodeEnabled.Load() }
func (ff *FeatureFlags) Bidirectional() bool      { return ff.bidirectional.Load() }
func (ff *FeatureFlags) NewSessionsEnabled() bool { return ff.newSessionsEnabled.Load() }

// --- Write flags (control path) ---

func (ff *FeatureFlags) SetVideo(enabled bool) {
	old := ff.videoEnabled.Swap(enabled)
	if old != enabled {
		ff.log.Infow("feature flag changed", "flag", "video", "enabled", enabled)
	}
}

func (ff *FeatureFlags) SetAudio(enabled bool) {
	old := ff.audioEnabled.Swap(enabled)
	if old != enabled {
		ff.log.Infow("feature flag changed", "flag", "audio", "enabled", enabled)
	}
}

func (ff *FeatureFlags) SetTranscode(enabled bool) {
	old := ff.transcodeEnabled.Swap(enabled)
	if old != enabled {
		ff.log.Infow("feature flag changed", "flag", "transcode", "enabled", enabled)
	}
}

func (ff *FeatureFlags) SetBidirectional(enabled bool) {
	old := ff.bidirectional.Swap(enabled)
	if old != enabled {
		ff.log.Infow("feature flag changed", "flag", "bidirectional", "enabled", enabled)
	}
}

func (ff *FeatureFlags) SetNewSessions(enabled bool) {
	old := ff.newSessionsEnabled.Swap(enabled)
	if old != enabled {
		ff.log.Infow("feature flag changed", "flag", "new_sessions", "enabled", enabled)
	}
}

// DisableAll is an emergency kill switch — stops all media processing.
func (ff *FeatureFlags) DisableAll() {
	ff.videoEnabled.Store(false)
	ff.audioEnabled.Store(false)
	ff.transcodeEnabled.Store(false)
	ff.bidirectional.Store(false)
	ff.newSessionsEnabled.Store(false)
	ff.log.Warnw("ALL feature flags disabled (emergency kill switch)", nil)
}

// EnableAll restores all feature flags to enabled.
func (ff *FeatureFlags) EnableAll() {
	ff.videoEnabled.Store(true)
	ff.audioEnabled.Store(true)
	ff.transcodeEnabled.Store(true)
	ff.bidirectional.Store(true)
	ff.newSessionsEnabled.Store(true)
	ff.log.Infow("all feature flags restored to enabled")
}

// IsDisabled returns true if a named feature is globally disabled.
// This is the recommended pattern for kill-switch checks:
//
//	if flags.IsDisabled("video") { fallbackToAudio() }
func (ff *FeatureFlags) IsDisabled(flag string) bool {
	switch flag {
	case "video":
		return !ff.videoEnabled.Load()
	case "audio":
		return !ff.audioEnabled.Load()
	case "transcode":
		return !ff.transcodeEnabled.Load()
	case "bidirectional":
		return !ff.bidirectional.Load()
	case "new_sessions":
		return !ff.newSessionsEnabled.Load()
	default:
		return false
	}
}

// Snapshot returns the current state of all flags as a JSON-serializable struct.
func (ff *FeatureFlags) Snapshot() FlagSnapshot {
	ff.mu.RLock()
	rules := make([]FlagRule, len(ff.rules))
	copy(rules, ff.rules)
	ff.mu.RUnlock()

	return FlagSnapshot{
		Video:         ff.videoEnabled.Load(),
		Audio:         ff.audioEnabled.Load(),
		Transcode:     ff.transcodeEnabled.Load(),
		Bidirectional: ff.bidirectional.Load(),
		NewSessions:   ff.newSessionsEnabled.Load(),
		Region:        ff.region,
		Rules:         rules,
	}
}

// ApplySnapshot applies a set of flags from a snapshot (e.g., from API request).
func (ff *FeatureFlags) ApplySnapshot(snap FlagSnapshot) {
	ff.SetVideo(snap.Video)
	ff.SetAudio(snap.Audio)
	ff.SetTranscode(snap.Transcode)
	ff.SetBidirectional(snap.Bidirectional)
	ff.SetNewSessions(snap.NewSessions)
}

// FlagSnapshot holds the state of all feature flags.
type FlagSnapshot struct {
	Video         bool       `json:"video"`
	Audio         bool       `json:"audio"`
	Transcode     bool       `json:"transcode"`
	Bidirectional bool       `json:"bidirectional"`
	NewSessions   bool       `json:"new_sessions"`
	Region        string     `json:"region,omitempty"`
	Rules         []FlagRule `json:"rules,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (ff *FeatureFlags) MarshalJSON() ([]byte, error) {
	return json.Marshal(ff.Snapshot())
}

// --- Percentage Rollout ---

// SetRollout sets the rollout percentage (0-100) for a flag.
// When percentage < 100, the flag is evaluated per-session using consistent hashing.
func (ff *FeatureFlags) SetRollout(flag string, percent int) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	ff.mu.Lock()
	ff.rollout[flag] = percent
	ff.mu.Unlock()
	ff.log.Infow("rollout percentage set", "flag", flag, "percent", percent)
}

// IsEnabledFor checks if a flag is enabled for a specific session/caller.
// Evaluation priority: tenant override > region override > percentage rollout > global toggle.
// sessionKey should be a stable identifier (e.g., callID or callerURI).
func (ff *FeatureFlags) IsEnabledFor(flag string, sessionKey string, tenant string) bool {
	ff.mu.RLock()

	// 1. Check tenant override first (highest priority)
	if overrides, ok := ff.tenantOverrides[tenant]; ok {
		if enabled, exists := overrides[flag]; exists {
			ff.mu.RUnlock()
			return enabled
		}
	}

	// 2. Check region override (node's region)
	if ff.region != "" {
		if overrides, ok := ff.regionOverrides[ff.region]; ok {
			if enabled, exists := overrides[flag]; exists {
				ff.mu.RUnlock()
				return enabled
			}
		}
	}

	// 3. Check percentage rollout
	percent, hasRollout := ff.rollout[flag]
	ff.mu.RUnlock()

	if hasRollout && percent < 100 {
		if percent <= 0 {
			return false
		}
		// Consistent hash: same sessionKey always gets the same result
		hash := hashString(sessionKey)
		return (hash % 100) < uint32(percent)
	}

	// 4. Fall back to global boolean toggle
	switch flag {
	case "video":
		return ff.videoEnabled.Load()
	case "audio":
		return ff.audioEnabled.Load()
	case "transcode":
		return ff.transcodeEnabled.Load()
	case "bidirectional":
		return ff.bidirectional.Load()
	case "new_sessions":
		return ff.newSessionsEnabled.Load()
	default:
		return true
	}
}

// --- Tenant Overrides ---

// SetTenantOverride sets a flag override for a specific tenant.
// Tenant overrides take priority over percentage rollout and global toggles.
func (ff *FeatureFlags) SetTenantOverride(tenant, flag string, enabled bool) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if ff.tenantOverrides[tenant] == nil {
		ff.tenantOverrides[tenant] = make(map[string]bool)
	}
	ff.tenantOverrides[tenant][flag] = enabled
	ff.log.Infow("tenant override set", "tenant", tenant, "flag", flag, "enabled", enabled)
}

// RemoveTenantOverride removes a flag override for a tenant.
func (ff *FeatureFlags) RemoveTenantOverride(tenant, flag string) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if overrides, ok := ff.tenantOverrides[tenant]; ok {
		delete(overrides, flag)
		if len(overrides) == 0 {
			delete(ff.tenantOverrides, tenant)
		}
	}
}

// --- Region Overrides ---

// SetRegionOverride sets a flag override for a specific region.
// Region overrides take priority over percentage rollout but not tenant overrides.
func (ff *FeatureFlags) SetRegionOverride(region, flag string, enabled bool) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if ff.regionOverrides[region] == nil {
		ff.regionOverrides[region] = make(map[string]bool)
	}
	ff.regionOverrides[region][flag] = enabled
	ff.log.Infow("region override set", "region", region, "flag", flag, "enabled", enabled)
}

// RemoveRegionOverride removes a flag override for a region.
func (ff *FeatureFlags) RemoveRegionOverride(region, flag string) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if overrides, ok := ff.regionOverrides[region]; ok {
		delete(overrides, flag)
		if len(overrides) == 0 {
			delete(ff.regionOverrides, region)
		}
	}
}

// --- Structured Rules ---

// AddRule adds a FlagRule. Rules with tenant or region constraints are evaluated
// during IsEnabledFor. Rules are stored in insertion order.
func (ff *FeatureFlags) AddRule(rule FlagRule) {
	ff.mu.Lock()
	defer ff.mu.Unlock()

	// Normalize percentage
	if rule.Percentage < 0 {
		rule.Percentage = 0
	}
	if rule.Percentage > 100 {
		rule.Percentage = 100
	}

	// Apply side effects for simpler overrides
	if rule.TenantID != "" {
		if ff.tenantOverrides[rule.TenantID] == nil {
			ff.tenantOverrides[rule.TenantID] = make(map[string]bool)
		}
		ff.tenantOverrides[rule.TenantID][rule.Flag] = rule.Enabled
	}
	if rule.Region != "" {
		if ff.regionOverrides[rule.Region] == nil {
			ff.regionOverrides[rule.Region] = make(map[string]bool)
		}
		ff.regionOverrides[rule.Region][rule.Flag] = rule.Enabled
	}
	if rule.Percentage > 0 && rule.TenantID == "" && rule.Region == "" {
		ff.rollout[rule.Flag] = rule.Percentage
	}

	ff.rules = append(ff.rules, rule)
	ff.log.Infow("flag rule added",
		"flag", rule.Flag, "enabled", rule.Enabled,
		"tenant", rule.TenantID, "region", rule.Region, "percentage", rule.Percentage)
}

// ClearRules removes all structured rules (but keeps global toggles).
func (ff *FeatureFlags) ClearRules() {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	ff.rules = nil
	ff.tenantOverrides = make(map[string]map[string]bool)
	ff.regionOverrides = make(map[string]map[string]bool)
	ff.rollout = make(map[string]int)
	ff.log.Infow("all flag rules cleared")
}

// Rules returns a copy of the current rule set.
func (ff *FeatureFlags) Rules() []FlagRule {
	ff.mu.RLock()
	defer ff.mu.RUnlock()
	out := make([]FlagRule, len(ff.rules))
	copy(out, ff.rules)
	return out
}

// GetRollout returns the current rollout configuration.
func (ff *FeatureFlags) GetRollout() map[string]int {
	ff.mu.RLock()
	defer ff.mu.RUnlock()
	out := make(map[string]int, len(ff.rollout))
	for k, v := range ff.rollout {
		out[k] = v
	}
	return out
}

// Region returns the configured region for this node.
func (ff *FeatureFlags) Region() string {
	return ff.region
}

func hashString(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
