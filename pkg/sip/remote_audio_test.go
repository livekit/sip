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

	"github.com/stretchr/testify/require"
)

func TestRemoteAudioDetector(t *testing.T) {
	var reports []time.Time
	room := &agentAudioSink{}
	d := newRemoteAudioDetector(room, func(t time.Time) { reports = append(reports, t) })

	require.NoError(t, d.WriteSample(agentAudioFrame(100)))
	require.Empty(t, reports, "no report for a frame below the threshold")

	room.err = errors.New("write failed")
	require.Error(t, d.WriteSample(agentAudioFrame(10000)))
	require.Empty(t, reports, "no report for a frame the room writer rejected")
	room.err = nil

	before := time.Now()
	require.NoError(t, d.WriteSample(agentAudioFrame(10000)))
	require.Len(t, reports, 1)
	require.False(t, reports[0].Before(before))

	require.NoError(t, d.WriteSample(agentAudioFrame(10000)))
	require.Len(t, reports, 1, "reported once")

	require.Equal(t, 3, room.frames, "every frame is passed on")
}
