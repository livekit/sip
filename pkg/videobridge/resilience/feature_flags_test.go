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
	"sync"
	"testing"

	"github.com/livekit/protocol/logger"
)

func newTestFlags() *FeatureFlags {
	return NewFeatureFlags(logger.GetLogger())
}

func newTestFlagsRegion(region string) *FeatureFlags {
	return NewFeatureFlagsWithRegion(logger.GetLogger(), region)
}

// --- Global Toggles ---

func TestFeatureFlags_DefaultsAllEnabled(t *testing.T) {
	ff := newTestFlags()
	if !ff.VideoEnabled() {
		t.Error("video should be enabled by default")
	}
	if !ff.AudioEnabled() {
		t.Error("audio should be enabled by default")
	}
	if !ff.TranscodeEnabled() {
		t.Error("transcode should be enabled by default")
	}
	if !ff.Bidirectional() {
		t.Error("bidirectional should be enabled by default")
	}
	if !ff.NewSessionsEnabled() {
		t.Error("new_sessions should be enabled by default")
	}
}

func TestFeatureFlags_SetToggle(t *testing.T) {
	ff := newTestFlags()
	ff.SetVideo(false)
	if ff.VideoEnabled() {
		t.Error("video should be disabled")
	}
	ff.SetVideo(true)
	if !ff.VideoEnabled() {
		t.Error("video should be re-enabled")
	}
}

func TestFeatureFlags_DisableAll(t *testing.T) {
	ff := newTestFlags()
	ff.DisableAll()
	if ff.VideoEnabled() || ff.AudioEnabled() || ff.TranscodeEnabled() || ff.Bidirectional() || ff.NewSessionsEnabled() {
		t.Error("all flags should be disabled after DisableAll")
	}
}

func TestFeatureFlags_EnableAll(t *testing.T) {
	ff := newTestFlags()
	ff.DisableAll()
	ff.EnableAll()
	if !ff.VideoEnabled() || !ff.AudioEnabled() || !ff.TranscodeEnabled() || !ff.Bidirectional() || !ff.NewSessionsEnabled() {
		t.Error("all flags should be enabled after EnableAll")
	}
}

// --- IsDisabled (kill switch pattern) ---

func TestFeatureFlags_IsDisabled(t *testing.T) {
	ff := newTestFlags()
	if ff.IsDisabled("video") {
		t.Error("video should not be disabled by default")
	}
	ff.SetVideo(false)
	if !ff.IsDisabled("video") {
		t.Error("video should be disabled after SetVideo(false)")
	}
	// Unknown flags are never disabled
	if ff.IsDisabled("unknown_flag") {
		t.Error("unknown flags should not be disabled")
	}
}

// --- Snapshot ---

func TestFeatureFlags_Snapshot(t *testing.T) {
	ff := newTestFlags()
	snap := ff.Snapshot()
	if !snap.Video || !snap.Audio || !snap.Transcode || !snap.Bidirectional || !snap.NewSessions {
		t.Error("snapshot should reflect all enabled defaults")
	}
	if snap.Region != "" {
		t.Error("region should be empty for default constructor")
	}
}

func TestFeatureFlags_SnapshotWithRegion(t *testing.T) {
	ff := newTestFlagsRegion("us-east-1")
	snap := ff.Snapshot()
	if snap.Region != "us-east-1" {
		t.Errorf("region should be us-east-1, got %s", snap.Region)
	}
}

func TestFeatureFlags_SnapshotIncludesRules(t *testing.T) {
	ff := newTestFlags()
	ff.AddRule(FlagRule{Flag: "video", Enabled: false, TenantID: "t1"})
	snap := ff.Snapshot()
	if len(snap.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(snap.Rules))
	}
	if snap.Rules[0].TenantID != "t1" {
		t.Error("rule should have tenant t1")
	}
}

func TestFeatureFlags_ApplySnapshot(t *testing.T) {
	ff := newTestFlags()
	ff.ApplySnapshot(FlagSnapshot{
		Video:         false,
		Audio:         true,
		Transcode:     false,
		Bidirectional: true,
		NewSessions:   false,
	})
	if ff.VideoEnabled() {
		t.Error("video should be disabled")
	}
	if !ff.AudioEnabled() {
		t.Error("audio should be enabled")
	}
	if ff.TranscodeEnabled() {
		t.Error("transcode should be disabled")
	}
	if !ff.Bidirectional() {
		t.Error("bidirectional should be enabled")
	}
	if ff.NewSessionsEnabled() {
		t.Error("new_sessions should be disabled")
	}
}

// --- MarshalJSON ---

func TestFeatureFlags_MarshalJSON(t *testing.T) {
	ff := newTestFlags()
	data, err := json.Marshal(ff)
	if err != nil {
		t.Fatal(err)
	}
	var snap FlagSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Video {
		t.Error("video should be true in JSON")
	}
}

// --- Percentage Rollout ---

func TestFeatureFlags_RolloutPercentage(t *testing.T) {
	ff := newTestFlags()
	ff.SetRollout("video", 50)

	// With 50% rollout, some sessions should be enabled and some disabled.
	// Use a large sample to verify distribution.
	enabled := 0
	total := 1000
	for i := 0; i < total; i++ {
		key := "session-" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		if ff.IsEnabledFor("video", key, "") {
			enabled++
		}
	}
	// Should be roughly 50% ±15% for this sample size
	if enabled < 200 || enabled > 800 {
		t.Errorf("expected roughly 50%% enabled, got %d/%d", enabled, total)
	}
}

func TestFeatureFlags_RolloutZero(t *testing.T) {
	ff := newTestFlags()
	ff.SetRollout("video", 0)
	if ff.IsEnabledFor("video", "any-session", "") {
		t.Error("0% rollout should always be disabled")
	}
}

func TestFeatureFlags_RolloutHundred(t *testing.T) {
	ff := newTestFlags()
	ff.SetRollout("video", 100)
	// 100% falls through to global toggle
	if !ff.IsEnabledFor("video", "any-session", "") {
		t.Error("100% rollout should be enabled")
	}
}

func TestFeatureFlags_RolloutClamp(t *testing.T) {
	ff := newTestFlags()
	ff.SetRollout("video", 150)
	rollout := ff.GetRollout()
	if rollout["video"] != 100 {
		t.Errorf("expected clamped to 100, got %d", rollout["video"])
	}
	ff.SetRollout("audio", -10)
	if rollout2 := ff.GetRollout(); rollout2["audio"] != 0 {
		t.Errorf("expected clamped to 0, got %d", rollout2["audio"])
	}
}

func TestFeatureFlags_RolloutConsistentHash(t *testing.T) {
	ff := newTestFlags()
	ff.SetRollout("video", 50)
	key := "stable-session-key"
	result1 := ff.IsEnabledFor("video", key, "")
	result2 := ff.IsEnabledFor("video", key, "")
	if result1 != result2 {
		t.Error("consistent hash should return the same result for same key")
	}
}

// --- Tenant Overrides ---

func TestFeatureFlags_TenantOverride(t *testing.T) {
	ff := newTestFlags()
	ff.SetVideo(true)
	ff.SetTenantOverride("acme", "video", false)

	// ACME tenant has video disabled
	if ff.IsEnabledFor("video", "session-1", "acme") {
		t.Error("video should be disabled for acme tenant")
	}
	// Other tenants use global toggle
	if !ff.IsEnabledFor("video", "session-1", "other") {
		t.Error("video should be enabled for other tenants")
	}
}

func TestFeatureFlags_TenantOverridePriority(t *testing.T) {
	ff := newTestFlags()
	ff.SetRollout("video", 0) // 0% rollout
	ff.SetTenantOverride("vip", "video", true)

	// Tenant override should win over 0% rollout
	if !ff.IsEnabledFor("video", "session-1", "vip") {
		t.Error("tenant override should take priority over rollout")
	}
	// Non-VIP should get 0% rollout
	if ff.IsEnabledFor("video", "session-1", "regular") {
		t.Error("regular tenant should get 0% rollout")
	}
}

func TestFeatureFlags_RemoveTenantOverride(t *testing.T) {
	ff := newTestFlags()
	ff.SetTenantOverride("acme", "video", false)
	ff.RemoveTenantOverride("acme", "video")

	// Should fall through to global (which is enabled)
	if !ff.IsEnabledFor("video", "session-1", "acme") {
		t.Error("after removing override, should fall through to global toggle")
	}
}

// --- Region Overrides ---

func TestFeatureFlags_RegionOverride(t *testing.T) {
	ff := newTestFlagsRegion("eu-west-1")
	ff.SetRegionOverride("eu-west-1", "video", false)

	// This node is in eu-west-1, video should be disabled
	if ff.IsEnabledFor("video", "session-1", "") {
		t.Error("video should be disabled in eu-west-1")
	}
}

func TestFeatureFlags_RegionOverrideNoMatch(t *testing.T) {
	ff := newTestFlagsRegion("us-east-1")
	ff.SetRegionOverride("eu-west-1", "video", false)

	// This node is in us-east-1, eu-west-1 override should not apply
	if !ff.IsEnabledFor("video", "session-1", "") {
		t.Error("video should be enabled in us-east-1 (no matching region override)")
	}
}

func TestFeatureFlags_TenantOverrideBeatsRegion(t *testing.T) {
	ff := newTestFlagsRegion("eu-west-1")
	ff.SetRegionOverride("eu-west-1", "video", false) // region says disabled
	ff.SetTenantOverride("vip", "video", true)         // tenant says enabled

	// Tenant override wins
	if !ff.IsEnabledFor("video", "session-1", "vip") {
		t.Error("tenant override should beat region override")
	}
	// Without tenant, region override applies
	if ff.IsEnabledFor("video", "session-1", "regular") {
		t.Error("without tenant override, region should apply")
	}
}

func TestFeatureFlags_RemoveRegionOverride(t *testing.T) {
	ff := newTestFlagsRegion("eu-west-1")
	ff.SetRegionOverride("eu-west-1", "video", false)
	ff.RemoveRegionOverride("eu-west-1", "video")

	if !ff.IsEnabledFor("video", "session-1", "") {
		t.Error("after removing region override, should fall through to global toggle")
	}
}

// --- FlagRule struct ---

func TestFeatureFlags_AddRule_Tenant(t *testing.T) {
	ff := newTestFlags()
	ff.AddRule(FlagRule{
		Flag:     "video",
		Enabled:  false,
		TenantID: "acme",
	})

	if ff.IsEnabledFor("video", "s1", "acme") {
		t.Error("rule should disable video for acme")
	}
	if !ff.IsEnabledFor("video", "s1", "other") {
		t.Error("other tenants should not be affected")
	}

	rules := ff.Rules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].TenantID != "acme" {
		t.Error("rule should have tenant acme")
	}
}

func TestFeatureFlags_AddRule_Region(t *testing.T) {
	ff := newTestFlagsRegion("ap-south-1")
	ff.AddRule(FlagRule{
		Flag:    "transcode",
		Enabled: false,
		Region:  "ap-south-1",
	})

	if ff.IsEnabledFor("transcode", "s1", "") {
		t.Error("rule should disable transcode in ap-south-1")
	}
}

func TestFeatureFlags_AddRule_Percentage(t *testing.T) {
	ff := newTestFlags()
	ff.AddRule(FlagRule{
		Flag:       "video",
		Enabled:    true,
		Percentage: 50,
	})

	rollout := ff.GetRollout()
	if rollout["video"] != 50 {
		t.Errorf("expected rollout 50, got %d", rollout["video"])
	}
}

func TestFeatureFlags_AddRule_PercentageClamped(t *testing.T) {
	ff := newTestFlags()
	ff.AddRule(FlagRule{Flag: "video", Percentage: 200})
	rollout := ff.GetRollout()
	if rollout["video"] != 100 {
		t.Errorf("expected clamped to 100, got %d", rollout["video"])
	}
}

func TestFeatureFlags_ClearRules(t *testing.T) {
	ff := newTestFlags()
	ff.AddRule(FlagRule{Flag: "video", Enabled: false, TenantID: "acme"})
	ff.AddRule(FlagRule{Flag: "audio", Enabled: false, Region: "eu-west-1"})
	ff.ClearRules()

	if len(ff.Rules()) != 0 {
		t.Error("rules should be empty after ClearRules")
	}
	// Tenant and region overrides should also be cleared
	if ff.IsEnabledFor("video", "s1", "acme") != ff.VideoEnabled() {
		t.Error("tenant overrides should be cleared")
	}
}

// --- Region method ---

func TestFeatureFlags_Region(t *testing.T) {
	ff := newTestFlagsRegion("us-west-2")
	if ff.Region() != "us-west-2" {
		t.Errorf("expected us-west-2, got %s", ff.Region())
	}
}

// --- Unknown flag fallback ---

func TestFeatureFlags_UnknownFlagDefaultsTrue(t *testing.T) {
	ff := newTestFlags()
	if !ff.IsEnabledFor("some_future_flag", "session", "") {
		t.Error("unknown flags should default to true")
	}
}

// --- Concurrency ---

func TestFeatureFlags_ConcurrentAccess(t *testing.T) {
	ff := newTestFlags()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			ff.SetVideo(true)
			ff.SetVideo(false)
		}()
		go func() {
			defer wg.Done()
			ff.IsEnabledFor("video", "session", "tenant")
		}()
		go func() {
			defer wg.Done()
			ff.AddRule(FlagRule{Flag: "video", TenantID: "t1", Enabled: true})
		}()
	}
	wg.Wait()
}

func TestFeatureFlags_ConcurrentRollout(t *testing.T) {
	ff := newTestFlags()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ff.SetRollout("video", 50)
		}()
		go func() {
			defer wg.Done()
			ff.IsEnabledFor("video", "session", "")
		}()
	}
	wg.Wait()
}
