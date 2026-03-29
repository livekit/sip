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
	"runtime"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/resilience"
	"github.com/livekit/sip/pkg/videobridge/session"
)

// TestLongRunningStateMachine creates sessions, runs them through a full
// lifecycle, and checks for memory leaks by comparing runtime.MemStats
// before and after. This is a simplified long-run test suitable for CI.
func TestLongRunningStateMachine(t *testing.T) {
	const numCycles = 1000

	// Force GC and capture baseline
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < numCycles; i++ {
		sm := session.NewStateMachine()

		// Full lifecycle: INIT → READY → STREAMING → CLOSING → CLOSED
		if err := sm.Transition(session.StateInit, session.StateReady); err != nil {
			t.Fatalf("cycle %d: INIT→READY: %v", i, err)
		}
		if err := sm.Transition(session.StateReady, session.StateStreaming); err != nil {
			t.Fatalf("cycle %d: READY→STREAMING: %v", i, err)
		}
		if err := sm.Transition(session.StateStreaming, session.StateClosing); err != nil {
			t.Fatalf("cycle %d: STREAMING→CLOSING: %v", i, err)
		}
		if err := sm.Transition(session.StateClosing, session.StateClosed); err != nil {
			t.Fatalf("cycle %d: CLOSING→CLOSED: %v", i, err)
		}
	}

	// Force GC and capture final
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Use int64 to handle case where GC freed more than was allocated (negative growth)
	heapGrowthBytes := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	heapGrowthMB := float64(heapGrowthBytes) / 1024 / 1024
	t.Logf("Long-run: %d cycles, heap growth: %.2f MB, total allocs: %d",
		numCycles, heapGrowthMB, after.TotalAlloc-before.TotalAlloc)

	// Allow up to 10MB growth for 1000 cycles — should be well under this
	if heapGrowthMB > 10 {
		t.Errorf("excessive heap growth: %.2f MB after %d cycles (possible leak)", heapGrowthMB, numCycles)
	}
}

// TestLongRunningCircuitBreaker exercises the circuit breaker through many
// trip/recovery cycles to detect state leaks or drift.
func TestLongRunningCircuitBreaker(t *testing.T) {
	log := logger.GetLogger()
	const cycles = 100

	cb := resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:                "longrun",
		MaxFailures:         3,
		OpenDuration:        10 * time.Millisecond,
		HalfOpenMaxAttempts: 1,
	})

	for c := 0; c < cycles; c++ {
		// Trip it
		for i := 0; i < 5; i++ {
			if cb.Allow() {
				cb.RecordFailure(nil)
			}
		}

		if cb.State() != resilience.StateOpen {
			t.Fatalf("cycle %d: expected open after failures", c)
		}

		// Wait for half-open
		time.Sleep(15 * time.Millisecond)

		// Recover
		if cb.Allow() {
			cb.RecordSuccess()
		}

		// May need another success for half-open → closed
		time.Sleep(5 * time.Millisecond)
		if cb.State() == resilience.StateHalfOpen {
			if cb.Allow() {
				cb.RecordSuccess()
			}
		}

		// Reset for next cycle
		cb.Reset()

		if cb.State() != resilience.StateClosed {
			t.Fatalf("cycle %d: expected closed after reset, got %s", c, cb.State())
		}
	}

	stats := cb.Stats()
	t.Logf("Long-run CB: %d cycles, trips=%d, successes=%d, failures=%d",
		cycles, stats.Trips, stats.TotalSuccess, stats.TotalFailure)
}

// TestSessionTTLEnforcement verifies the lifecycle monitor correctly detects
// sessions that exceed their max duration.
func TestSessionTTLEnforcement(t *testing.T) {
	sm := session.NewStateMachine()
	sm.Transition(session.StateInit, session.StateReady)

	lc := session.NewLifecycleMonitor(logger.GetLogger(), session.LifecycleConfig{
		MaxDuration:      100 * time.Millisecond,
		IdleTimeout:      0, // disabled
		StreamingTimeout: 0, // disabled
	}, sm)

	// Immediately after creation, should not be expired
	shouldClose, _ := lc.Check()
	if shouldClose {
		t.Error("should not request close immediately after creation")
	}

	// Wait past max duration
	time.Sleep(150 * time.Millisecond)

	shouldClose, reason := lc.Check()
	if !shouldClose {
		t.Error("should request close after max duration exceeded")
	}
	if reason != "max_duration_exceeded" {
		t.Errorf("expected reason max_duration_exceeded, got %s", reason)
	}
}

// TestSessionIdleDetection verifies idle timeout tracking via Check().
func TestSessionIdleDetection(t *testing.T) {
	sm := session.NewStateMachine()
	sm.Transition(session.StateInit, session.StateReady)
	sm.Transition(session.StateReady, session.StateStreaming)

	lc := session.NewLifecycleMonitor(logger.GetLogger(), session.LifecycleConfig{
		MaxDuration: 0, // disabled
		IdleTimeout: 100 * time.Millisecond,
	}, sm)

	// Touch activity
	lc.TouchVideo()

	// Should not be idle right after touch
	shouldClose, _ := lc.Check()
	if shouldClose {
		t.Error("should not request close right after touch")
	}

	// Wait past idle timeout
	time.Sleep(150 * time.Millisecond)

	shouldClose, reason := lc.Check()
	if !shouldClose {
		t.Error("should request close after idle timeout")
	}
	if reason != "idle_timeout" {
		t.Errorf("expected reason idle_timeout, got %s", reason)
	}

	// Touch again and verify reset
	lc.TouchVideo()
	shouldClose, _ = lc.Check()
	if shouldClose {
		t.Error("should not request close after fresh touch")
	}
}
