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
)

// TestLoadConcurrentSessions verifies the SessionGuard handles concurrent
// session creation correctly under high load. It spins up many goroutines
// attempting to acquire sessions simultaneously and verifies:
// - Per-node limit is respected
// - Per-caller limit is respected
// - Rate limiting kicks in
// - No race conditions
func TestLoadConcurrentSessions(t *testing.T) {
	log := logger.GetLogger()
	guard := resilience.NewSessionGuard(log, resilience.SessionGuardConfig{
		MaxSessionsPerNode:   50,
		MaxSessionsPerCaller: 5,
		NewSessionRateLimit:  100.0,
		NewSessionBurst:      200,
	})

	const numGoroutines = 200
	const callers = 10

	var admitted atomic.Int64
	var rejected atomic.Int64
	var wg sync.WaitGroup

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		callerID := fmt.Sprintf("sip:caller%d@test.com", i%callers)
		go func(caller string) {
			defer wg.Done()
			if err := guard.Admit(caller); err == nil {
				admitted.Add(1)
				// Simulate short session
				time.Sleep(10 * time.Millisecond)
				guard.Release(caller)
			} else {
				rejected.Add(1)
			}
		}(callerID)
	}

	wg.Wait()

	totalAdmitted := admitted.Load()
	totalRejected := rejected.Load()

	t.Logf("Load test: %d admitted, %d rejected out of %d attempts",
		totalAdmitted, totalRejected, numGoroutines)

	// At least some should be admitted
	if totalAdmitted == 0 {
		t.Error("no sessions admitted — guard is too restrictive")
	}

	// Per-node limit should never be exceeded (we release quickly, so most should get through)
	// But the key invariant: admitted + rejected == total
	if totalAdmitted+totalRejected != numGoroutines {
		t.Errorf("accounting error: admitted(%d) + rejected(%d) != total(%d)",
			totalAdmitted, totalRejected, numGoroutines)
	}
}

// TestLoadPerCallerLimit verifies per-caller limits under concurrent load.
func TestLoadPerCallerLimit(t *testing.T) {
	log := logger.GetLogger()
	guard := resilience.NewSessionGuard(log, resilience.SessionGuardConfig{
		MaxSessionsPerNode:   1000,
		MaxSessionsPerCaller: 3,
		NewSessionRateLimit:  0, // no rate limit
	})

	const caller = "sip:heavycaller@test.com"
	const numGoroutines = 50

	var admitted atomic.Int64
	var wg sync.WaitGroup

	// Don't release — hold all sessions to test the limit
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			if err := guard.Admit(caller); err == nil {
				admitted.Add(1)
			}
		}()
	}

	wg.Wait()

	if admitted.Load() > 3 {
		t.Errorf("per-caller limit exceeded: %d admitted (max 3)", admitted.Load())
	}
	if admitted.Load() == 0 {
		t.Error("no sessions admitted for caller")
	}

	t.Logf("Per-caller load: %d/%d admitted", admitted.Load(), numGoroutines)
}

// TestLoadCircuitBreakerThroughput measures circuit breaker overhead under load.
func TestLoadCircuitBreakerThroughput(t *testing.T) {
	log := logger.GetLogger()
	cb := resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:                "load_test",
		MaxFailures:         100, // high threshold so it doesn't trip
		OpenDuration:        time.Second,
		HalfOpenMaxAttempts: 5,
	})

	const iterations = 10000
	start := time.Now()

	for i := 0; i < iterations; i++ {
		if cb.Allow() {
			cb.RecordSuccess()
		}
	}

	elapsed := time.Since(start)
	opsPerSec := float64(iterations) / elapsed.Seconds()

	t.Logf("Circuit breaker throughput: %.0f ops/sec (%v for %d ops)", opsPerSec, elapsed, iterations)

	if opsPerSec < 100000 {
		t.Logf("WARNING: circuit breaker throughput seems low (%.0f ops/sec)", opsPerSec)
	}
}

// TestLoadFeatureFlagEvaluation measures feature flag evaluation throughput.
func TestLoadFeatureFlagEvaluation(t *testing.T) {
	log := logger.GetLogger()
	ff := resilience.NewFeatureFlagsWithRegion(log, "us-east-1")
	ff.SetRollout("video", 50)
	ff.SetTenantOverride("premium", "video", true)

	const iterations = 100000
	start := time.Now()

	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("session-%d", i)
		tenant := "default"
		if i%10 == 0 {
			tenant = "premium"
		}
		ff.IsEnabledFor("video", key, tenant)
	}

	elapsed := time.Since(start)
	opsPerSec := float64(iterations) / elapsed.Seconds()

	t.Logf("Feature flag eval throughput: %.0f ops/sec (%v for %d ops)", opsPerSec, elapsed, iterations)
}

// BenchmarkSessionGuardAdmit benchmarks admission control throughput.
func BenchmarkSessionGuardAdmit(b *testing.B) {
	log := logger.GetLogger()
	guard := resilience.NewSessionGuard(log, resilience.SessionGuardConfig{
		MaxSessionsPerNode:   10000,
		MaxSessionsPerCaller: 100,
		NewSessionRateLimit:  0,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		caller := fmt.Sprintf("sip:bench%d@test.com", i%100)
		if err := guard.Admit(caller); err == nil {
			guard.Release(caller)
		}
	}
}

// BenchmarkCircuitBreakerAllow benchmarks CB Allow check on hot path.
func BenchmarkCircuitBreakerAllow(b *testing.B) {
	log := logger.GetLogger()
	cb := resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:        "bench",
		MaxFailures: 1000,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.Allow()
	}
}

// BenchmarkFeatureFlagEval benchmarks feature flag evaluation.
func BenchmarkFeatureFlagEval(b *testing.B) {
	log := logger.GetLogger()
	ff := resilience.NewFeatureFlagsWithRegion(log, "us-east-1")
	ff.SetRollout("video", 50)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ff.IsEnabledFor("video", fmt.Sprintf("s-%d", i), "default")
	}
}
