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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// --- Session Explosion Guard ---

// SessionGuardConfig configures session explosion protection.
type SessionGuardConfig struct {
	// MaxSessionsPerNode is the hard ceiling per bridge instance. 0 = unlimited.
	MaxSessionsPerNode int
	// MaxSessionsPerCaller limits concurrent sessions from the same SIP From URI. 0 = unlimited.
	MaxSessionsPerCaller int
	// NewSessionRateLimit is max new sessions per second (token bucket). 0 = unlimited.
	NewSessionRateLimit float64
	// NewSessionBurst is the burst size for the rate limiter.
	NewSessionBurst int
}

// SessionGuard prevents session explosion via per-node limits, per-caller limits,
// and rate limiting on new session creation.
type SessionGuard struct {
	log  logger.Logger
	conf SessionGuardConfig

	mu             sync.Mutex
	activeTotal    atomic.Int64
	activeByCaller map[string]int

	// Token bucket rate limiter
	tokens     float64
	lastRefill time.Time

	// Stats
	rejected     atomic.Uint64
	rateLimited  atomic.Uint64
	callerLimited atomic.Uint64
}

// NewSessionGuard creates a session explosion guard.
func NewSessionGuard(log logger.Logger, conf SessionGuardConfig) *SessionGuard {
	if conf.MaxSessionsPerNode <= 0 {
		conf.MaxSessionsPerNode = 100
	}
	if conf.MaxSessionsPerCaller <= 0 {
		conf.MaxSessionsPerCaller = 5
	}
	if conf.NewSessionRateLimit <= 0 {
		conf.NewSessionRateLimit = 10.0 // 10 new sessions/sec
	}
	if conf.NewSessionBurst <= 0 {
		conf.NewSessionBurst = 20
	}

	return &SessionGuard{
		log:            log,
		conf:           conf,
		activeByCaller: make(map[string]int),
		tokens:         float64(conf.NewSessionBurst),
		lastRefill:     time.Now(),
	}
}

// Admit checks if a new session should be admitted.
// callerID is typically the SIP From URI.
// Returns nil if admitted, error with reason if rejected.
func (g *SessionGuard) Admit(callerID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1. Per-node limit
	current := int(g.activeTotal.Load())
	if current >= g.conf.MaxSessionsPerNode {
		g.rejected.Add(1)
		stats.SessionErrors.WithLabelValues("guard_node_limit").Inc()
		return fmt.Errorf("node session limit reached (%d/%d)", current, g.conf.MaxSessionsPerNode)
	}

	// 2. Per-caller limit
	if callerID != "" {
		callerCount := g.activeByCaller[callerID]
		if callerCount >= g.conf.MaxSessionsPerCaller {
			g.callerLimited.Add(1)
			stats.SessionErrors.WithLabelValues("guard_caller_limit").Inc()
			return fmt.Errorf("caller session limit reached for %s (%d/%d)", callerID, callerCount, g.conf.MaxSessionsPerCaller)
		}
	}

	// 3. Rate limit (token bucket)
	if !g.tryConsumeToken() {
		g.rateLimited.Add(1)
		stats.SessionErrors.WithLabelValues("guard_rate_limit").Inc()
		return fmt.Errorf("session rate limit exceeded (%.0f/sec)", g.conf.NewSessionRateLimit)
	}

	// Admitted — track it
	g.activeTotal.Add(1)
	if callerID != "" {
		g.activeByCaller[callerID]++
	}

	return nil
}

// Release decrements counters when a session ends.
func (g *SessionGuard) Release(callerID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.activeTotal.Add(-1)
	if callerID != "" {
		g.activeByCaller[callerID]--
		if g.activeByCaller[callerID] <= 0 {
			delete(g.activeByCaller, callerID)
		}
	}
}

func (g *SessionGuard) tryConsumeToken() bool {
	now := time.Now()
	elapsed := now.Sub(g.lastRefill).Seconds()
	g.lastRefill = now

	// Refill tokens
	g.tokens += elapsed * g.conf.NewSessionRateLimit
	max := float64(g.conf.NewSessionBurst)
	if g.tokens > max {
		g.tokens = max
	}

	if g.tokens < 1.0 {
		return false
	}
	g.tokens--
	return true
}

// Stats returns guard statistics.
func (g *SessionGuard) Stats() SessionGuardStats {
	g.mu.Lock()
	callers := len(g.activeByCaller)
	g.mu.Unlock()
	return SessionGuardStats{
		Active:        g.activeTotal.Load(),
		UniquCallers:  callers,
		Rejected:      g.rejected.Load(),
		RateLimited:   g.rateLimited.Load(),
		CallerLimited: g.callerLimited.Load(),
	}
}

// SessionGuardStats holds guard statistics.
type SessionGuardStats struct {
	Active        int64  `json:"active"`
	UniquCallers  int    `json:"unique_callers"`
	Rejected      uint64 `json:"rejected"`
	RateLimited   uint64 `json:"rate_limited"`
	CallerLimited uint64 `json:"caller_limited"`
}

// --- Transcoder Overload Protection ---

// TranscoderGuardConfig configures transcoder overload protection.
type TranscoderGuardConfig struct {
	// MaxConcurrent is the hard limit on concurrent transcode sessions.
	MaxConcurrent int
	// QueueDepthThreshold: if queue depth exceeds this, reject new transcode requests.
	QueueDepthThreshold int
	// CPUThreshold: if CPU usage exceeds this ratio (0.0-1.0), shed load.
	CPUThreshold float64
	// ShedStrategy defines what happens when overloaded.
	ShedStrategy LoadShedStrategy
}

// LoadShedStrategy defines how to handle overload.
type LoadShedStrategy int

const (
	// ShedRejectNew rejects new transcode requests but keeps existing ones running.
	ShedRejectNew LoadShedStrategy = iota
	// ShedFallbackPassthrough switches new requests to H.264 passthrough (no transcode).
	ShedFallbackPassthrough
	// ShedKillOldest terminates the oldest transcode session to make room.
	ShedKillOldest
)

// TranscoderGuard prevents transcoder overload.
type TranscoderGuard struct {
	log  logger.Logger
	conf TranscoderGuardConfig

	active    atomic.Int32
	queueDepth atomic.Int64
	cpuUsage  atomic.Uint64 // stored as float64 bits

	// Stats
	rejected     atomic.Uint64
	shed         atomic.Uint64
	fallbacks    atomic.Uint64
}

// NewTranscoderGuard creates a transcoder overload guard.
func NewTranscoderGuard(log logger.Logger, conf TranscoderGuardConfig) *TranscoderGuard {
	if conf.MaxConcurrent <= 0 {
		conf.MaxConcurrent = 10
	}
	if conf.QueueDepthThreshold <= 0 {
		conf.QueueDepthThreshold = 60 // ~2 seconds at 30fps
	}
	if conf.CPUThreshold <= 0 {
		conf.CPUThreshold = 0.90
	}
	return &TranscoderGuard{
		log:  log,
		conf: conf,
	}
}

// AdmitTranscode checks if a new transcode session should be admitted.
// Returns (admitted, fallbackToPassthrough).
func (g *TranscoderGuard) AdmitTranscode() (bool, bool) {
	current := int(g.active.Load())

	// Hard concurrent limit
	if current >= g.conf.MaxConcurrent {
		g.rejected.Add(1)
		stats.SessionErrors.WithLabelValues("transcode_guard_limit").Inc()

		switch g.conf.ShedStrategy {
		case ShedFallbackPassthrough:
			g.fallbacks.Add(1)
			g.log.Warnw("transcoder at capacity, falling back to passthrough", nil,
				"active", current, "max", g.conf.MaxConcurrent)
			return false, true // don't transcode, use passthrough
		case ShedKillOldest:
			g.shed.Add(1)
			g.log.Warnw("transcoder at capacity, shedding load", nil,
				"active", current, "max", g.conf.MaxConcurrent)
			return false, false
		default: // ShedRejectNew
			g.log.Warnw("transcoder at capacity, rejecting", nil,
				"active", current, "max", g.conf.MaxConcurrent)
			return false, false
		}
	}

	// Queue depth check
	depth := g.queueDepth.Load()
	if int(depth) > g.conf.QueueDepthThreshold {
		g.rejected.Add(1)
		stats.SessionErrors.WithLabelValues("transcode_guard_queue").Inc()
		g.log.Warnw("transcode queue too deep, rejecting", nil,
			"depth", depth, "threshold", g.conf.QueueDepthThreshold)

		if g.conf.ShedStrategy == ShedFallbackPassthrough {
			g.fallbacks.Add(1)
			return false, true
		}
		return false, false
	}

	g.active.Add(1)
	return true, false
}

// ReleaseTranscode decrements the active transcoder count.
func (g *TranscoderGuard) ReleaseTranscode() {
	g.active.Add(-1)
}

// UpdateQueueDepth updates the current transcode queue depth.
func (g *TranscoderGuard) UpdateQueueDepth(depth int64) {
	g.queueDepth.Store(depth)
}

// Stats returns transcoder guard statistics.
func (g *TranscoderGuard) Stats() TranscoderGuardStats {
	return TranscoderGuardStats{
		Active:       int(g.active.Load()),
		QueueDepth:   g.queueDepth.Load(),
		Rejected:     g.rejected.Load(),
		Shed:         g.shed.Load(),
		Fallbacks:    g.fallbacks.Load(),
	}
}

// TranscoderGuardStats holds transcoder guard statistics.
type TranscoderGuardStats struct {
	Active     int    `json:"active"`
	QueueDepth int64  `json:"queue_depth"`
	Rejected   uint64 `json:"rejected"`
	Shed       uint64 `json:"shed"`
	Fallbacks  uint64 `json:"fallbacks"`
}

// --- Rollback ---

// Rollback tracks resources allocated during session initialization
// and cleans them up if setup fails partway through.
type Rollback struct {
	log     logger.Logger
	steps   []rollbackStep
	done    bool
}

type rollbackStep struct {
	name    string
	cleanup func() error
}

// NewRollback creates a new rollback tracker.
func NewRollback(log logger.Logger) *Rollback {
	return &Rollback{log: log}
}

// Add registers a cleanup step. Steps are executed in reverse order on Rollback().
func (r *Rollback) Add(name string, cleanup func() error) {
	r.steps = append(r.steps, rollbackStep{name: name, cleanup: cleanup})
}

// Commit marks the initialization as successful. No rollback will occur.
func (r *Rollback) Commit() {
	r.done = true
}

// Execute runs all cleanup steps in reverse order if Commit() was not called.
// Typically called via defer: defer rb.Execute()
func (r *Rollback) Execute() {
	if r.done {
		return
	}

	for i := len(r.steps) - 1; i >= 0; i-- {
		step := r.steps[i]
		if err := step.cleanup(); err != nil {
			r.log.Warnw("rollback step failed", err, "step", step.name)
		} else {
			r.log.Debugw("rollback step executed", "step", step.name)
		}
	}

	if len(r.steps) > 0 {
		r.log.Infow("rollback completed", "steps", len(r.steps))
		stats.SessionErrors.WithLabelValues("session_rollback").Inc()
	}
}
