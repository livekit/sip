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
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// RetryConfig configures the retry strategy.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts (including the first). 0 = no retry.
	MaxAttempts int
	// InitialDelay is the base delay before the first retry.
	InitialDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
	// Multiplier scales the delay after each attempt (default 2.0).
	Multiplier float64
	// Jitter adds randomness to prevent thundering herd. Range: 0.0-1.0.
	Jitter float64
	// RetryableCheck is an optional function to decide if an error is retryable.
	// If nil, all errors are retried.
	RetryableCheck func(err error) bool
}

// DefaultRetryConfig returns a sensible default for media pipeline retries.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:  3,
		InitialDelay: 500 * time.Millisecond,
		MaxDelay:     10 * time.Second,
		Multiplier:   2.0,
		Jitter:       0.2,
	}
}

// RoomJoinRetryConfig returns a retry config tuned for LiveKit room joins.
func RoomJoinRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:  5,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
		Jitter:       0.3,
	}
}

// TranscoderRetryConfig returns a retry config for transcoder subprocess restarts.
func TranscoderRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:  3,
		InitialDelay: 200 * time.Millisecond,
		MaxDelay:     5 * time.Second,
		Multiplier:   3.0,
		Jitter:       0.1,
	}
}

// Do executes fn with retry logic. Returns the last error if all attempts fail.
func Do(ctx context.Context, log logger.Logger, name string, conf RetryConfig, fn func(ctx context.Context) error) error {
	if conf.MaxAttempts <= 0 {
		conf.MaxAttempts = 1
	}
	if conf.Multiplier <= 0 {
		conf.Multiplier = 2.0
	}
	if conf.InitialDelay <= 0 {
		conf.InitialDelay = 500 * time.Millisecond
	}

	var lastErr error
	delay := conf.InitialDelay

	for attempt := 1; attempt <= conf.MaxAttempts; attempt++ {
		lastErr = fn(ctx)
		if lastErr == nil {
			if attempt > 1 {
				log.Infow("retry succeeded",
					"operation", name,
					"attempt", attempt,
				)
			}
			return nil
		}

		// Check if error is retryable
		if conf.RetryableCheck != nil && !conf.RetryableCheck(lastErr) {
			log.Warnw("non-retryable error",
				lastErr,
				"operation", name,
				"attempt", attempt,
			)
			return lastErr
		}

		// Last attempt — don't sleep
		if attempt == conf.MaxAttempts {
			break
		}

		// Log and wait
		log.Warnw("operation failed, retrying",
			lastErr,
			"operation", name,
			"attempt", attempt,
			"maxAttempts", conf.MaxAttempts,
			"nextDelay", delay,
		)
		stats.SessionErrors.WithLabelValues("retry_" + name).Inc()

		// Sleep with context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: context cancelled during retry: %w", name, ctx.Err())
		case <-time.After(addJitter(delay, conf.Jitter)):
		}

		// Exponential backoff
		delay = time.Duration(float64(delay) * conf.Multiplier)
		if conf.MaxDelay > 0 && delay > conf.MaxDelay {
			delay = conf.MaxDelay
		}
	}

	stats.SessionErrors.WithLabelValues("retry_exhausted_" + name).Inc()
	return fmt.Errorf("%s: all %d attempts failed: %w", name, conf.MaxAttempts, lastErr)
}

// DoWithResult executes fn with retry logic and returns both the result and error.
func DoWithResult[T any](ctx context.Context, log logger.Logger, name string, conf RetryConfig, fn func(ctx context.Context) (T, error)) (T, error) {
	var result T
	err := Do(ctx, log, name, conf, func(ctx context.Context) error {
		var fnErr error
		result, fnErr = fn(ctx)
		return fnErr
	})
	return result, err
}

func addJitter(d time.Duration, jitter float64) time.Duration {
	if jitter <= 0 {
		return d
	}
	jitterRange := float64(d) * math.Min(jitter, 1.0)
	return d + time.Duration(rand.Float64()*jitterRange)
}
