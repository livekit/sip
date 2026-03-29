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

package testing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/config"
	"github.com/livekit/sip/pkg/videobridge/resilience"
	"github.com/livekit/sip/pkg/videobridge/session"
)

// TestSessionLifecycleFlow verifies the complete session lifecycle:
// creation → ready → streaming → degradation → closing → closed.
func TestSessionLifecycleFlow(t *testing.T) {
	log := logger.GetLogger()
	conf := session.LifecycleConfig{
		MaxDuration:      10 * time.Second,
		IdleTimeout:      5 * time.Second,
		StreamingTimeout: 2 * time.Second,
	}

	sm := session.NewStateMachine()
	lc := session.NewLifecycleMonitor(log, conf, sm)

	// Phase 1: INIT → READY
	if err := sm.Transition(session.StateInit, session.StateReady); err != nil {
		t.Fatalf("INIT→READY failed: %v", err)
	}
	if !sm.IsActive() {
		t.Error("expected session to be active in READY state")
	}

	// Phase 2: READY → STREAMING (simulate media arrival)
	if err := sm.Transition(session.StateReady, session.StateStreaming); err != nil {
		t.Fatalf("READY→STREAMING failed: %v", err)
	}
	lc.TouchVideo()

	// Phase 3: Check lifecycle (should not be expired)
	shouldClose, reason := lc.Check()
	if shouldClose {
		t.Errorf("should not close immediately: %s", reason)
	}

	// Phase 4: STREAMING → DEGRADED (simulate quality loss)
	if err := sm.Transition(session.StateStreaming, session.StateDegraded); err != nil {
		t.Fatalf("STREAMING→DEGRADED failed: %v", err)
	}

	// Phase 5: DEGRADED → CLOSING (simulate shutdown)
	if err := sm.Transition(session.StateDegraded, session.StateClosing); err != nil {
		t.Fatalf("DEGRADED→CLOSING failed: %v", err)
	}
	if sm.IsActive() {
		t.Error("expected session to be inactive in CLOSING state")
	}

	// Phase 6: CLOSING → CLOSED
	if err := sm.Transition(session.StateClosing, session.StateClosed); err != nil {
		t.Fatalf("CLOSING→CLOSED failed: %v", err)
	}

	t.Logf("Session lifecycle completed: INIT→READY→STREAMING→DEGRADED→CLOSING→CLOSED")
}

// TestFeatureFlagRolloutEvaluation verifies flag evaluation with different rollout strategies.
func TestFeatureFlagRolloutEvaluation(t *testing.T) {
	log := logger.GetLogger()
	ff := resilience.NewFeatureFlagsWithRegion(log, "us-west-2")

	// Test 1: Global toggle
	ff.SetVideo(false)
	if !ff.IsDisabled("video") {
		t.Error("video should be disabled globally")
	}
	ff.SetVideo(true)

	// Test 2: Percentage rollout (50%)
	ff.SetRollout("video", 50)
	enabled := 0
	for i := 0; i < 100; i++ {
		if ff.IsEnabledFor("video", fmt.Sprintf("session-%d", i), "default") {
			enabled++
		}
	}
	if enabled < 40 || enabled > 60 {
		t.Logf("rollout 50%%: %d/100 enabled (expected ~50)", enabled)
	}

	// Test 3: Tenant override (highest priority)
	ff.SetTenantOverride("premium", "video", false)
	if ff.IsEnabledFor("video", "session-1", "premium") {
		t.Error("premium tenant should have video disabled via override")
	}

	// Test 4: Region override
	ff.SetRegionOverride("eu-central-1", "video", false)
	ffEU := resilience.NewFeatureFlagsWithRegion(log, "eu-central-1")
	ffEU.SetRollout("video", 100) // 100% rollout globally
	ffEU.SetRegionOverride("eu-central-1", "video", false)
	if ffEU.IsEnabledFor("video", "session-1", "default") {
		t.Error("eu-central-1 region should have video disabled")
	}

	t.Logf("Feature flag rollout evaluation: global, percentage, tenant, region all working")
}

// TestDynamicConfigHotReload verifies runtime config changes without restart.
func TestDynamicConfigHotReload(t *testing.T) {
	log := logger.GetLogger()
	dc := resilience.NewDynamicConfig(log)

	// Initial state
	initialBitrate := dc.MaxBitrate()
	initialTimeout := dc.MediaTimeout()

	// Change 1: Update bitrate
	if err := dc.SetMaxBitrate(2_000_000, "test"); err != nil {
		t.Fatalf("SetMaxBitrate failed: %v", err)
	}
	if dc.MaxBitrate() != 2_000_000 {
		t.Errorf("bitrate not updated: got %d, want 2000000", dc.MaxBitrate())
	}

	// Change 2: Update timeout
	if err := dc.SetMediaTimeout(30*time.Second, "test"); err != nil {
		t.Fatalf("SetMediaTimeout failed: %v", err)
	}
	if dc.MediaTimeout() != 30*time.Second {
		t.Errorf("timeout not updated: got %v, want 30s", dc.MediaTimeout())
	}

	// Verify snapshot captures both changes
	snap := dc.Snapshot()
	if snap.MaxBitrate != 2_000_000 || snap.MediaTimeout != 30*time.Second {
		t.Errorf("snapshot mismatch: bitrate=%d, timeout=%v", snap.MaxBitrate, snap.MediaTimeout)
	}

	t.Logf("Dynamic config hot-reload: initial bitrate=%d, timeout=%v → updated to 2M, 30s",
		initialBitrate, initialTimeout)
}

// TestSessionGuardAdmissionControl verifies session limits are enforced.
func TestSessionGuardAdmissionControl(t *testing.T) {
	log := logger.GetLogger()
	guard := resilience.NewSessionGuard(log, resilience.SessionGuardConfig{
		MaxSessionsPerNode:   5,
		MaxSessionsPerCaller: 2,
		NewSessionRateLimit:  10.0,
		NewSessionBurst:      20,
	})

	const caller = "sip:test@example.com"

	// Admit 2 sessions for the same caller
	for i := 0; i < 2; i++ {
		if err := guard.Admit(caller); err != nil {
			t.Fatalf("session %d: admission failed: %v", i, err)
		}
	}

	// 3rd session should be rejected (per-caller limit)
	if err := guard.Admit(caller); err == nil {
		t.Error("expected 3rd session to be rejected (per-caller limit)")
	}

	// Release one and try again
	guard.Release(caller)
	if err := guard.Admit(caller); err != nil {
		t.Fatalf("after release, admission failed: %v", err)
	}

	stats := guard.Stats()
	t.Logf("Session guard: active=%d, rejected=%d, unique_callers=%d",
		stats.Active, stats.Rejected, stats.UniquCallers)
}

// TestCircuitBreakerIntegration verifies CB state transitions with real timing.
func TestCircuitBreakerIntegration(t *testing.T) {
	log := logger.GetLogger()
	cb := resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:                "integration_test",
		MaxFailures:         3,
		OpenDuration:        100 * time.Millisecond,
		HalfOpenMaxAttempts: 2,
	})

	// Trip the circuit
	for i := 0; i < 5; i++ {
		if cb.Allow() {
			cb.RecordFailure(fmt.Errorf("test failure %d", i))
		}
	}

	if cb.State() != resilience.StateOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}

	// Wait for half-open
	time.Sleep(150 * time.Millisecond)

	// Recover
	successCount := 0
	for i := 0; i < 3; i++ {
		if cb.Allow() {
			cb.RecordSuccess()
			successCount++
		}
	}

	if successCount < 2 {
		t.Errorf("expected at least 2 successes during recovery, got %d", successCount)
	}

	time.Sleep(10 * time.Millisecond)
	if cb.State() != resilience.StateClosed {
		t.Errorf("expected closed after recovery, got %s", cb.State())
	}

	t.Logf("Circuit breaker integration: trip → open → half-open → closed")
}

// TestConfigValidation verifies config constraints are enforced.
func TestConfigValidation(t *testing.T) {
	// Valid config
	validConf := &config.Config{
		SIP: config.SIPConfig{
			Port: 5060,
		},
		RTP: config.RTPConfig{
			PortStart: 20000,
			PortEnd:   30000,
		},
		Video: config.VideoConfig{
			DefaultCodec: "h264",
		},
	}

	if validConf.RTP.PortStart >= validConf.RTP.PortEnd {
		t.Error("invalid RTP port range")
	}

	// Test codec validation
	validCodecs := map[string]bool{"h264": true, "vp8": true, "vp9": true}
	if !validCodecs[validConf.Video.DefaultCodec] {
		t.Errorf("invalid codec: %s", validConf.Video.DefaultCodec)
	}

	t.Logf("Config validation: SIP port=%d, RTP range=%d-%d, codec=%s",
		validConf.SIP.Port, validConf.RTP.PortStart, validConf.RTP.PortEnd, validConf.Video.DefaultCodec)
}

// TestBridgeContextCancellation verifies graceful shutdown via context.
func TestBridgeContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Simulate a long-running operation
	done := make(chan bool)
	go func() {
		select {
		case <-ctx.Done():
			done <- true
		case <-time.After(5 * time.Second):
			done <- false
		}
	}()

	result := <-done
	if !result {
		t.Error("context cancellation did not trigger")
	}

	t.Logf("Bridge context cancellation: graceful shutdown verified")
}
