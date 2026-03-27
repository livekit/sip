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
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// DegradationLevel represents the current quality level of the bridge.
type DegradationLevel int32

const (
	// LevelFull means full video + audio, no degradation.
	LevelFull DegradationLevel = 0
	// LevelReducedVideo means lower resolution/framerate video + full audio.
	LevelReducedVideo DegradationLevel = 1
	// LevelAudioOnly means video disabled, audio-only fallback.
	LevelAudioOnly DegradationLevel = 2
	// LevelMinimal means audio at reduced quality (last resort before drop).
	LevelMinimal DegradationLevel = 3
)

func (l DegradationLevel) String() string {
	switch l {
	case LevelFull:
		return "full"
	case LevelReducedVideo:
		return "reduced_video"
	case LevelAudioOnly:
		return "audio_only"
	case LevelMinimal:
		return "minimal"
	default:
		return "unknown"
	}
}

// DegradationConfig configures the graceful degradation controller.
type DegradationConfig struct {
	// CPUThresholdHigh triggers degradation when CPU usage exceeds this (0.0-1.0).
	CPUThresholdHigh float64
	// CPUThresholdLow triggers recovery when CPU drops below this.
	CPUThresholdLow float64
	// PacketLossThreshold triggers degradation when loss exceeds this ratio.
	PacketLossThreshold float64
	// TranscodeFailThreshold: consecutive transcode failures before degrading.
	TranscodeFailThreshold int
	// RecoveryDelay: minimum time before attempting to recover to a higher level.
	RecoveryDelay time.Duration
}

// DegradationAction is a callback for level changes.
type DegradationAction struct {
	// OnVideoDisable is called when video should be disabled (audio-only fallback).
	OnVideoDisable func()
	// OnVideoEnable is called when video can be re-enabled.
	OnVideoEnable func()
	// OnBitrateReduce is called with a target bitrate reduction factor (0.0-1.0).
	OnBitrateReduce func(factor float64)
	// OnFramerateReduce is called with a target framerate (e.g., 15 instead of 30).
	OnFramerateReduce func(fps int)
}

// DegradationController monitors system health and gracefully reduces quality
// rather than dropping calls when resources are constrained.
type DegradationController struct {
	log     logger.Logger
	conf    DegradationConfig
	actions DegradationAction

	level atomic.Int32

	mu                  sync.Mutex
	lastDegradation     time.Time
	lastRecovery        time.Time
	consecutiveTransErr int

	// Stats
	degradations atomic.Uint64
	recoveries   atomic.Uint64
}

// NewDegradationController creates a new graceful degradation controller.
func NewDegradationController(log logger.Logger, conf DegradationConfig, actions DegradationAction) *DegradationController {
	if conf.CPUThresholdHigh <= 0 {
		conf.CPUThresholdHigh = 0.85
	}
	if conf.CPUThresholdLow <= 0 {
		conf.CPUThresholdLow = 0.60
	}
	if conf.PacketLossThreshold <= 0 {
		conf.PacketLossThreshold = 0.10
	}
	if conf.TranscodeFailThreshold <= 0 {
		conf.TranscodeFailThreshold = 3
	}
	if conf.RecoveryDelay <= 0 {
		conf.RecoveryDelay = 30 * time.Second
	}

	dc := &DegradationController{
		log:     log,
		conf:    conf,
		actions: actions,
	}
	dc.level.Store(int32(LevelFull))

	return dc
}

// Level returns the current degradation level.
func (dc *DegradationController) Level() DegradationLevel {
	return DegradationLevel(dc.level.Load())
}

// ReportCPU reports the current CPU usage (0.0-1.0).
func (dc *DegradationController) ReportCPU(usage float64) {
	current := dc.Level()

	if usage >= dc.conf.CPUThresholdHigh {
		dc.degrade("high CPU usage", usage)
	} else if usage <= dc.conf.CPUThresholdLow && current > LevelFull {
		dc.recover("CPU usage normalized")
	}
}

// ReportPacketLoss reports the current packet loss ratio (0.0-1.0).
func (dc *DegradationController) ReportPacketLoss(lossRatio float64) {
	if lossRatio >= dc.conf.PacketLossThreshold {
		dc.degrade("high packet loss", lossRatio)
	}
}

// ReportTranscodeError reports a transcoder failure.
func (dc *DegradationController) ReportTranscodeError(err error) {
	dc.mu.Lock()
	dc.consecutiveTransErr++
	count := dc.consecutiveTransErr
	dc.mu.Unlock()

	if count >= dc.conf.TranscodeFailThreshold {
		dc.degrade("transcoder failures", float64(count))
	}
}

// ReportTranscodeSuccess resets the transcoder failure counter.
func (dc *DegradationController) ReportTranscodeSuccess() {
	dc.mu.Lock()
	dc.consecutiveTransErr = 0
	dc.mu.Unlock()
}

// ForceLevel sets the degradation level directly (for testing or manual override).
func (dc *DegradationController) ForceLevel(level DegradationLevel) {
	old := DegradationLevel(dc.level.Swap(int32(level)))
	if old != level {
		dc.log.Infow("degradation level forced", "from", old.String(), "to", level.String())
		dc.applyLevel(level)
	}
}

func (dc *DegradationController) degrade(reason string, value float64) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	current := DegradationLevel(dc.level.Load())
	if current >= LevelMinimal {
		return // already at lowest level
	}

	next := current + 1
	dc.level.Store(int32(next))
	dc.lastDegradation = time.Now()
	dc.degradations.Add(1)

	dc.log.Warnw("degrading quality",
		nil,
		"reason", reason,
		"value", value,
		"from", current.String(),
		"to", next.String(),
	)
	stats.SessionErrors.WithLabelValues("degradation_" + next.String()).Inc()

	dc.applyLevel(next)
}

func (dc *DegradationController) recover(reason string) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	current := DegradationLevel(dc.level.Load())
	if current <= LevelFull {
		return // already at full quality
	}

	// Don't recover too quickly
	if time.Since(dc.lastDegradation) < dc.conf.RecoveryDelay {
		return
	}

	next := current - 1
	dc.level.Store(int32(next))
	dc.lastRecovery = time.Now()
	dc.recoveries.Add(1)

	dc.log.Infow("recovering quality",
		"reason", reason,
		"from", current.String(),
		"to", next.String(),
	)

	dc.applyLevel(next)
}

func (dc *DegradationController) applyLevel(level DegradationLevel) {
	switch level {
	case LevelFull:
		if dc.actions.OnVideoEnable != nil {
			dc.actions.OnVideoEnable()
		}
		if dc.actions.OnBitrateReduce != nil {
			dc.actions.OnBitrateReduce(1.0) // full bitrate
		}

	case LevelReducedVideo:
		if dc.actions.OnBitrateReduce != nil {
			dc.actions.OnBitrateReduce(0.5) // half bitrate
		}
		if dc.actions.OnFramerateReduce != nil {
			dc.actions.OnFramerateReduce(15) // 15fps
		}

	case LevelAudioOnly:
		if dc.actions.OnVideoDisable != nil {
			dc.actions.OnVideoDisable()
		}

	case LevelMinimal:
		if dc.actions.OnVideoDisable != nil {
			dc.actions.OnVideoDisable()
		}
		if dc.actions.OnBitrateReduce != nil {
			dc.actions.OnBitrateReduce(0.25) // minimal audio bitrate
		}
	}
}

// Stats returns degradation statistics.
func (dc *DegradationController) Stats() DegradationStats {
	return DegradationStats{
		Level:        dc.Level().String(),
		Degradations: dc.degradations.Load(),
		Recoveries:   dc.recoveries.Load(),
	}
}

// DegradationStats holds degradation statistics.
type DegradationStats struct {
	Level        string `json:"level"`
	Degradations uint64 `json:"degradations"`
	Recoveries   uint64 `json:"recoveries"`
}
