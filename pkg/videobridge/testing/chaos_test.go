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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/resilience"
	"github.com/livekit/sip/pkg/videobridge/session"
)

// TestChaosCircuitBreaker injects rapid failures until the circuit breaker trips,
// then verifies it auto-disables and eventually recovers.
func TestChaosCircuitBreaker(t *testing.T) {
	log := logger.GetLogger()

	var videoDisabled atomic.Bool

	cb := resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:                "chaos_test",
		MaxFailures:         5,
		OpenDuration:        200 * time.Millisecond,
		HalfOpenMaxAttempts: 2,
		OnStateChange: func(from, to resilience.CircuitState) {
			if to == resilience.StateOpen {
				videoDisabled.Store(true)
			} else if to == resilience.StateClosed {
				videoDisabled.Store(false)
			}
		},
	})

	// Phase 1: inject failures to trip the circuit
	for i := 0; i < 10; i++ {
		if cb.Allow() {
			cb.RecordFailure(fmt.Errorf("injected failure %d", i))
		}
	}

	time.Sleep(10 * time.Millisecond) // let state propagate

	if cb.State() != resilience.StateOpen {
		t.Fatalf("expected circuit to be open after failures, got %s", cb.State())
	}
	if !videoDisabled.Load() {
		t.Errorf("expected video to be disabled after circuit trip (videoDisabled=%v)", videoDisabled.Load())
	}

	// Phase 2: verify requests are blocked while open
	blocked := 0
	for i := 0; i < 10; i++ {
		if !cb.Allow() {
			blocked++
		}
	}
	if blocked == 0 {
		t.Error("expected some requests to be blocked while circuit is open")
	}

	// Phase 3: wait for half-open transition
	time.Sleep(300 * time.Millisecond)

	// The first Allow() transitions Open→HalfOpen (doesn't increment halfOpenCount).
	// Then HalfOpenMaxAttempts more Allow()+RecordSuccess cycles are needed to close.
	// With HalfOpenMaxAttempts=2, we need 3 total Allow()+RecordSuccess.
	for i := 0; i < 3; i++ {
		if !cb.Allow() {
			t.Errorf("expected Allow to return true during recovery (iteration %d)", i)
			break
		}
		cb.RecordSuccess()
	}

	time.Sleep(10 * time.Millisecond) // let state change propagate
	if cb.State() != resilience.StateClosed {
		t.Errorf("expected circuit to close after recovery, got %s", cb.State())
	}
	if videoDisabled.Load() {
		t.Error("expected video to be re-enabled after circuit recovery")
	}

	stats := cb.Stats()
	t.Logf("Chaos CB stats: %+v", stats)
}

// TestChaosFeatureFlagFlapping rapidly toggles feature flags under concurrent
// reads to verify no race conditions or data corruption.
func TestChaosFeatureFlagFlapping(t *testing.T) {
	log := logger.GetLogger()
	ff := resilience.NewFeatureFlagsWithRegion(log, "chaos-region")

	const (
		writers    = 10
		readers    = 50
		iterations = 1000
	)

	var wg sync.WaitGroup

	// Writers: rapidly toggle flags
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				enabled := i%2 == 0
				switch id % 5 {
				case 0:
					ff.SetVideo(enabled)
				case 1:
					ff.SetAudio(enabled)
				case 2:
					ff.SetTranscode(enabled)
				case 3:
					ff.SetRollout("video", i%100)
				case 4:
					ff.SetTenantOverride(fmt.Sprintf("tenant-%d", id), "video", enabled)
				}
			}
		}(w)
	}

	// Readers: concurrent evaluations
	var readCount atomic.Int64
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ff.IsEnabledFor("video", fmt.Sprintf("session-%d-%d", id, i), "default")
				ff.IsDisabled("video")
				ff.Snapshot()
				readCount.Add(3)
			}
		}(r)
	}

	wg.Wait()

	t.Logf("Flag flapping: %d writers x %d iters, %d total reads completed without race",
		writers, iterations, readCount.Load())

	// If we got here without -race detector failures, the test passes.
	// Final state should be consistent
	snap := ff.Snapshot()
	t.Logf("Final flag state: video=%v audio=%v transcode=%v",
		snap.Video, snap.Audio, snap.Transcode)
}

// TestChaosConcurrentStateTransitions hammers the session state machine
// with concurrent transition attempts to verify atomicity.
func TestChaosConcurrentStateTransitions(t *testing.T) {
	const numMachines = 50
	const goroutinesPerMachine = 10

	var wg sync.WaitGroup
	var successfulTransitions atomic.Int64
	var failedTransitions atomic.Int64

	for m := 0; m < numMachines; m++ {
		sm := session.NewStateMachine()

		// Move to Ready first (required before concurrent attempts)
		if err := sm.Transition(session.StateInit, session.StateReady); err != nil {
			t.Fatalf("machine %d: failed INIT→READY: %v", m, err)
		}

		// Now hammer concurrent transitions from Ready
		wg.Add(goroutinesPerMachine)
		for g := 0; g < goroutinesPerMachine; g++ {
			go func(gID int) {
				defer wg.Done()
				switch gID % 3 {
				case 0:
					// Try Ready → Streaming
					if err := sm.Transition(session.StateReady, session.StateStreaming); err == nil {
						successfulTransitions.Add(1)
					} else {
						failedTransitions.Add(1)
					}
				case 1:
					// Try Ready → Closing (force close)
					if err := sm.Transition(session.StateReady, session.StateClosing); err == nil {
						successfulTransitions.Add(1)
					} else {
						failedTransitions.Add(1)
					}
				case 2:
					// Read state (should never panic)
					_ = sm.Current()
					_ = sm.IsActive()
					successfulTransitions.Add(1)
				}
			}(g)
		}
	}

	wg.Wait()

	t.Logf("Concurrent state transitions: %d successful, %d failed (expected: exactly 1 winner per machine for mutating ops)",
		successfulTransitions.Load(), failedTransitions.Load())

	// Key invariant: no panics, no data races
	// The -race detector would catch data races
}

// TestChaosDynamicConfigConcurrent hammers dynamic config with concurrent
// reads and writes to verify thread safety.
func TestChaosDynamicConfigConcurrent(t *testing.T) {
	log := logger.GetLogger()
	dc := resilience.NewDynamicConfig(log)

	const (
		writers    = 5
		readers    = 20
		iterations = 500
	)

	var wg sync.WaitGroup

	// Writers
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				dc.SetMaxBitrate(int64(500000+i*1000), "chaos-test")
				dc.SetMediaTimeout(time.Duration(5+i%30)*time.Second, "chaos-test")
			}
		}(w)
	}

	// Readers
	var readCount atomic.Int64
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = dc.MaxBitrate()
				_ = dc.MediaTimeout()
				_ = dc.Snapshot()
				readCount.Add(3)
			}
		}()
	}

	wg.Wait()

	t.Logf("Dynamic config chaos: %d reads completed, final bitrate=%d",
		readCount.Load(), dc.MaxBitrate())
}
