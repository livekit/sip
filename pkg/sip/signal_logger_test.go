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
		require.InDelta(t, float64(DefaultInitialNoiseFloorDB), sl.noiseFloor, 0.01)
		require.Equal(t, DefaultHangoverDuration, sl.hangoverDuration)
		require.InDelta(t, float64(DefaultEnterVoiceOffsetDB), sl.enterVoiceOffsetDB, 0.01)
		require.InDelta(t, float64(DefaultExitVoiceOffsetDB), sl.exitVoiceOffsetDB, 0.01)
	})

	t.Run("with valid options", func(t *testing.T) {
		out, err := NewSignalLogger(log, "incoming", next, WithNoiseFloor(-60), WithHangoverDuration(2*time.Second), WithEnterVoiceOffsetDB(9), WithExitVoiceOffsetDB(4))
		sl, ok := out.(*SignalLogger)
		require.True(t, ok)
		require.NoError(t, err)
		require.NotNil(t, sl)
		require.Equal(t, -60.0, sl.noiseFloor)
		require.Equal(t, 2*time.Second, sl.hangoverDuration)
		require.Equal(t, 9.0, sl.enterVoiceOffsetDB)
		require.Equal(t, 4.0, sl.exitVoiceOffsetDB)
	})

	t.Run("with invalid options", func(t *testing.T) {
		_, err := NewSignalLogger(log, "incoming", next, WithHangoverDuration(-time.Second))
		require.Error(t, err)
		require.Contains(t, err.Error(), "hangover duration must be positive, got -1s")
		_, err = NewSignalLogger(log, "incoming", next, WithEnterVoiceOffsetDB(-1))
		require.Error(t, err)
		require.Contains(t, err.Error(), "enterVoiceOffsetDB must be positive, got -1")
		_, err = NewSignalLogger(log, "incoming", next, WithExitVoiceOffsetDB(-1))
		require.Error(t, err)
		require.Contains(t, err.Error(), "exitVoiceOffsetDB must be positive, got -1")
		_, err = NewSignalLogger(log, "incoming", next, WithNoiseFloor(-101))
		require.Error(t, err)
		require.Contains(t, err.Error(), "noise floor must be >= -100 dBFS, got -101")
	})
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
		t.Logf("%d written sample %d/%d, noise floor %.1f dBFS, state changes %d, last signal %t (%dms ago)\n", sl.framesProcessed, randIndex, len(array), sl.noiseFloor, sl.stateChanges, sl.lastIsSignal, since)
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

// silenceAmplitude: frames with small amplitude yield low dBFS (e.g. ~-56 dBFS for 100), below exit threshold.
const silenceAmplitude = 50

// signalAmplitude: frames with larger amplitude yield high dBFS (e.g. ~-16 dBFS for 5000), above enter threshold (noiseFloor+10).
const signalAmplitude = 5000

func TestSignalLogger_WriteSample(t *testing.T) {
	silenceFrames := make([]msdk.PCM16Sample, 100)
	for i := range silenceFrames {
		amplitude := int16(rand.Uint32() % uint32(silenceAmplitude))
		if rand.Uint()%2 == 0 {
			amplitude = -amplitude
		}
		silenceFrames[i] = createFrame(480, amplitude)
	}

	signalFrames := make([]msdk.PCM16Sample, 100)
	for i := range signalFrames {
		// Random amplitude around signalAmplitude (signalAmplitude/2 to signalAmplitude*3/2) so frames stay above enter threshold.
		amplitude := int16(rand.Uint32()%uint32(signalAmplitude) + signalAmplitude/2)
		if rand.Uint()%2 == 0 {
			amplitude = -amplitude
		}
		signalFrames[i] = createFrame(480, amplitude)
	}

	t.Run("not_printing_on_first_10_frames", func(t *testing.T) {
		sl, _ := newTestLogger(t)

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
				// Use fixed low-amplitude silence so we stay below exit threshold for hangover
				lowSilence := make([]msdk.PCM16Sample, 60)
				for i := range lowSilence {
					lowSilence[i] = createFrame(480, 20)
				}
				sl := testTransition(t, signalFrames, lowSilence, 10, 60, true)
				require.GreaterOrEqual(t, sl.stateChanges, uint64(1), "expected at least one transition to silence")
			})
		})
	})

	t.Run("silence_to_silence_transitions", func(t *testing.T) {
		sl := testTransition(t, silenceFrames, silenceFrames, 10, 0, false)
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
		// Fixed low-amplitude silence and 60 frames (60*DefFrameDur > 1s hangover) for reliable transition.
		lowSilence := make([]msdk.PCM16Sample, 60)
		for i := range lowSilence {
			lowSilence[i] = createFrame(480, 20)
		}
		for i := 0; i < 100; i++ {
			t.Run(fmt.Sprintf("signal_to_silence_transition_%d", i), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					sl := testTransition(t, signalFrames, lowSilence, 10, 60, true)
					require.GreaterOrEqual(t, sl.stateChanges, uint64(1), "expected at least one transition to silence")
					require.Equal(t, false, sl.lastIsSignal)
				})
			})
		}
	})
}
