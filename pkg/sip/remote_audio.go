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

// remoteAudioDetector sits on the audio from the SIP side to the room. It
// passes every frame on, and reports the first frame above voiceThresholdDBFS
// once.
type remoteAudioDetector struct {
	next   msdk.PCM16Writer
	report func(t time.Time)
	// done is atomic: the RTP loop and the jitter buffer timer both write.
	done atomic.Bool
}

func newRemoteAudioDetector(next msdk.PCM16Writer, report func(t time.Time)) *remoteAudioDetector {
	return &remoteAudioDetector{next: next, report: report}
}

func (d *remoteAudioDetector) String() string {
	return fmt.Sprintf("RemoteAudioDetector -> %s", d.next.String())
}

func (d *remoteAudioDetector) SampleRate() int {
	return d.next.SampleRate()
}

func (d *remoteAudioDetector) Close() error {
	return d.next.Close()
}

func (d *remoteAudioDetector) WriteSample(sample msdk.PCM16Sample) error {
	if !d.done.Load() && rmsToDBFS(sample) > voiceThresholdDBFS && d.done.CompareAndSwap(false, true) {
		d.report(time.Now())
	}
	return d.next.WriteSample(sample)
}
