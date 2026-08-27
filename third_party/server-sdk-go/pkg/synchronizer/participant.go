// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package synchronizer

import (
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// internal struct for managing sender reports
type participantSynchronizer struct {
	sync.Mutex

	tracks        map[uint32]*TrackSynchronizer
	maxRemovedPTS time.Duration // high-water mark from tracks that have been removed
}

func newParticipantSynchronizer() *participantSynchronizer {
	return &participantSynchronizer{
		tracks: make(map[uint32]*TrackSynchronizer),
	}
}

func (p *participantSynchronizer) onSenderReport(pkt *rtcp.SenderReport) {
	p.Lock()
	t := p.tracks[pkt.SSRC]
	p.Unlock()

	if t != nil {
		t.onSenderReport(pkt)
	}
}

func (p *participantSynchronizer) removeTrack(ssrc uint32) {
	p.Lock()
	defer p.Unlock()

	if t := p.tracks[ssrc]; t != nil {
		if pts := t.LastPTSAdjusted(); pts > p.maxRemovedPTS {
			p.maxRemovedPTS = pts
		}
		t.Close()
	}
	delete(p.tracks, ssrc)
}

func (p *participantSynchronizer) getMaxPTSAdjusted() time.Duration {
	p.Lock()
	defer p.Unlock()

	maxPTS := p.maxRemovedPTS
	for _, t := range p.tracks {
		if pts := t.LastPTSAdjusted(); pts > maxPTS {
			maxPTS = pts
		}
	}
	return maxPTS
}

func (p *participantSynchronizer) drain(maxPTS time.Duration) {
	p.Lock()
	defer p.Unlock()

	for _, t := range p.tracks {
		t.Lock()
		t.maxPTS = maxPTS
		t.Unlock()
	}
}
