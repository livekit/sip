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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"
)

// DynamicConfig holds runtime-tunable configuration values that can be
// changed via API without restarting the process. Immutable config (ports,
// credentials) is NOT included — only operational parameters.
//
// Read path: lock-free atomics for hot-path values.
// Write path: mutex-protected with validation and change notification.
type DynamicConfig struct {
	log logger.Logger

	// Hot-path values (lock-free reads)
	maxBitrate       atomic.Int64  // video max bitrate (bps)
	maxConcurrent    atomic.Int32  // max concurrent transcode sessions
	mediaTimeout     atomic.Int64  // media timeout (nanoseconds)
	transcodeEnabled atomic.Bool   // transcode on/off
	gpuEnabled       atomic.Bool   // GPU acceleration on/off

	// Cold-path values (mutex-protected)
	mu             sync.RWMutex
	videoCodec     string        // "h264" or "vp8"
	keyframeIvl    time.Duration // target keyframe interval
	jitterLatency  time.Duration // jitter buffer latency
	idleTimeout    time.Duration // session idle timeout
	maxDuration    time.Duration // max session duration

	// Change listeners
	listeners []func(key string, value interface{})

	// Change history
	changes    []ConfigChange
	changesCap int
}

// ConfigChange records a single configuration change.
type ConfigChange struct {
	Timestamp time.Time   `json:"ts"`
	Key       string      `json:"key"`
	OldValue  interface{} `json:"old_value"`
	NewValue  interface{} `json:"new_value"`
	Source    string      `json:"source"` // "api", "circuit_breaker", "auto"
}

// DynamicConfigSnapshot is the JSON-serializable view of all dynamic config.
type DynamicConfigSnapshot struct {
	MaxBitrate       int64         `json:"max_bitrate_bps"`
	MaxConcurrent    int32         `json:"max_concurrent"`
	MediaTimeout     time.Duration `json:"media_timeout"`
	TranscodeEnabled bool          `json:"transcode_enabled"`
	GPUEnabled       bool          `json:"gpu_enabled"`
	VideoCodec       string        `json:"video_codec"`
	KeyframeInterval time.Duration `json:"keyframe_interval"`
	JitterLatency    time.Duration `json:"jitter_latency"`
	IdleTimeout      time.Duration `json:"idle_timeout"`
	MaxDuration      time.Duration `json:"max_duration"`
}

// DynamicConfigUpdate is the input format for partial config updates via API.
type DynamicConfigUpdate struct {
	MaxBitrate       *int64  `json:"max_bitrate_bps,omitempty"`
	MaxConcurrent    *int32  `json:"max_concurrent,omitempty"`
	MediaTimeoutMs   *int64  `json:"media_timeout_ms,omitempty"`
	TranscodeEnabled *bool   `json:"transcode_enabled,omitempty"`
	GPUEnabled       *bool   `json:"gpu_enabled,omitempty"`
	VideoCodec       *string `json:"video_codec,omitempty"`
	KeyframeIvlMs    *int64  `json:"keyframe_interval_ms,omitempty"`
	JitterLatencyMs  *int64  `json:"jitter_latency_ms,omitempty"`
	IdleTimeoutSec   *int64  `json:"idle_timeout_sec,omitempty"`
	MaxDurationSec   *int64  `json:"max_duration_sec,omitempty"`
}

// NewDynamicConfig creates a DynamicConfig with sensible defaults.
func NewDynamicConfig(log logger.Logger) *DynamicConfig {
	dc := &DynamicConfig{
		log:         log.WithValues("component", "dynamic_config"),
		videoCodec:  "h264",
		keyframeIvl: 2 * time.Second,
		jitterLatency: 80 * time.Millisecond,
		idleTimeout:   30 * time.Second,
		maxDuration:   2 * time.Hour,
		changesCap:    200,
	}
	dc.maxBitrate.Store(1_500_000)
	dc.maxConcurrent.Store(10)
	dc.mediaTimeout.Store(int64(15 * time.Second))
	dc.transcodeEnabled.Store(true)
	dc.gpuEnabled.Store(false)
	dc.changes = make([]ConfigChange, 0, 200)
	return dc
}

// --- Lock-free reads (hot path) ---

func (dc *DynamicConfig) MaxBitrate() int64        { return dc.maxBitrate.Load() }
func (dc *DynamicConfig) MaxConcurrent() int32      { return dc.maxConcurrent.Load() }
func (dc *DynamicConfig) MediaTimeout() time.Duration {
	return time.Duration(dc.mediaTimeout.Load())
}
func (dc *DynamicConfig) TranscodeEnabled() bool { return dc.transcodeEnabled.Load() }
func (dc *DynamicConfig) GPUEnabled() bool       { return dc.gpuEnabled.Load() }

// --- Mutex-protected reads ---

func (dc *DynamicConfig) VideoCodec() string {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.videoCodec
}

func (dc *DynamicConfig) KeyframeInterval() time.Duration {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.keyframeIvl
}

func (dc *DynamicConfig) JitterLatency() time.Duration {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.jitterLatency
}

func (dc *DynamicConfig) IdleTimeout() time.Duration {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.idleTimeout
}

func (dc *DynamicConfig) MaxDuration() time.Duration {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.maxDuration
}

// --- Writes with validation ---

func (dc *DynamicConfig) SetMaxBitrate(bps int64, source string) error {
	if bps < 100_000 || bps > 50_000_000 {
		return fmt.Errorf("max_bitrate out of range [100000, 50000000]: %d", bps)
	}
	old := dc.maxBitrate.Swap(bps)
	dc.recordChange("max_bitrate_bps", old, bps, source)
	return nil
}

func (dc *DynamicConfig) SetMaxConcurrent(n int32, source string) error {
	if n < 1 || n > 1000 {
		return fmt.Errorf("max_concurrent out of range [1, 1000]: %d", n)
	}
	old := dc.maxConcurrent.Swap(n)
	dc.recordChange("max_concurrent", old, n, source)
	return nil
}

func (dc *DynamicConfig) SetMediaTimeout(d time.Duration, source string) error {
	if d < time.Second || d > 5*time.Minute {
		return fmt.Errorf("media_timeout out of range [1s, 5m]: %s", d)
	}
	old := time.Duration(dc.mediaTimeout.Swap(int64(d)))
	dc.recordChange("media_timeout", old, d, source)
	return nil
}

func (dc *DynamicConfig) SetTranscodeEnabled(enabled bool, source string) {
	old := dc.transcodeEnabled.Swap(enabled)
	if old != enabled {
		dc.recordChange("transcode_enabled", old, enabled, source)
	}
}

func (dc *DynamicConfig) SetGPUEnabled(enabled bool, source string) {
	old := dc.gpuEnabled.Swap(enabled)
	if old != enabled {
		dc.recordChange("gpu_enabled", old, enabled, source)
	}
}

func (dc *DynamicConfig) SetVideoCodec(codec string, source string) error {
	if codec != "h264" && codec != "vp8" {
		return fmt.Errorf("invalid video codec: %s (must be h264 or vp8)", codec)
	}
	dc.mu.Lock()
	old := dc.videoCodec
	dc.videoCodec = codec
	dc.mu.Unlock()
	if old != codec {
		dc.recordChange("video_codec", old, codec, source)
	}
	return nil
}

func (dc *DynamicConfig) SetKeyframeInterval(d time.Duration, source string) error {
	if d < 500*time.Millisecond || d > 30*time.Second {
		return fmt.Errorf("keyframe_interval out of range [500ms, 30s]: %s", d)
	}
	dc.mu.Lock()
	old := dc.keyframeIvl
	dc.keyframeIvl = d
	dc.mu.Unlock()
	if old != d {
		dc.recordChange("keyframe_interval", old, d, source)
	}
	return nil
}

func (dc *DynamicConfig) SetJitterLatency(d time.Duration, source string) error {
	if d < 10*time.Millisecond || d > time.Second {
		return fmt.Errorf("jitter_latency out of range [10ms, 1s]: %s", d)
	}
	dc.mu.Lock()
	old := dc.jitterLatency
	dc.jitterLatency = d
	dc.mu.Unlock()
	if old != d {
		dc.recordChange("jitter_latency", old, d, source)
	}
	return nil
}

func (dc *DynamicConfig) SetIdleTimeout(d time.Duration, source string) error {
	if d < 5*time.Second || d > 10*time.Minute {
		return fmt.Errorf("idle_timeout out of range [5s, 10m]: %s", d)
	}
	dc.mu.Lock()
	old := dc.idleTimeout
	dc.idleTimeout = d
	dc.mu.Unlock()
	if old != d {
		dc.recordChange("idle_timeout", old, d, source)
	}
	return nil
}

func (dc *DynamicConfig) SetMaxDuration(d time.Duration, source string) error {
	if d < time.Minute || d > 24*time.Hour {
		return fmt.Errorf("max_duration out of range [1m, 24h]: %s", d)
	}
	dc.mu.Lock()
	old := dc.maxDuration
	dc.maxDuration = d
	dc.mu.Unlock()
	if old != d {
		dc.recordChange("max_duration", old, d, source)
	}
	return nil
}

// --- Bulk update (for API) ---

// Apply applies a partial update. Only non-nil fields are changed.
// Returns a list of validation errors (non-fatal: valid fields are still applied).
func (dc *DynamicConfig) Apply(update DynamicConfigUpdate, source string) []error {
	var errs []error
	if update.MaxBitrate != nil {
		if err := dc.SetMaxBitrate(*update.MaxBitrate, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.MaxConcurrent != nil {
		if err := dc.SetMaxConcurrent(*update.MaxConcurrent, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.MediaTimeoutMs != nil {
		if err := dc.SetMediaTimeout(time.Duration(*update.MediaTimeoutMs)*time.Millisecond, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.TranscodeEnabled != nil {
		dc.SetTranscodeEnabled(*update.TranscodeEnabled, source)
	}
	if update.GPUEnabled != nil {
		dc.SetGPUEnabled(*update.GPUEnabled, source)
	}
	if update.VideoCodec != nil {
		if err := dc.SetVideoCodec(*update.VideoCodec, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.KeyframeIvlMs != nil {
		if err := dc.SetKeyframeInterval(time.Duration(*update.KeyframeIvlMs)*time.Millisecond, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.JitterLatencyMs != nil {
		if err := dc.SetJitterLatency(time.Duration(*update.JitterLatencyMs)*time.Millisecond, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.IdleTimeoutSec != nil {
		if err := dc.SetIdleTimeout(time.Duration(*update.IdleTimeoutSec)*time.Second, source); err != nil {
			errs = append(errs, err)
		}
	}
	if update.MaxDurationSec != nil {
		if err := dc.SetMaxDuration(time.Duration(*update.MaxDurationSec)*time.Second, source); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// --- Snapshot ---

func (dc *DynamicConfig) Snapshot() DynamicConfigSnapshot {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return DynamicConfigSnapshot{
		MaxBitrate:       dc.maxBitrate.Load(),
		MaxConcurrent:    dc.maxConcurrent.Load(),
		MediaTimeout:     time.Duration(dc.mediaTimeout.Load()),
		TranscodeEnabled: dc.transcodeEnabled.Load(),
		GPUEnabled:       dc.gpuEnabled.Load(),
		VideoCodec:       dc.videoCodec,
		KeyframeInterval: dc.keyframeIvl,
		JitterLatency:    dc.jitterLatency,
		IdleTimeout:      dc.idleTimeout,
		MaxDuration:      dc.maxDuration,
	}
}

// MarshalJSON implements json.Marshaler.
func (dc *DynamicConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(dc.Snapshot())
}

// --- Change listeners ---

// OnChange registers a callback invoked after any config value changes.
// The callback receives the key name and new value.
func (dc *DynamicConfig) OnChange(fn func(key string, value interface{})) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.listeners = append(dc.listeners, fn)
}

// RecentChanges returns the last N config changes.
func (dc *DynamicConfig) RecentChanges(n int) []ConfigChange {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	total := len(dc.changes)
	if n > total {
		n = total
	}
	if n <= 0 {
		return nil
	}
	out := make([]ConfigChange, n)
	copy(out, dc.changes[total-n:])
	return out
}

func (dc *DynamicConfig) recordChange(key string, oldVal, newVal interface{}, source string) {
	change := ConfigChange{
		Timestamp: time.Now(),
		Key:       key,
		OldValue:  oldVal,
		NewValue:  newVal,
		Source:    source,
	}

	dc.log.Infow("config changed",
		"key", key, "old", oldVal, "new", newVal, "source", source)

	dc.mu.Lock()
	if len(dc.changes) >= dc.changesCap {
		// Evict oldest quarter
		dc.changes = dc.changes[dc.changesCap/4:]
	}
	dc.changes = append(dc.changes, change)
	listeners := make([]func(string, interface{}), len(dc.listeners))
	copy(listeners, dc.listeners)
	dc.mu.Unlock()

	for _, fn := range listeners {
		fn(key, newVal)
	}
}
