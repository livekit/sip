// Copyright 2026 LiveKit, Inc.
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
	"errors"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"
)

// agentAudioSink counts the frames written to it. If err is set, it rejects
// every frame with err.
type agentAudioSink struct {
	frames int
	err    error
}

func (s *agentAudioSink) String() string  { return "agentAudioSink" }
func (s *agentAudioSink) SampleRate() int { return RoomSampleRate }
func (s *agentAudioSink) Close() error    { return nil }
func (s *agentAudioSink) WriteSample(msdk.PCM16Sample) error {
	if s.err != nil {
		return s.err
	}
	s.frames++
	return nil
}

// agentAudioFrame returns a 20 ms frame with every sample at level.
func agentAudioFrame(level int16) msdk.PCM16Sample {
	f := make(msdk.PCM16Sample, RoomSampleRate/50)
	for i := range f {
		f[i] = level
	}
	return f
}

type agentAudioReport struct {
	t                      time.Time
	participantID, trackID string
}

func TestAgentAudioDetector(t *testing.T) {
	const (
		loud  = 10000 // about -10 dBFS
		quiet = 100   // about -50 dBFS, below voiceThresholdDBFS
	)
	require.Greater(t, rmsToDBFS(agentAudioFrame(loud)), float64(voiceThresholdDBFS))
	require.Less(t, rmsToDBFS(agentAudioFrame(quiet)), float64(voiceThresholdDBFS))

	var reports []agentAudioReport
	rep := newAgentAudioReporter(func(t time.Time, participantID, trackID string) {
		reports = append(reports, agentAudioReport{t, participantID, trackID})
	})
	output := msdk.NewWriteCloserSwitch[msdk.PCM16Sample](RoomSampleRate)
	mixIn := &agentAudioSink{}
	d := newAgentAudioDetector(mixIn, output, rep, "PA_agent", "TR_voice")

	// The room output to SIP is not connected yet: the caller can't hear it.
	require.NoError(t, d.WriteSample(agentAudioFrame(loud)))
	require.Empty(t, reports, "no report before the room output is connected")

	output.Swap(&agentAudioSink{})

	require.NoError(t, d.WriteSample(agentAudioFrame(quiet)))
	require.Empty(t, reports, "no report for a frame below the threshold")

	mixIn.err = errors.New("write failed")
	require.Error(t, d.WriteSample(agentAudioFrame(loud)))
	require.Empty(t, reports, "no report for a frame the mixer rejected")
	mixIn.err = nil

	before := time.Now()
	require.NoError(t, d.WriteSample(agentAudioFrame(loud)))
	require.Len(t, reports, 1)
	require.Equal(t, "PA_agent", reports[0].participantID)
	require.Equal(t, "TR_voice", reports[0].trackID)
	require.False(t, reports[0].t.Before(before))

	require.NoError(t, d.WriteSample(agentAudioFrame(loud)))
	require.Len(t, reports, 1, "reported once per detector")

	// Another agent track in the same room: the room has already reported.
	d2 := newAgentAudioDetector(&agentAudioSink{}, output, rep, "PA_agent2", "TR_other")
	require.NoError(t, d2.WriteSample(agentAudioFrame(loud)))
	require.Len(t, reports, 1, "reported once per room")

	require.Equal(t, 4, mixIn.frames, "every frame is passed on")
}
