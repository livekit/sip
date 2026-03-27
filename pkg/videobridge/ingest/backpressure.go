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

package ingest

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// DropStrategy defines how frames are dropped under congestion.
type DropStrategy int

const (
	// DropNone does not drop frames (default — only use for passthrough).
	DropNone DropStrategy = iota
	// DropNonKeyframe drops all non-keyframes when congested, preserving IDR frames.
	DropNonKeyframe
	// DropTail drops the newest frames when the queue is full (tail drop).
	DropTail
	// DropTemporalLayer drops higher temporal layers first (SVC-aware).
	DropTemporalLayer
)

// BackpressureConfig configures the backpressure controller.
type BackpressureConfig struct {
	// MaxQueueDepth is the maximum number of pending frames before dropping.
	MaxQueueDepth int
	// Strategy defines which frames to drop under congestion.
	Strategy DropStrategy
	// CongestionThreshold is the queue depth ratio (0.0-1.0) at which congestion starts.
	CongestionThreshold float64
	// RecoveryThreshold is the queue depth ratio below which congestion clears.
	RecoveryThreshold float64
}

// BackpressureController monitors queue depth and applies drop strategies
// when the downstream consumer (transcoder or publisher) cannot keep up.
type BackpressureController struct {
	log  logger.Logger
	conf BackpressureConfig

	mu        sync.Mutex
	queueSize atomic.Int64
	congested atomic.Bool

	// Counters
	framesReceived atomic.Uint64
	framesDropped  atomic.Uint64
	framesPassed   atomic.Uint64

	// Congestion events
	congestionStart  time.Time
	congestionEvents atomic.Uint64
}

// NewBackpressureController creates a new backpressure controller.
func NewBackpressureController(log logger.Logger, conf BackpressureConfig) *BackpressureController {
	if conf.MaxQueueDepth <= 0 {
		conf.MaxQueueDepth = 30 // ~1 second at 30fps
	}
	if conf.CongestionThreshold <= 0 {
		conf.CongestionThreshold = 0.8
	}
	if conf.RecoveryThreshold <= 0 {
		conf.RecoveryThreshold = 0.3
	}
	if conf.Strategy == 0 {
		conf.Strategy = DropNonKeyframe
	}

	return &BackpressureController{
		log:  log,
		conf: conf,
	}
}

// ShouldDrop decides whether to drop a frame based on current congestion state.
// isKeyframe indicates whether the frame is an IDR/keyframe.
// Returns true if the frame should be dropped.
func (bp *BackpressureController) ShouldDrop(isKeyframe bool) bool {
	bp.framesReceived.Add(1)

	depth := bp.queueSize.Load()
	maxDepth := int64(bp.conf.MaxQueueDepth)

	// Check congestion state
	ratio := float64(depth) / float64(maxDepth)
	wasCongested := bp.congested.Load()

	if !wasCongested && ratio >= bp.conf.CongestionThreshold {
		bp.congested.Store(true)
		bp.congestionEvents.Add(1)
		bp.mu.Lock()
		bp.congestionStart = time.Now()
		bp.mu.Unlock()
		bp.log.Warnw("congestion detected", nil,
			"queueDepth", depth,
			"maxDepth", maxDepth,
			"ratio", ratio,
		)
		stats.SessionErrors.WithLabelValues("congestion_start").Inc()
	} else if wasCongested && ratio <= bp.conf.RecoveryThreshold {
		bp.congested.Store(false)
		bp.mu.Lock()
		dur := time.Since(bp.congestionStart)
		bp.mu.Unlock()
		bp.log.Infow("congestion cleared",
			"duration", dur,
			"droppedFrames", bp.framesDropped.Load(),
		)
	}

	if !bp.congested.Load() {
		bp.framesPassed.Add(1)
		return false
	}

	// Apply drop strategy
	switch bp.conf.Strategy {
	case DropNone:
		bp.framesPassed.Add(1)
		return false

	case DropNonKeyframe:
		if isKeyframe {
			bp.framesPassed.Add(1)
			return false // never drop keyframes
		}
		bp.framesDropped.Add(1)
		stats.SessionErrors.WithLabelValues("frame_dropped").Inc()
		return true

	case DropTail:
		if depth >= maxDepth {
			bp.framesDropped.Add(1)
			stats.SessionErrors.WithLabelValues("frame_dropped").Inc()
			return true
		}
		bp.framesPassed.Add(1)
		return false

	case DropTemporalLayer:
		// Drop non-keyframes, and if still congested, drop more aggressively
		if isKeyframe {
			bp.framesPassed.Add(1)
			return false
		}
		// Higher congestion = drop more frames
		if ratio > 0.95 {
			// Critical: drop everything except keyframes
			bp.framesDropped.Add(1)
			stats.SessionErrors.WithLabelValues("frame_dropped").Inc()
			return true
		}
		if ratio > 0.9 {
			// Drop 75% of non-keyframes
			if bp.framesReceived.Load()%4 != 0 {
				bp.framesDropped.Add(1)
				stats.SessionErrors.WithLabelValues("frame_dropped").Inc()
				return true
			}
		}
		bp.framesPassed.Add(1)
		return false

	default:
		bp.framesPassed.Add(1)
		return false
	}
}

// Enqueue increments the queue depth counter. Call when a frame enters the processing queue.
func (bp *BackpressureController) Enqueue() {
	bp.queueSize.Add(1)
}

// Dequeue decrements the queue depth counter. Call when a frame leaves the processing queue.
func (bp *BackpressureController) Dequeue() {
	bp.queueSize.Add(-1)
}

// IsCongested returns the current congestion state.
func (bp *BackpressureController) IsCongested() bool {
	return bp.congested.Load()
}

// Stats returns backpressure statistics.
func (bp *BackpressureController) Stats() BackpressureStats {
	return BackpressureStats{
		FramesReceived:   bp.framesReceived.Load(),
		FramesDropped:    bp.framesDropped.Load(),
		FramesPassed:     bp.framesPassed.Load(),
		QueueDepth:       bp.queueSize.Load(),
		Congested:        bp.congested.Load(),
		CongestionEvents: bp.congestionEvents.Load(),
	}
}

// BackpressureStats holds backpressure statistics.
type BackpressureStats struct {
	FramesReceived   uint64
	FramesDropped    uint64
	FramesPassed     uint64
	QueueDepth       int64
	Congested        bool
	CongestionEvents uint64
}
