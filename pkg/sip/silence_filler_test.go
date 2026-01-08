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
	"slices"
	"testing"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// Both a rtp.Handler and msdk.PCM16Writer designed to test the silence suppression handler
type SilenceSuppressionTester struct {
	audioSampleRate       int
	framesReceived        []bool // true if received a signal frame, false if it was generated silence
	receivedSilenceFrames uint64
	receivedSignalFrames  uint64
	gapFiller             rtp.Handler
}

func newSilenceSuppressionTester(audioSampleRate int, log logger.Logger) *SilenceSuppressionTester {
	tester := &SilenceSuppressionTester{
		audioSampleRate:       audioSampleRate,
		framesReceived:        make([]bool, 0),
		receivedSilenceFrames: 0,
		receivedSignalFrames:  0,
	}
	tester.gapFiller = newSilenceFiller(tester, tester, audioSampleRate, log)
	return tester
}

func (s *SilenceSuppressionTester) String() string {
	return "SilenceSuppressionTester"
}

func (s *SilenceSuppressionTester) SampleRate() int {
	return s.audioSampleRate
}

func (s *SilenceSuppressionTester) Close() error {
	return nil
}

func (s *SilenceSuppressionTester) WriteSample(sample msdk.PCM16Sample) error {
	s.framesReceived = append(s.framesReceived, false)
	s.receivedSilenceFrames++
	return nil
}

func (s *SilenceSuppressionTester) HandleRTP(h *rtp.Header, payload []byte) error {
	s.framesReceived = append(s.framesReceived, true)
	s.receivedSignalFrames++
	return nil
}

func (s *SilenceSuppressionTester) SendSignalFrames(count int, nextSeq uint16, nextTimestamp uint32) (uint16, uint32, error) {
	samplesPerFrame := s.audioSampleRate / rtp.DefFramesPerSec
	for i := 0; i < count; i++ {
		h := &rtp.Header{
			SequenceNumber: nextSeq,
			Timestamp:      nextTimestamp,
		}
		nextSeq++
		nextTimestamp += uint32(samplesPerFrame)
		err := s.gapFiller.HandleRTP(h, []byte{0x01, 0x02, 0x03})
		if err != nil {
			return nextSeq, nextTimestamp, err
		}
	}
	return nextSeq, nextTimestamp, nil
}

func (s *SilenceSuppressionTester) assertSilenceIndexes(t *testing.T, expectedSize int, silenceIndexes []int) {
	require.Equal(t, expectedSize, len(s.framesReceived))
	require.Equal(t, uint64(len(silenceIndexes)), s.receivedSilenceFrames)
	require.Equal(t, uint64(expectedSize-len(silenceIndexes)), s.receivedSignalFrames)
	for i, isSignal := range s.framesReceived {
		if slices.Contains(silenceIndexes, i) {
			require.False(t, isSignal, "frame %d should be silence", i)
		} else {
			require.True(t, isSignal, "frame %d should be signal", i)
		}
	}
	for _, index := range silenceIndexes { // Make sure we're not missing any indexes
		if index < 0 || index >= expectedSize {
			t.Fatalf("index %d out of range", index)
		}
	}
}

func TestSilenceSuppressionHandling(t *testing.T) {
	const (
		sampleRate      = 8000
		samplesPerFrame = uint32(sampleRate / rtp.DefFramesPerSec) // 160 samples per 20ms frame
	)

	log := logger.GetLogger()

	t.Run("no gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		_, _, err := tester.SendSignalFrames(10, 100, 1000)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 10, []int{})
	})

	t.Run("single frame gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		nextSeq := uint16(100)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 1
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 11, []int{5})
	})

	t.Run("handful of frames gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		nextSeq := uint16(100)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 3
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 13, []int{5, 6, 7})
	})

	t.Run("large gap that's not filled", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		nextSeq := uint16(100)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 50 // Too large, shouldn't be filled
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 10, []int{})
	})

	t.Run("timestamp wrap-around no gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// Start near wrap-around
		nextSeq := uint16(100)
		nextTimestamp := uint32(0xFFFFFF00) // Near wrap-around
		_, _, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 5, []int{})
	})

	t.Run("timestamp wrap-around with gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// 2 signal + 3 silence (across wrap-around) + 2 signal = 7 total
		nextSeq := uint16(100)
		nextTimestamp := uint32(0xFFFFFF00)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 3
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 7, []int{2, 3, 4})
	})

	t.Run("sequence wrap-around no gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// Start near sequence wrap-around
		nextSeq := uint16(0xFFFE)
		nextTimestamp := uint32(10000)
		_, _, err := tester.SendSignalFrames(4, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 4, []int{})
	})

	t.Run("sequence wrap-around with gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// Start near sequence wrap-around
		nextSeq := uint16(0xFFFE)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 3
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 7, []int{2, 3, 4})
	})
}
