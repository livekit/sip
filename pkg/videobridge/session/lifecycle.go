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

package session

import (
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// LifecycleConfig configures session lifecycle enforcement.
type LifecycleConfig struct {
	// MaxDuration is the hard limit on session duration. 0 = unlimited.
	MaxDuration time.Duration
	// IdleTimeout closes sessions with no media activity. 0 = disabled.
	IdleTimeout time.Duration
	// StreamingTimeout closes sessions that never reach Streaming state. 0 = disabled.
	StreamingTimeout time.Duration
	// CleanupInterval is how often the reaper checks for expired sessions.
	CleanupInterval time.Duration
}

// DefaultLifecycleConfig returns production defaults.
func DefaultLifecycleConfig() LifecycleConfig {
	return LifecycleConfig{
		MaxDuration:      4 * time.Hour,
		IdleTimeout:      60 * time.Second,
		StreamingTimeout: 30 * time.Second,
		CleanupInterval:  10 * time.Second,
	}
}

// LifecycleMonitor tracks per-session activity and enforces timeouts.
// Embedded in Session — not a standalone goroutine.
type LifecycleMonitor struct {
	log  logger.Logger
	conf LifecycleConfig
	sm   *StateMachine

	startTime      time.Time
	lastVideoAt    atomic.Int64 // unix nanos
	lastAudioAt    atomic.Int64
	closeRequested atomic.Bool
	closeReason    atomic.Value // string
}

// NewLifecycleMonitor creates a lifecycle monitor.
func NewLifecycleMonitor(log logger.Logger, conf LifecycleConfig, sm *StateMachine) *LifecycleMonitor {
	return &LifecycleMonitor{
		log:       log,
		conf:      conf,
		sm:        sm,
		startTime: time.Now(),
	}
}

// TouchVideo records video activity.
func (lm *LifecycleMonitor) TouchVideo() {
	lm.lastVideoAt.Store(time.Now().UnixNano())
}

// TouchAudio records audio activity.
func (lm *LifecycleMonitor) TouchAudio() {
	lm.lastAudioAt.Store(time.Now().UnixNano())
}

// Check evaluates all lifecycle conditions and returns true if the session should close.
// Returns (shouldClose, reason).
func (lm *LifecycleMonitor) Check() (bool, string) {
	now := time.Now()

	// 1. Max duration
	if lm.conf.MaxDuration > 0 {
		if now.Sub(lm.startTime) > lm.conf.MaxDuration {
			return true, "max_duration_exceeded"
		}
	}

	// 2. Streaming timeout (never received first media)
	if lm.conf.StreamingTimeout > 0 && !lm.sm.IsStreaming() && lm.sm.IsActive() {
		if now.Sub(lm.startTime) > lm.conf.StreamingTimeout {
			return true, "streaming_timeout"
		}
	}

	// 3. Idle timeout (no media activity)
	if lm.conf.IdleTimeout > 0 && lm.sm.IsStreaming() {
		lastVideo := lm.lastVideoAt.Load()
		lastAudio := lm.lastAudioAt.Load()
		lastActivity := lastVideo
		if lastAudio > lastActivity {
			lastActivity = lastAudio
		}
		if lastActivity > 0 {
			idle := now.Sub(time.Unix(0, lastActivity))
			if idle > lm.conf.IdleTimeout {
				return true, "idle_timeout"
			}
		}
	}

	return false, ""
}

// RequestClose marks the session for closure with a reason.
func (lm *LifecycleMonitor) RequestClose(reason string) {
	if lm.closeRequested.CompareAndSwap(false, true) {
		lm.closeReason.Store(reason)
		stats.SessionErrors.WithLabelValues("lifecycle_" + reason).Inc()
		lm.log.Infow("lifecycle close requested", "reason", reason,
			"duration", time.Since(lm.startTime),
			"state", lm.sm.Current().String(),
		)
	}
}

// ShouldClose returns true if close was requested.
func (lm *LifecycleMonitor) ShouldClose() bool {
	return lm.closeRequested.Load()
}

// CloseReason returns the reason for closure, if any.
func (lm *LifecycleMonitor) CloseReason() string {
	v := lm.closeReason.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// SessionReaper periodically checks all sessions and closes expired ones.
type SessionReaper struct {
	log      logger.Logger
	conf     LifecycleConfig
	sessions func() []*Session
	closeFn  func(callID string)
	stop     chan struct{}
}

// NewSessionReaper creates a reaper that enforces lifecycle limits.
func NewSessionReaper(log logger.Logger, conf LifecycleConfig, sessions func() []*Session, closeFn func(callID string)) *SessionReaper {
	if conf.CleanupInterval <= 0 {
		conf.CleanupInterval = 10 * time.Second
	}
	return &SessionReaper{
		log:      log,
		conf:     conf,
		sessions: sessions,
		closeFn:  closeFn,
		stop:     make(chan struct{}),
	}
}

// Start begins the reaper loop.
func (r *SessionReaper) Start() {
	go r.loop()
}

// Stop halts the reaper.
func (r *SessionReaper) Stop() {
	close(r.stop)
}

func (r *SessionReaper) loop() {
	ticker := time.NewTicker(r.conf.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.sweep()
		}
	}
}

func (r *SessionReaper) sweep() {
	sessions := r.sessions()
	for _, sess := range sessions {
		if sess.lifecycle == nil {
			continue
		}
		shouldClose, reason := sess.lifecycle.Check()
		if shouldClose {
			sess.lifecycle.RequestClose(reason)
			r.closeFn(sess.CallID)
		}
	}
}
