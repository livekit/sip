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
	"sync/atomic"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
)

const (
	DefaultSignalMultiplier = float64(2)       // Singnal needs to be at least 2 times the noise floor to be detected.
	DefaultDecayAlpha       = float64(0.95)    // 5% of new silence is added to the noise floor.
	DefaultAttackAlpha      = float64(0.999)   // 0.1% of new signal is added to the noise floor.
	DefaultNoiseFloorMin    = float64(30)      // Minimum noise floor. Useful when mic changes to avoid false positives.
	DefaultNoiseFloorMax    = float64(300)     // Maximum noise floor. Would always detect signal in very noisy environments, but that's okay.
	AlphaMin                = float64(0.1)     // Minimum alpha.
	AlphaMax                = float64(0.99999) // Maximum alpha.
)

// Keeps an internal state of whether we're currently transmitting signal (voice or noise), or silence.
// This implements msdk.PCM16Writer to inspect decoded packet content.
// Used to log changes betweem those states.
type SignalLogger struct {
	log              logger.Logger
	next             msdk.PCM16Writer
	direction        string
	isLastSignal     int32
	decayAlpha       float64 // Weight of previous noise floor when updating silence noise floor.
	attackAlpha      float64 // Weight of previous noise floor when updating signal noise floor.
	signalMultiplier float64 // Threshold multiplier for signal to be detected.
	noiseFloor       float64 // Moveing average of noise floor.
	noiseFloorMin    float64 // Minimum noise floor.
	noiseFloorMax    float64 // Maximum noise floor.
	framesProcessed  uint64
	stateChanges     uint64
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

func WithDecayAlpha(alpha float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if alpha < AlphaMin || alpha > AlphaMax {
			return fmt.Errorf("decay alpha must be between %f and %f", AlphaMin, AlphaMax)
		}
		s.decayAlpha = alpha
		return nil
	}
}

func WithAttackAlpha(alpha float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if alpha < AlphaMin || alpha > AlphaMax {
			return fmt.Errorf("attack alpha must be between %f and %f", AlphaMin, AlphaMax)
		}
		s.attackAlpha = alpha
		return nil
	}
}

func WithNoiseFloorMax(noiseFloorMax float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if noiseFloorMax <= 0 {
			return fmt.Errorf("noise floor max must be greater than 0")
		}
		s.noiseFloorMax = noiseFloorMax
		return nil
	}
}

func WithNoiseFloorMin(noiseFloorMin float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if noiseFloorMin < 0 {
			return fmt.Errorf("noise floor min must be non-negative")
		}
		s.noiseFloorMin = noiseFloorMin
		return nil
	}
}
func NewSignalLogger(log logger.Logger, direction string, next msdk.PCM16Writer, options ...SignalLoggerOption) (*SignalLogger, error) {
	s := &SignalLogger{
		log:              log,
		direction:        direction,
		next:             next,
		isLastSignal:     0,
		framesProcessed:  0,
		stateChanges:     0,
		signalMultiplier: DefaultSignalMultiplier,
		decayAlpha:       DefaultDecayAlpha,
		attackAlpha:      DefaultAttackAlpha,
		noiseFloorMax:    DefaultNoiseFloorMax,
		noiseFloorMin:    DefaultNoiseFloorMin,
		noiseFloor:       0.0,
	}
	for _, option := range options {
		if err := option(s); err != nil {
			return nil, err
		}
	}
	s.noiseFloor = (s.noiseFloorMax + s.noiseFloorMin) / 2
	return s, nil
}

func (s *SignalLogger) String() string {
	return fmt.Sprintf("SignalLogger(%s) -> %s", s.direction, s.next.String())
}

func (s *SignalLogger) SampleRate() int {
	return s.next.SampleRate()
}

func (s *SignalLogger) Close() error {
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

// Updates the noise floor using moving average.
func (s *SignalLogger) updateNoiseFloor(currentEnergy float64, isSignal int32) {
	alpha := s.decayAlpha
	if isSignal != 0 {
		alpha = s.attackAlpha
	}
	s.noiseFloor = (alpha * s.noiseFloor) + ((1 - alpha) * currentEnergy)
	s.noiseFloor = min(s.noiseFloor, s.noiseFloorMax)
	s.noiseFloor = max(s.noiseFloor, s.noiseFloorMin)
}

func (s *SignalLogger) WriteSample(sample msdk.PCM16Sample) error {
	currentEnergy := s.MeanAbsoluteDeviation(sample)
	lastSignal := atomic.LoadInt32(&s.isLastSignal)
	signalMultiplier := s.signalMultiplier
	if lastSignal == 1 {
		signalMultiplier *= 0.9 // Reduce signal multiplier when last signal was signal, to avoid flip-flopping.
	}
	isSignal := int32(0)
	if currentEnergy > (s.noiseFloor * signalMultiplier) {
		isSignal = 1
	}
	s.updateNoiseFloor(currentEnergy, isSignal)
	atomic.AddUint64(&s.framesProcessed, 1)
	lastSignal = atomic.SwapInt32(&s.isLastSignal, isSignal)
	if lastSignal != isSignal && atomic.LoadUint64(&s.framesProcessed) > 10 {
		stateChanges := atomic.AddUint64(&s.stateChanges, 1)
		s.log.Infow("signal changed", "direction", s.direction, "signal", isSignal, "stateChanges", stateChanges)
	}
	return s.next.WriteSample(sample)
}
