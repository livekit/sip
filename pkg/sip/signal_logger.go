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

package sip

import (
	"fmt"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
)

const (
	DefaultSignalMultiplier = float64(2)      // Signal needs to be at least 2 times the noise floor to be detected.
	DefaultNoiseFloor       = float64(25)     // Use a low noise floor; The purpose is to detect any kind of signal, not just voice.
	DefaultHangoverDuration = 1 * time.Second // Duration of silence that needs to elapse before we no longer consider it a signal.
)

// Keeps an internal state of whether we're currently transmitting signal (voice or noise), or silence.
// This implements msdk.PCM16Writer to inspect decoded packet content.
// Used to log changes betweem those states.
type SignalLogger struct {
	// Configuration
	log              logger.Logger
	next             msdk.PCM16Writer
	name             string
	hangoverDuration time.Duration // Time to stay in "signal" state after last signal to avoid flip-flopping.
	signalMultiplier float64       // Threshold multiplier for signal to be detected.
	noiseFloor       float64       // Moving average of noise floor.

	// State
	lastSignalTime time.Time
	lastIsSignal   bool

	// Stats
	framesProcessed uint64
	stateChanges    uint64
}

type SignalLoggerOption func(*SignalLogger) error

func WithSignalMultiplier(signalMultiplier float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if signalMultiplier <= 1 {
			return fmt.Errorf("signal multiplier must be greater than 1")
		}
		s.signalMultiplier = signalMultiplier
		return nil
	}
}

func WithNoiseFloor(noiseFloor float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if noiseFloor <= 0 {
			return fmt.Errorf("noise floor min must be positive")
		}
		s.noiseFloor = noiseFloor
		return nil
	}
}

func WithHangoverDuration(hangoverDuration time.Duration) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if hangoverDuration <= 0 {
			return fmt.Errorf("hangover duration must be positive")
		}
		s.hangoverDuration = hangoverDuration
		return nil
	}
}
func NewSignalLogger(log logger.Logger, name string, next msdk.PCM16Writer, options ...SignalLoggerOption) (msdk.PCM16Writer, error) {
	s := &SignalLogger{
		log:              log,
		next:             next,
		name:             name,
		hangoverDuration: DefaultHangoverDuration,
		signalMultiplier: DefaultSignalMultiplier,
		noiseFloor:       DefaultNoiseFloor,
	}
	for _, option := range options {
		if err := option(s); err != nil {
			return next, err
		}
	}
	return s, nil
}

func (s *SignalLogger) String() string {
	return fmt.Sprintf("SignalLogger(%s) -> %s", s.name, s.next.String())
}

func (s *SignalLogger) SampleRate() int {
	return s.next.SampleRate()
}

func (s *SignalLogger) Close() error {
	if s.stateChanges > 0 {
		s.log.Infow("signal logger closing", "name", s.name, "stateChanges", s.stateChanges)
	}
	return s.next.Close()
}

// Calculates the mean absolute deviation of the frame.
func (s *SignalLogger) MeanAbsoluteDeviation(sample msdk.PCM16Sample) float64 {
	if len(sample) == 0 {
		return 0
	}
	var totalAbs int64
	for _, v := range sample {
		if v < 0 {
			totalAbs += int64(-v)
		} else {
			totalAbs += int64(v)
		}
	}
	return float64(totalAbs) / float64(len(sample))
}

func (s *SignalLogger) WriteSample(sample msdk.PCM16Sample) error {
	currentEnergy := s.MeanAbsoluteDeviation(sample)

	now := time.Now()
	isSignal := currentEnergy > (s.noiseFloor * s.signalMultiplier)
	if isSignal {
		s.lastSignalTime = now
	}

	s.framesProcessed++
	if s.framesProcessed > 10 { // Don't log any changes at first
		if isSignal && isSignal != s.lastIsSignal { // silence -> signal: Immediate transition
			s.lastIsSignal = true
			s.stateChanges++
			s.log.Infow("signal changed", "name", s.name, "signal", isSignal, "stateChanges", s.stateChanges)
		} else if !isSignal && isSignal != s.lastIsSignal { // signal -> silence: Only after hangover
			if now.Sub(s.lastSignalTime) >= s.hangoverDuration {
				s.lastIsSignal = false
				s.stateChanges++
				s.log.Infow("signal changed", "name", s.name, "signal", isSignal, "stateChanges", s.stateChanges)
			}
		}
	} else {
		s.lastIsSignal = isSignal
	}
	return s.next.WriteSample(sample)
}
