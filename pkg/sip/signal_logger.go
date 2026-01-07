package sip

import (
	"fmt"
	"sync/atomic"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
)

const (
	DefaultSignalMultiplier = float64(2)     // Singnal needs to be at least 2 times the noise floor to be detected.
	DefaultDecayAlpha       = float64(0.95)  // 5% of new silence is added to the noise floor.
	DefaultAttackAlpha      = float64(0.999) // 0.1% of new signal is added to the noise floor.
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
	framesProcessed  uint64
}

type NewSignalLoggerOption func(*SignalLogger)

func WithSignalMultiplier(signalMultiplier float64) NewSignalLoggerOption {
	return func(s *SignalLogger) {
		if signalMultiplier < 0 {
			signalMultiplier = -signalMultiplier
		}
		if signalMultiplier > 1 {
			s.signalMultiplier = signalMultiplier
		}
	}
}

func WithDecayAlpha(alpha float64) NewSignalLoggerOption {
	return func(s *SignalLogger) {
		if alpha < 0 {
			alpha = -alpha
		}
		if alpha > 1 {
			alpha = 1
		}
		s.decayAlpha = alpha
	}
}

func WithAttackAlpha(alpha float64) NewSignalLoggerOption {
	return func(s *SignalLogger) {
		if alpha < 0 {
			alpha = -alpha
		}
		if alpha > 1 {
			alpha = 1
		}
		s.attackAlpha = alpha
	}
}

func NewSignalLogger(log logger.Logger, direction string, alpha float64, next msdk.PCM16Writer) *SignalLogger {
	s := &SignalLogger{
		log:              log,
		direction:        direction,
		next:             next,
		isLastSignal:     0,
		framesProcessed:  0,
		signalMultiplier: DefaultSignalMultiplier,
		decayAlpha:       DefaultDecayAlpha,
		attackAlpha:      DefaultAttackAlpha,
		noiseFloor:       0.0,
	}
	return s
}

func (s *SignalLogger) String() string {
	return fmt.Sprintf("SignalLogger(%s) -> %s", s.direction, s.next.String())
}

func (s *SignalLogger) SampleRate() int {
	return s.next.SampleRate()
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
func (s *SignalLogger) UpdateNoiseFloor(currentEnergy float64, isSignal int32) {
	alpha := s.attackAlpha
	if isSignal == 0 {
		alpha = s.decayAlpha
	}
	s.noiseFloor = (alpha * s.noiseFloor) + ((1 - alpha) * currentEnergy)
}

func (s *SignalLogger) WriteSample(sample msdk.PCM16Sample) error {
	currentEnergy := s.MeanAbsoluteDeviation(sample)
	isSignal := int32(0)
	if currentEnergy > (s.noiseFloor * s.signalMultiplier) {
		isSignal = 1
	}
	s.UpdateNoiseFloor(currentEnergy, isSignal)
	atomic.AddUint64(&s.framesProcessed, 1)
	lastSignal := atomic.SwapInt32(&s.isLastSignal, isSignal)
	if lastSignal != isSignal && atomic.LoadUint64(&s.framesProcessed) > 10 {
		s.log.Infow("signal changed", "direction", s.direction, "signal", isSignal)
	}
	return s.next.WriteSample(sample)
}
