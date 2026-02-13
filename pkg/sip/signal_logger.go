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
	"math"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
)

const (
	// int16Max is the maximum absolute value for int16 (32768); used for dBFS reference.
	int16Max = 1 << 15

	// DefaultInitialNoiseFloorDB is the initial noise floor in dBFS for adaptive tracking.
	DefaultInitialNoiseFloorDB = -50
	// DefaultHangoverDuration is how long we stay in "signal" after level drops below exit threshold.
	DefaultHangoverDuration = 1 * time.Second
	// DefaultEnterVoiceOffsetDB is the default offset above noise floor to enter voice (hysteresis high).
	DefaultEnterVoiceOffsetDB = 10
	// DefaultExitVoiceOffsetDB is the default offset above noise floor to exit voice (hysteresis low).
	DefaultExitVoiceOffsetDB = 5
	// DefaultNoiseFloorUpdateMaxDB is the default: only adapt noise floor when current level is below noiseFloor + this.
	DefaultNoiseFloorUpdateMaxDB = 5

	// minDBFS clamps very quiet frames to avoid -inf in dBFS.
	minDBFS = -100
)

// SignalLogger keeps internal state of whether we're in voice or silence, using RMS → dBFS,
// an adaptive noise floor, and hysteresis. It implements msdk.PCM16Writer and logs state changes.
type SignalLogger struct {
	// Configuration
	log                   logger.Logger
	next                  msdk.PCM16Writer
	name                  string
	hangoverDuration      time.Duration
	noiseFloor            float64 // Adaptive noise floor in dBFS.
	enterVoiceOffsetDB    float64 // Offset above noise floor to enter voice (hysteresis high).
	exitVoiceOffsetDB     float64 // Offset above noise floor to exit voice (hysteresis low).
	noiseFloorUpdateMaxDB float64 // Only adapt noise floor when current level is below noiseFloor + this.

	// State
	lastSignalTime time.Time
	lastIsSignal   bool

	// Stats
	framesProcessed uint64
	stateChanges    uint64
}

type SignalLoggerOption func(*SignalLogger) error

// WithNoiseFloor sets the initial noise floor in dBFS (e.g. -40). Adaptive updates apply after.
func WithNoiseFloor(noiseFloorDB float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		s.noiseFloor = max(noiseFloorDB, -60) // anything below -60 dBFS is almost never realistic for a live phone line
		return nil
	}
}

func WithHangoverDuration(hangoverDuration time.Duration) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if hangoverDuration <= 0 {
			return fmt.Errorf("hangover duration must be positive, got %s", hangoverDuration)
		}
		s.hangoverDuration = hangoverDuration
		return nil
	}
}

// WithEnterVoiceOffsetDB sets the offset (dB) above noise floor to enter voice. Default is DefaultEnterVoiceOffsetDB.
func WithEnterVoiceOffsetDB(db float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if db <= 0 {
			return fmt.Errorf("enterVoiceOffsetDB must be positive, got %g", db)
		}
		s.enterVoiceOffsetDB = db
		return nil
	}
}

// WithExitVoiceOffsetDB sets the offset (dB) above noise floor to exit voice. Default is DefaultExitVoiceOffsetDB.
func WithExitVoiceOffsetDB(db float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if db <= 0 {
			return fmt.Errorf("exitVoiceOffsetDB must be positive, got %g", db)
		}
		s.exitVoiceOffsetDB = db
		return nil
	}
}

// WithNoiseFloorUpdateMaxDB sets the max offset (dB) above noise floor for adaptation; only update when current < noiseFloor+this. Default is DefaultNoiseFloorUpdateMaxDB.
func WithNoiseFloorUpdateMaxDB(db float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if db <= 0 {
			return fmt.Errorf("noiseFloorUpdateMaxDB must be positive, got %g", db)
		}
		s.noiseFloorUpdateMaxDB = db
		return nil
	}
}

func NewSignalLogger(log logger.Logger, name string, next msdk.PCM16Writer, options ...SignalLoggerOption) (msdk.PCM16Writer, error) {
	s := &SignalLogger{
		log:                   log,
		next:                  next,
		name:                  name,
		hangoverDuration:      DefaultHangoverDuration,
		noiseFloor:            DefaultInitialNoiseFloorDB,
		enterVoiceOffsetDB:    DefaultEnterVoiceOffsetDB,
		exitVoiceOffsetDB:     DefaultExitVoiceOffsetDB,
		noiseFloorUpdateMaxDB: DefaultNoiseFloorUpdateMaxDB,
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

// rmsToDBFS computes RMS of the frame then converts to dBFS: 10*log10(mean(square)/MAX^2).
// Uses int16Max (32768) as reference. Returns a value <= 0; silence approaches -inf, so we clamp to minDBFS.
func (s *SignalLogger) rmsToDBFS(sample msdk.PCM16Sample, minDBFS float64) float64 {
	if len(sample) == 0 {
		return minDBFS
	}
	var sumSq int64
	for _, v := range sample {
		x := int64(v)
		sumSq += x * x
	}
	meanSq := float64(sumSq) / float64(len(sample))
	refSq := float64(int16Max) * float64(int16Max)
	if meanSq <= 0 {
		return minDBFS
	}
	db := 10 * math.Log10(meanSq/refSq)
	if db < minDBFS {
		return minDBFS
	}
	return db
}

// updateNoiseFloor adapts the noise floor only when current_dB < noiseFloor + noiseFloorUpdateMaxDB.
func (s *SignalLogger) updateNoiseFloor(currentDB float64) {
	if currentDB < s.noiseFloor+s.noiseFloorUpdateMaxDB {
		s.noiseFloor = 0.95*s.noiseFloor + 0.05*currentDB
	}
}

func (s *SignalLogger) WriteSample(sample msdk.PCM16Sample) error {
	currentDB := s.rmsToDBFS(sample, minDBFS)

	s.updateNoiseFloor(currentDB)

	enterThreshold := s.noiseFloor + s.enterVoiceOffsetDB
	exitThreshold := s.noiseFloor + s.exitVoiceOffsetDB

	now := time.Now()
	aboveEnter := currentDB > enterThreshold
	belowExit := currentDB < exitThreshold

	if aboveEnter {
		s.lastSignalTime = now
	}

	s.framesProcessed++
	if s.framesProcessed <= 10 {
		s.lastIsSignal = aboveEnter
		return s.next.WriteSample(sample)
	}

	if aboveEnter && !s.lastIsSignal {
		s.lastIsSignal = true
		s.stateChanges++
		s.log.Infow("signal changed", "name", s.name, "signal", true, "stateChanges", s.stateChanges, "dBFS", currentDB, "noiseFloor", s.noiseFloor)
	} else if belowExit && s.lastIsSignal {
		if now.Sub(s.lastSignalTime) >= s.hangoverDuration {
			s.lastIsSignal = false
			s.stateChanges++
			s.log.Infow("signal changed", "name", s.name, "signal", false, "stateChanges", s.stateChanges, "dBFS", currentDB, "noiseFloor", s.noiseFloor)
		}
	}

	return s.next.WriteSample(sample)
}
