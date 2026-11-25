// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package sip

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"
)

type audioSink struct {
	frames int
	err    error
}

func (s *audioSink) String() string  { return "audioSink" }
func (s *audioSink) SampleRate() int { return RoomSampleRate }
func (s *audioSink) Close() error    { return nil }
func (s *audioSink) WriteSample(msdk.PCM16Sample) error {
	if s.err != nil {
		return s.err
	}
	s.frames++
	return nil
}

func audioFrame(level int16) msdk.PCM16Sample {
	f := make(msdk.PCM16Sample, RoomSampleRate/50)
	for i := range f {
		f[i] = level
	}
	return f
}

const (
	loudLevel  = 10000 // about -10 dBFS
	quietLevel = 100   // about -50 dBFS, below voiceThresholdDBFS
)

func TestAudioDetector(t *testing.T) {
	require.Greater(t, rmsToDBFS(audioFrame(loudLevel)), float64(voiceThresholdDBFS))
	require.Less(t, rmsToDBFS(audioFrame(quietLevel)), float64(voiceThresholdDBFS))

	var reports []time.Time
	next := &audioSink{}
	d := newAudioDetector(next, "test", new(atomic.Bool), nil, func(t time.Time) { reports = append(reports, t) })

	require.NoError(t, d.WriteSample(audioFrame(quietLevel)))
	require.Empty(t, reports, "no report for a frame below the threshold")

	next.err = errors.New("write failed")
	require.Error(t, d.WriteSample(audioFrame(loudLevel)))
	require.Empty(t, reports, "no report for a frame the next writer rejected")
	next.err = nil

	before := time.Now()
	require.NoError(t, d.WriteSample(audioFrame(loudLevel)))
	require.Len(t, reports, 1)
	require.False(t, reports[0].Before(before))

	require.NoError(t, d.WriteSample(audioFrame(loudLevel)))
	require.Len(t, reports, 1, "reported once")

	require.Equal(t, 3, next.frames, "every frame is passed on")
}

// TestAudioDetectorFlags: each key reports once. A track that is published
// again gets the same key, so it does not report again. A detector counts a
// frame only when ready returns true.
func TestAudioDetectorFlags(t *testing.T) {
	var reports []string
	var flags audioFlags
	var ready atomic.Bool
	newDetector := func(key, trackID string) *audioDetector {
		return newAudioDetector(&audioSink{}, trackID, flags.get(key), ready.Load, func(time.Time) { reports = append(reports, trackID) })
	}
	voice := newDetector("agent/roomio_audio", "TR_voice")
	ambience := newDetector("agent/background_audio", "TR_ambience")

	require.NoError(t, voice.WriteSample(audioFrame(loudLevel)))
	require.Empty(t, reports, "no report while not ready")

	ready.Store(true)
	require.NoError(t, ambience.WriteSample(audioFrame(loudLevel)))
	require.NoError(t, voice.WriteSample(audioFrame(loudLevel)))
	require.Equal(t, []string{"TR_ambience", "TR_voice"}, reports, "one report per track")

	republished := newDetector("agent/roomio_audio", "TR_voice2")
	require.NoError(t, republished.WriteSample(audioFrame(loudLevel)))
	require.Equal(t, []string{"TR_ambience", "TR_voice"}, reports, "no report for a republished track")
}
