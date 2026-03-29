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

// CircuitState represents the state of a circuit breaker.
type CircuitState int32

const (
	StateClosed   CircuitState = 0 // normal operation, requests pass through
	StateOpen     CircuitState = 1 // failures exceeded threshold, requests are blocked
	StateHalfOpen CircuitState = 2 // testing if the service has recovered
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// CircuitBreakerConfig configures the circuit breaker.
type CircuitBreakerConfig struct {
	// Name identifies this circuit breaker in logs and metrics.
	Name string
	// MaxFailures is the number of consecutive failures before opening the circuit.
	MaxFailures int
	// OpenDuration is how long the circuit stays open before transitioning to half-open.
	OpenDuration time.Duration
	// HalfOpenMaxAttempts is how many test requests are allowed in half-open state.
	HalfOpenMaxAttempts int
	// OnStateChange is called when the circuit breaker changes state.
	OnStateChange func(from, to CircuitState)
}

// CircuitBreaker implements the circuit breaker pattern for media pipeline components.
// It prevents cascading failures by stopping requests to a failing component
// and periodically testing if it has recovered.
type CircuitBreaker struct {
	log  logger.Logger
	conf CircuitBreakerConfig

	state           atomic.Int32
	mu              sync.Mutex
	consecutiveFail int
	lastFailure     time.Time
	lastStateChange time.Time
	halfOpenCount   int

	// Stats
	totalSuccess atomic.Uint64
	totalFailure atomic.Uint64
	totalRejected atomic.Uint64
	trips        atomic.Uint64 // number of times circuit opened
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(log logger.Logger, conf CircuitBreakerConfig) *CircuitBreaker {
	if conf.MaxFailures <= 0 {
		conf.MaxFailures = 5
	}
	if conf.OpenDuration <= 0 {
		conf.OpenDuration = 10 * time.Second
	}
	if conf.HalfOpenMaxAttempts <= 0 {
		conf.HalfOpenMaxAttempts = 3
	}

	cb := &CircuitBreaker{
		log:  log.WithValues("circuitBreaker", conf.Name),
		conf: conf,
	}
	cb.state.Store(int32(StateClosed))
	cb.lastStateChange = time.Now()

	return cb
}

// Allow checks if a request should be allowed through.
// Returns true if the request can proceed, false if the circuit is open.
func (cb *CircuitBreaker) Allow() bool {
	state := CircuitState(cb.state.Load())

	switch state {
	case StateClosed:
		return true

	case StateOpen:
		cb.mu.Lock()
		defer cb.mu.Unlock()
		// Check if it's time to try half-open
		if time.Since(cb.lastFailure) >= cb.conf.OpenDuration {
			cb.transitionTo(StateHalfOpen)
			cb.halfOpenCount = 0
			return true
		}
		cb.totalRejected.Add(1)
		stats.SessionErrors.WithLabelValues("circuit_rejected_" + cb.conf.Name).Inc()
		return false

	case StateHalfOpen:
		cb.mu.Lock()
		defer cb.mu.Unlock()
		if cb.halfOpenCount < cb.conf.HalfOpenMaxAttempts {
			cb.halfOpenCount++
			return true
		}
		cb.totalRejected.Add(1)
		return false

	default:
		return true
	}
}

// RecordSuccess records a successful operation.
// In half-open state, enough successes will close the circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.totalSuccess.Add(1)

	state := CircuitState(cb.state.Load())
	if state == StateHalfOpen {
		cb.mu.Lock()
		defer cb.mu.Unlock()
		cb.consecutiveFail = 0
		// If we've had enough successful test requests, close the circuit
		if cb.halfOpenCount >= cb.conf.HalfOpenMaxAttempts {
			cb.transitionTo(StateClosed)
		}
	} else if state == StateClosed {
		cb.mu.Lock()
		cb.consecutiveFail = 0
		cb.mu.Unlock()
	}
}

// RecordFailure records a failed operation.
// Enough consecutive failures will open the circuit.
func (cb *CircuitBreaker) RecordFailure(err error) {
	cb.totalFailure.Add(1)

	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFail++
	cb.lastFailure = time.Now()

	state := CircuitState(cb.state.Load())

	switch state {
	case StateClosed:
		if cb.consecutiveFail >= cb.conf.MaxFailures {
			cb.log.Warnw("circuit breaker tripped",
				err,
				"consecutiveFailures", cb.consecutiveFail,
				"threshold", cb.conf.MaxFailures,
			)
			cb.transitionTo(StateOpen)
			cb.trips.Add(1)
			stats.SessionErrors.WithLabelValues("circuit_tripped_" + cb.conf.Name).Inc()
		}

	case StateHalfOpen:
		// Any failure in half-open → back to open
		cb.log.Warnw("circuit breaker re-tripped from half-open", err)
		cb.transitionTo(StateOpen)
		stats.SessionErrors.WithLabelValues("circuit_retripped_" + cb.conf.Name).Inc()
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() CircuitState {
	return CircuitState(cb.state.Load())
}

// Stats returns circuit breaker statistics.
func (cb *CircuitBreaker) Stats() CircuitBreakerStats {
	return CircuitBreakerStats{
		State:         CircuitState(cb.state.Load()).String(),
		TotalSuccess:  cb.totalSuccess.Load(),
		TotalFailure:  cb.totalFailure.Load(),
		TotalRejected: cb.totalRejected.Load(),
		Trips:         cb.trips.Load(),
	}
}

// CircuitBreakerStats holds circuit breaker statistics.
type CircuitBreakerStats struct {
	State         string `json:"state"`
	TotalSuccess  uint64 `json:"total_success"`
	TotalFailure  uint64 `json:"total_failure"`
	TotalRejected uint64 `json:"total_rejected"`
	Trips         uint64 `json:"trips"`
}

// Reset forces the circuit breaker to the closed state.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFail = 0
	cb.halfOpenCount = 0
	cb.transitionTo(StateClosed)
	cb.log.Infow("circuit breaker manually reset")
}

// must be called with mu held
func (cb *CircuitBreaker) transitionTo(to CircuitState) {
	from := CircuitState(cb.state.Load())
	if from == to {
		return
	}

	cb.state.Store(int32(to))
	cb.lastStateChange = time.Now()

	cb.log.Infow("circuit breaker state change",
		"from", from.String(),
		"to", to.String(),
	)

	if cb.conf.OnStateChange != nil {
		go cb.conf.OnStateChange(from, to)
	}
}
