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
	"fmt"
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
)

// AgentAudioFunc is called once per room, for the first agent audio frame
// that goes to the SIP side. See EventAgentAudioSent.
type AgentAudioFunc func(t time.Time, participantID, trackID string)

// agentAudioReporter is shared by the agent audio detectors of one room, so
// the room reports only its first agent audio frame, whichever track it is on.
type agentAudioReporter struct {
	report   AgentAudioFunc
	reported atomic.Bool
}

func newAgentAudioReporter(report AgentAudioFunc) *agentAudioReporter {
	return &agentAudioReporter{report: report}
}

// agentAudioDetector sits on an agent track, before the mixer. It passes every
// frame on, and reports the first frame above voiceThresholdDBFS that the room
// sends on to the SIP side. Once any detector of the room has reported, every
// detector only passes frames on.
type agentAudioDetector struct {
	next          msdk.PCM16Writer
	output        *msdk.WriteCloserSwitch[msdk.PCM16Sample] // the room output to the SIP side
	rep           *agentAudioReporter
	participantID string
	trackID       string
}

func newAgentAudioDetector(next msdk.PCM16Writer, output *msdk.WriteCloserSwitch[msdk.PCM16Sample], rep *agentAudioReporter, participantID, trackID string) *agentAudioDetector {
	return &agentAudioDetector{next: next, output: output, rep: rep, participantID: participantID, trackID: trackID}
}

func (d *agentAudioDetector) String() string {
	return fmt.Sprintf("AgentAudioDetector(%s) -> %s", d.trackID, d.next.String())
}

func (d *agentAudioDetector) SampleRate() int {
	return d.next.SampleRate()
}

func (d *agentAudioDetector) Close() error {
	return d.next.Close()
}

func (d *agentAudioDetector) WriteSample(sample msdk.PCM16Sample) error {
	d.check(sample)
	return d.next.WriteSample(sample)
}

func (d *agentAudioDetector) check(sample msdk.PCM16Sample) {
	if d.rep.reported.Load() {
		return
	}
	// Before the room output is connected (inbound: before the 200 OK), the
	// mixer drops the audio, so the caller hears nothing of it.
	if d.output.Get() == nil {
		return
	}
	if rmsToDBFS(sample) <= voiceThresholdDBFS {
		return
	}
	if d.rep.reported.CompareAndSwap(false, true) {
		d.rep.report(time.Now(), d.participantID, d.trackID)
	}
}
