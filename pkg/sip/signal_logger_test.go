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
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// mockPCM16Writer is a simple mock implementation of PCM16Writer for testing
type mockPCM16Writer struct {
	sampleRate int
	samples    []msdk.PCM16Sample
	closed     atomic.Bool
}

func newMockPCM16Writer(sampleRate int) *mockPCM16Writer {
	return &mockPCM16Writer{
		sampleRate: sampleRate,
		samples:    make([]msdk.PCM16Sample, 0),
	}
}

func (m *mockPCM16Writer) String() string {
	return "mockPCM16Writer"
}

func (m *mockPCM16Writer) SampleRate() int {
	return m.sampleRate
}

func (m *mockPCM16Writer) Close() error {
	m.closed.Store(true)
	return nil
}

func (m *mockPCM16Writer) WriteSample(sample msdk.PCM16Sample) error {
	m.samples = append(m.samples, sample)
	return nil
}

func TestSignalLogger_initialization(t *testing.T) {
	log := logger.GetLogger()
	next := newMockPCM16Writer(48000)

	t.Run("default initialization", func(t *testing.T) {
		out, err := NewSignalLogger(log, "incoming", next)
		sl, ok := out.(*SignalLogger)
		require.True(t, ok)
		require.NoError(t, err)
		require.NotNil(t, sl)
		require.Equal(t, DefaultSignalMultiplier, sl.signalMultiplier)
		require.Equal(t, DefaultNoiseFloor, sl.noiseFloor)
		require.Equal(t, DefaultHangoverDuration, sl.hangoverDuration)
	})

	t.Run("with valid options", func(t *testing.T) {
		out, err := NewSignalLogger(log, "incoming", next, WithSignalMultiplier(3.0), WithNoiseFloor(100), WithHangoverDuration(time.Second))
		sl, ok := out.(*SignalLogger)
		require.True(t, ok)
		require.NoError(t, err)
		require.NotNil(t, sl)
		require.Equal(t, 3.0, sl.signalMultiplier)
		require.Equal(t, 100.0, sl.noiseFloor)
		require.Equal(t, time.Second, sl.hangoverDuration)
	})

	t.Run("with invalid options", func(t *testing.T) {
		_, err := NewSignalLogger(log, "incoming", next, WithSignalMultiplier(0.5))
		require.Error(t, err)
		require.Contains(t, err.Error(), "signal multiplier must be greater than 1")
		_, err = NewSignalLogger(log, "incoming", next, WithNoiseFloor(0))
		require.Error(t, err)
		require.Contains(t, err.Error(), "noise floor min must be positive")
		_, err = NewSignalLogger(log, "incoming", next, WithHangoverDuration(-time.Second))
		require.Error(t, err)
		require.Contains(t, err.Error(), "hangover duration must be positive")
	})
}

func TestSignalLogger_MeanAbsoluteDeviation(t *testing.T) {
	log := logger.GetLogger()
	next := newMockPCM16Writer(48000)
	out, err := NewSignalLogger(log, "incoming", next)
	sl, ok := out.(*SignalLogger)
	require.True(t, ok)
	require.NoError(t, err)

	tests := []struct {
		name     string
		sample   msdk.PCM16Sample
		expected float64
	}{
		{
			name:     "empty sample",
			sample:   msdk.PCM16Sample{},
			expected: 0.0,
		},
		{
			name:     "all zeros",
			sample:   msdk.PCM16Sample{0, 0, 0, 0},
			expected: 0.0,
		},
		{
			name:     "all positive",
			sample:   msdk.PCM16Sample{100, 200, 300, 400},
			expected: 250.0,
		},
		{
			name:     "all negative",
			sample:   msdk.PCM16Sample{-100, -200, -300, -400},
			expected: 250.0,
		},
		{
			name:     "mixed positive and negative",
			sample:   msdk.PCM16Sample{-100, 200, -300, 400},
			expected: 250.0,
		},
		{
			name:     "single value",
			sample:   msdk.PCM16Sample{1000},
			expected: 1000.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sl.MeanAbsoluteDeviation(tt.sample)
			require.InDelta(t, tt.expected, result, 0.01)
		})
	}
}

func newTestLogger(t *testing.T, opts ...SignalLoggerOption) (*SignalLogger, *mockPCM16Writer) {
	next := newMockPCM16Writer(48000)
	out, err := NewSignalLogger(logger.GetLogger(), "incoming", next, opts...)
	sl, ok := out.(*SignalLogger)
	require.True(t, ok)
	require.NoError(t, err)
	return sl, next
}

func writer(t *testing.T, sl *SignalLogger, array []msdk.PCM16Sample, count int, wait bool) error {
	for i := 0; i < count; i++ {
		randIndex := rand.Uint() % uint(len(array))
		if err := sl.WriteSample(array[randIndex]); err != nil {
			return err
		}
		since := time.Since(sl.lastSignalTime).Milliseconds()
		t.Logf("%d written sample %d/%d: amplitude %d, noise floor %f, state changes %d, last signal %t (%dms ago)\n", sl.framesProcessed, randIndex, len(array), array[randIndex][0], sl.noiseFloor, sl.stateChanges, sl.lastIsSignal, since)
		if wait {
			time.Sleep(rtp.DefFrameDur)
		}
	}
	return nil
}

func testTransition(t *testing.T, first, second []msdk.PCM16Sample, firstCount, secondCount int, wait bool, opts ...SignalLoggerOption) *SignalLogger {
	sl, _ := newTestLogger(t, opts...)
	require.NoError(t, writer(t, sl, first, firstCount, wait))
	require.Equal(t, uint64(firstCount), sl.framesProcessed)
	require.NoError(t, writer(t, sl, second, secondCount, wait))
	require.Equal(t, uint64(firstCount+secondCount), sl.framesProcessed)
	return sl
}

func createFrame(size int, amplitude int16) msdk.PCM16Sample {
	frame := make(msdk.PCM16Sample, size)
	for i := range frame {
		frame[i] = amplitude
	}
	return frame
}

func TestSignalLogger_WriteSample(t *testing.T) {

	silenceFrames := make([]msdk.PCM16Sample, 100)
	for i := range silenceFrames {
		amplitude := int16(rand.Uint32()) % int16(DefaultNoiseFloor)
		silenceFrames[i] = createFrame(480, amplitude)
	}

	signalFrames := make([]msdk.PCM16Sample, 100)
	for i := range signalFrames {
		amplitude := (int16(rand.Uint32()) % 1000)
		// Adding (DefaultNoiseFloorMax * DefaultSignalMultiplier) prevents signal from being detected as silence when loud for too long.
		if amplitude < 0 {
			amplitude -= int16(DefaultNoiseFloor * DefaultSignalMultiplier)
		} else {
			amplitude += int16(DefaultNoiseFloor * DefaultSignalMultiplier)
		}
		signalFrames[i] = createFrame(480, amplitude)
	}

	t.Run("not_printing_on_first_10_frames", func(t *testing.T) {
		sl, _ := newTestLogger(t)
		sl.noiseFloor = 40

		require.NoError(t, writer(t, sl, silenceFrames, 5, false))
		require.NoError(t, writer(t, sl, signalFrames, 3, false))
		require.NoError(t, writer(t, sl, silenceFrames, 2, false))
		require.Equal(t, uint64(10), sl.framesProcessed)
		require.Equal(t, uint64(0), sl.stateChanges)
	})

	t.Run("printing_on_11th_frame_transition", func(t *testing.T) {
		t.Run("silence_to_signal", func(t *testing.T) {
			sl := testTransition(t, silenceFrames, signalFrames, 10, 1, false)
			require.Equal(t, uint64(1), sl.stateChanges)
		})
		t.Run("signal_to_silence", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sl := testTransition(t, signalFrames, silenceFrames, 10, 50, true)
				require.Equal(t, uint64(1), sl.stateChanges)
			})
		})
	})

	t.Run("silence_to_silence_transitions", func(t *testing.T) {
		sl := testTransition(t, silenceFrames, silenceFrames, 10, 0, false) // Not too long, it will eventually bring noise floor low enough
		require.Equal(t, uint64(0), sl.stateChanges)
		require.Equal(t, false, sl.lastIsSignal)
	})

	t.Run("signal_to_signal_transitions", func(t *testing.T) {
		sl := testTransition(t, signalFrames, signalFrames, 10, 0, false)
		require.Equal(t, uint64(0), sl.stateChanges)
		require.Equal(t, true, sl.lastIsSignal)
	})

	t.Run("silence_to_signal_transitions", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			t.Run(fmt.Sprintf("silence_to_signal_transition_%d", i), func(t *testing.T) {
				sl := testTransition(t, silenceFrames, signalFrames, 10, 1, false)
				require.Equal(t, uint64(1), sl.stateChanges)
				require.Equal(t, true, sl.lastIsSignal)
			})
		}
	})

	t.Run("signal_to_silence_transitions", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			t.Run(fmt.Sprintf("signal_to_silence_transition_%d", i), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					sl := testTransition(t, signalFrames, silenceFrames, 10, 50, true)
					require.Equal(t, uint64(1), sl.stateChanges)
					require.Equal(t, false, sl.lastIsSignal)
				})
			})
		}
	})

}
