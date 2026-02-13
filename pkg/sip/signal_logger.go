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
	// DefaultInitialNoiseFloorDB is the default noise floor in dBFS.
	DefaultInitialNoiseFloorDB = -50
	// DefaultHangoverDuration is how long we stay in "signal" after level drops below exit threshold.
	DefaultHangoverDuration = 1 * time.Second
	// DefaultEnterVoiceOffsetDB is the default offset above noise floor to enter voice (hysteresis high).
	DefaultEnterVoiceOffsetDB = 10
	// DefaultExitVoiceOffsetDB is the default offset above noise floor to exit voice (hysteresis low).
	DefaultExitVoiceOffsetDB = 5

	// minDBFS clamps very quiet frames to avoid -inf in dBFS.
	minDBFS = -100
)

// SignalLogger keeps internal state of whether we're in voice or silence, using RMS → dBFS
// and a fixed noise floor with hysteresis. It implements msdk.PCM16Writer and logs state changes.
type SignalLogger struct {
	// Configuration
	log                logger.Logger
	next               msdk.PCM16Writer
	name               string
	hangoverDuration   time.Duration
	noiseFloor         float64 // Noise floor in dBFS (fixed, not adaptive).
	enterVoiceOffsetDB float64 // Offset above noise floor to enter voice (hysteresis high).
	exitVoiceOffsetDB  float64 // Offset above noise floor to exit voice (hysteresis low).

	// State
	lastSignalTime time.Time
	lastIsSignal   bool

	// Stats
	framesProcessed uint64
	stateChanges    uint64
}

type SignalLoggerOption func(*SignalLogger) error

// WithNoiseFloor sets the noise floor in dBFS (e.g. -40). Must be >= minDBFS.
func WithNoiseFloor(noiseFloorDB float64) SignalLoggerOption {
	return func(s *SignalLogger) error {
		if noiseFloorDB < minDBFS {
			return fmt.Errorf("noise floor must be >= %g dBFS, got %g", float64(minDBFS), noiseFloorDB)
		}
		s.noiseFloor = noiseFloorDB
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

func NewSignalLogger(log logger.Logger, name string, next msdk.PCM16Writer, options ...SignalLoggerOption) (msdk.PCM16Writer, error) {
	s := &SignalLogger{
		log:                log,
		next:               next,
		name:               name,
		hangoverDuration:   DefaultHangoverDuration,
		noiseFloor:         DefaultInitialNoiseFloorDB,
		enterVoiceOffsetDB: DefaultEnterVoiceOffsetDB,
		exitVoiceOffsetDB:  DefaultExitVoiceOffsetDB,
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
// Uses math.MaxInt16 (32767) as reference. Returns a value <= 0; silence approaches -inf, so we clamp to minDBFS.
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
	refSq := float64(math.MaxInt16) * float64(math.MaxInt16)
	if meanSq <= 0 {
		return minDBFS
	}
	db := 10 * math.Log10(meanSq/refSq)
	if db < minDBFS {
		return minDBFS
	}
	return db
}

func (s *SignalLogger) WriteSample(sample msdk.PCM16Sample) error {
	currentDB := s.rmsToDBFS(sample, minDBFS)

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
