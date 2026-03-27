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
	"time"

	"github.com/livekit/protocol/logger"
)

func newTestDynConfig() *DynamicConfig {
	return NewDynamicConfig(logger.GetLogger())
}

// --- Defaults ---

func TestDynamicConfig_Defaults(t *testing.T) {
	dc := newTestDynConfig()
	if dc.MaxBitrate() != 1_500_000 {
		t.Errorf("expected default max_bitrate 1500000, got %d", dc.MaxBitrate())
	}
	if dc.MaxConcurrent() != 10 {
		t.Errorf("expected default max_concurrent 10, got %d", dc.MaxConcurrent())
	}
	if dc.MediaTimeout() != 15*time.Second {
		t.Errorf("expected default media_timeout 15s, got %s", dc.MediaTimeout())
	}
	if !dc.TranscodeEnabled() {
		t.Error("transcode should be enabled by default")
	}
	if dc.GPUEnabled() {
		t.Error("GPU should be disabled by default")
	}
	if dc.VideoCodec() != "h264" {
		t.Errorf("expected default video_codec h264, got %s", dc.VideoCodec())
	}
	if dc.KeyframeInterval() != 2*time.Second {
		t.Errorf("expected default keyframe_interval 2s, got %s", dc.KeyframeInterval())
	}
	if dc.JitterLatency() != 80*time.Millisecond {
		t.Errorf("expected default jitter_latency 80ms, got %s", dc.JitterLatency())
	}
	if dc.IdleTimeout() != 30*time.Second {
		t.Errorf("expected default idle_timeout 30s, got %s", dc.IdleTimeout())
	}
	if dc.MaxDuration() != 2*time.Hour {
		t.Errorf("expected default max_duration 2h, got %s", dc.MaxDuration())
	}
}

// --- Set with validation ---

func TestDynamicConfig_SetMaxBitrate(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMaxBitrate(2_000_000, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.MaxBitrate() != 2_000_000 {
		t.Errorf("expected 2000000, got %d", dc.MaxBitrate())
	}
}

func TestDynamicConfig_SetMaxBitrate_OutOfRange(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMaxBitrate(50, "test"); err == nil {
		t.Error("expected error for too-low bitrate")
	}
	if err := dc.SetMaxBitrate(100_000_000, "test"); err == nil {
		t.Error("expected error for too-high bitrate")
	}
	// Value should not have changed
	if dc.MaxBitrate() != 1_500_000 {
		t.Errorf("value should not have changed, got %d", dc.MaxBitrate())
	}
}

func TestDynamicConfig_SetMaxConcurrent(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMaxConcurrent(20, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.MaxConcurrent() != 20 {
		t.Errorf("expected 20, got %d", dc.MaxConcurrent())
	}
}

func TestDynamicConfig_SetMaxConcurrent_OutOfRange(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMaxConcurrent(0, "test"); err == nil {
		t.Error("expected error for 0")
	}
	if err := dc.SetMaxConcurrent(5000, "test"); err == nil {
		t.Error("expected error for 5000")
	}
}

func TestDynamicConfig_SetMediaTimeout(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMediaTimeout(30*time.Second, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.MediaTimeout() != 30*time.Second {
		t.Errorf("expected 30s, got %s", dc.MediaTimeout())
	}
}

func TestDynamicConfig_SetMediaTimeout_OutOfRange(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMediaTimeout(100*time.Millisecond, "test"); err == nil {
		t.Error("expected error for 100ms")
	}
	if err := dc.SetMediaTimeout(10*time.Minute, "test"); err == nil {
		t.Error("expected error for 10m")
	}
}

func TestDynamicConfig_SetTranscodeEnabled(t *testing.T) {
	dc := newTestDynConfig()
	dc.SetTranscodeEnabled(false, "test")
	if dc.TranscodeEnabled() {
		t.Error("transcode should be disabled")
	}
	dc.SetTranscodeEnabled(true, "test")
	if !dc.TranscodeEnabled() {
		t.Error("transcode should be re-enabled")
	}
}

func TestDynamicConfig_SetGPUEnabled(t *testing.T) {
	dc := newTestDynConfig()
	dc.SetGPUEnabled(true, "test")
	if !dc.GPUEnabled() {
		t.Error("GPU should be enabled")
	}
}

func TestDynamicConfig_SetVideoCodec(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetVideoCodec("vp8", "test"); err != nil {
		t.Fatal(err)
	}
	if dc.VideoCodec() != "vp8" {
		t.Errorf("expected vp8, got %s", dc.VideoCodec())
	}
}

func TestDynamicConfig_SetVideoCodec_Invalid(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetVideoCodec("av1", "test"); err == nil {
		t.Error("expected error for unsupported codec")
	}
	if dc.VideoCodec() != "h264" {
		t.Error("codec should not have changed")
	}
}

func TestDynamicConfig_SetKeyframeInterval(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetKeyframeInterval(5*time.Second, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.KeyframeInterval() != 5*time.Second {
		t.Errorf("expected 5s, got %s", dc.KeyframeInterval())
	}
}

func TestDynamicConfig_SetKeyframeInterval_OutOfRange(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetKeyframeInterval(100*time.Millisecond, "test"); err == nil {
		t.Error("expected error for 100ms")
	}
	if err := dc.SetKeyframeInterval(time.Minute, "test"); err == nil {
		t.Error("expected error for 1m")
	}
}

func TestDynamicConfig_SetJitterLatency(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetJitterLatency(150*time.Millisecond, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.JitterLatency() != 150*time.Millisecond {
		t.Errorf("expected 150ms, got %s", dc.JitterLatency())
	}
}

func TestDynamicConfig_SetJitterLatency_OutOfRange(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetJitterLatency(time.Millisecond, "test"); err == nil {
		t.Error("expected error for 1ms")
	}
	if err := dc.SetJitterLatency(5*time.Second, "test"); err == nil {
		t.Error("expected error for 5s")
	}
}

func TestDynamicConfig_SetIdleTimeout(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetIdleTimeout(time.Minute, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.IdleTimeout() != time.Minute {
		t.Errorf("expected 1m, got %s", dc.IdleTimeout())
	}
}

func TestDynamicConfig_SetMaxDuration(t *testing.T) {
	dc := newTestDynConfig()
	if err := dc.SetMaxDuration(4*time.Hour, "test"); err != nil {
		t.Fatal(err)
	}
	if dc.MaxDuration() != 4*time.Hour {
		t.Errorf("expected 4h, got %s", dc.MaxDuration())
	}
}

// --- Bulk Apply ---

func TestDynamicConfig_Apply(t *testing.T) {
	dc := newTestDynConfig()
	bitrate := int64(3_000_000)
	concurrent := int32(25)
	codec := "vp8"
	update := DynamicConfigUpdate{
		MaxBitrate:    &bitrate,
		MaxConcurrent: &concurrent,
		VideoCodec:    &codec,
	}
	errs := dc.Apply(update, "test")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if dc.MaxBitrate() != 3_000_000 {
		t.Errorf("expected 3000000, got %d", dc.MaxBitrate())
	}
	if dc.MaxConcurrent() != 25 {
		t.Errorf("expected 25, got %d", dc.MaxConcurrent())
	}
	if dc.VideoCodec() != "vp8" {
		t.Errorf("expected vp8, got %s", dc.VideoCodec())
	}
}

func TestDynamicConfig_Apply_PartialFailure(t *testing.T) {
	dc := newTestDynConfig()
	badBitrate := int64(1) // too low
	goodConcurrent := int32(50)
	update := DynamicConfigUpdate{
		MaxBitrate:    &badBitrate,
		MaxConcurrent: &goodConcurrent,
	}
	errs := dc.Apply(update, "test")
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	// MaxConcurrent should still be applied
	if dc.MaxConcurrent() != 50 {
		t.Errorf("valid field should still be applied, got %d", dc.MaxConcurrent())
	}
	// MaxBitrate should not have changed
	if dc.MaxBitrate() != 1_500_000 {
		t.Errorf("invalid field should not change, got %d", dc.MaxBitrate())
	}
}

func TestDynamicConfig_Apply_NilFieldsIgnored(t *testing.T) {
	dc := newTestDynConfig()
	errs := dc.Apply(DynamicConfigUpdate{}, "test")
	if len(errs) != 0 {
		t.Fatalf("empty update should have no errors: %v", errs)
	}
	// Nothing should change
	if dc.MaxBitrate() != 1_500_000 {
		t.Error("nothing should have changed")
	}
}

// --- Snapshot ---

func TestDynamicConfig_Snapshot(t *testing.T) {
	dc := newTestDynConfig()
	snap := dc.Snapshot()
	if snap.MaxBitrate != 1_500_000 {
		t.Errorf("expected 1500000, got %d", snap.MaxBitrate)
	}
	if snap.VideoCodec != "h264" {
		t.Errorf("expected h264, got %s", snap.VideoCodec)
	}
}

func TestDynamicConfig_MarshalJSON(t *testing.T) {
	dc := newTestDynConfig()
	data, err := json.Marshal(dc)
	if err != nil {
		t.Fatal(err)
	}
	var snap DynamicConfigSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.MaxBitrate != 1_500_000 {
		t.Errorf("expected 1500000, got %d", snap.MaxBitrate)
	}
}

// --- Change tracking ---

func TestDynamicConfig_ChangeTracking(t *testing.T) {
	dc := newTestDynConfig()
	dc.SetMaxBitrate(2_000_000, "test")
	dc.SetMaxConcurrent(20, "test")

	changes := dc.RecentChanges(10)
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	if changes[0].Key != "max_bitrate_bps" {
		t.Errorf("expected first change to be max_bitrate_bps, got %s", changes[0].Key)
	}
	if changes[1].Key != "max_concurrent" {
		t.Errorf("expected second change to be max_concurrent, got %s", changes[1].Key)
	}
	if changes[0].Source != "test" {
		t.Errorf("expected source 'test', got %s", changes[0].Source)
	}
}

func TestDynamicConfig_RecentChanges_Empty(t *testing.T) {
	dc := newTestDynConfig()
	if changes := dc.RecentChanges(10); len(changes) != 0 {
		t.Errorf("expected 0 changes, got %d", len(changes))
	}
}

func TestDynamicConfig_RecentChanges_Limited(t *testing.T) {
	dc := newTestDynConfig()
	for i := 0; i < 10; i++ {
		dc.SetMaxBitrate(int64(1_000_000+i*100_000), "test")
	}
	changes := dc.RecentChanges(3)
	if len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(changes))
	}
}

// --- Change listeners ---

func TestDynamicConfig_OnChange(t *testing.T) {
	dc := newTestDynConfig()
	var received []string
	dc.OnChange(func(key string, value interface{}) {
		received = append(received, key)
	})

	dc.SetMaxBitrate(2_000_000, "test")
	dc.SetTranscodeEnabled(false, "test")

	if len(received) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(received))
	}
	if received[0] != "max_bitrate_bps" {
		t.Errorf("expected max_bitrate_bps, got %s", received[0])
	}
	if received[1] != "transcode_enabled" {
		t.Errorf("expected transcode_enabled, got %s", received[1])
	}
}

func TestDynamicConfig_OnChange_NoChangeNoDuplicate(t *testing.T) {
	dc := newTestDynConfig()
	count := 0
	dc.OnChange(func(key string, value interface{}) {
		count++
	})

	// Setting same value should NOT fire listener for bool fields
	dc.SetTranscodeEnabled(true, "test") // already true
	if count != 0 {
		t.Errorf("should not fire for same value, got %d", count)
	}
}

// --- Concurrency ---

func TestDynamicConfig_ConcurrentAccess(t *testing.T) {
	dc := newTestDynConfig()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			dc.SetMaxBitrate(2_000_000, "test")
		}()
		go func() {
			defer wg.Done()
			_ = dc.MaxBitrate()
		}()
		go func() {
			defer wg.Done()
			dc.SetVideoCodec("vp8", "test")
		}()
		go func() {
			defer wg.Done()
			_ = dc.Snapshot()
		}()
	}
	wg.Wait()
}
