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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
)

// RoomAudioTrack identifies a room track in EventRoomAudioSent.
type RoomAudioTrack struct {
	ParticipantID   string
	ParticipantKind string
	TrackID         string
	TrackName       string
}

// RoomAudioFunc is called once per room track, for its first audio frame that
// goes to the SIP side. See EventRoomAudioSent.
type RoomAudioFunc func(t time.Time, track RoomAudioTrack)

// audioDetector passes every frame on, and reports the first frame above
// voiceThresholdDBFS that the next writer accepts. Detectors that share done
// report only once in total, for example a track that its participant
// publishes again.
type audioDetector struct {
	next   msdk.PCM16Writer
	name   string
	done   *atomic.Bool
	ready  func() bool // a frame counts only if ready returns true; nil means always
	report func(t time.Time)
}

func newAudioDetector(next msdk.PCM16Writer, name string, done *atomic.Bool, ready func() bool, report func(t time.Time)) *audioDetector {
	return &audioDetector{next: next, name: name, done: done, ready: ready, report: report}
}

func (d *audioDetector) String() string {
	return fmt.Sprintf("%s -> %s", d.name, d.next.String())
}

func (d *audioDetector) SampleRate() int {
	return d.next.SampleRate()
}

func (d *audioDetector) Close() error {
	return d.next.Close()
}

func (d *audioDetector) WriteSample(sample msdk.PCM16Sample) error {
	voice := !d.done.Load() && (d.ready == nil || d.ready()) && rmsToDBFS(sample) > voiceThresholdDBFS
	t := time.Now()
	if err := d.next.WriteSample(sample); err != nil {
		return err
	}
	// Report only a frame that the next writer accepted.
	if voice && d.done.CompareAndSwap(false, true) {
		d.report(t)
	}
	return nil
}

// audioFlags holds one done flag per key, for audioDetector.
type audioFlags struct {
	mu    sync.Mutex
	flags map[string]*atomic.Bool
}

func (f *audioFlags) get(key string) *atomic.Bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flags == nil {
		f.flags = make(map[string]*atomic.Bool)
	}
	done := f.flags[key]
	if done == nil {
		done = new(atomic.Bool)
		f.flags[key] = done
	}
	return done
}
