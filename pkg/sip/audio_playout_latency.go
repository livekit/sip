// Copyright 2026 LiveKit, Inc.
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

package sip

import (
	"bytes"
	"sync"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
)

const (
	playoutTrackedPackets = 128
	playoutTrackedOrigins = 64
)

// A zero origin with unknown=false denotes generated silence. unknown=true
// means received audio lost its metadata; it must not acquire another origin.
type playoutOrigin struct {
	at      time.Time
	unknown bool
}

type playoutOriginSpan struct {
	playoutOrigin
	units int64
}

// playoutLatency preserves provenance across both asynchronous playout and
// resampler buffering. It wraps the actual room-facing PCM writer, so the
// measurement includes that write and resampler output flushed during Close.
type playoutLatency struct {
	msdk.PCM16Writer
	stats *LatencyStats

	mu      sync.Mutex
	packets map[*byte]time.Time
	keys    [playoutTrackedPackets]*byte
	nextKey int
	frames  [playoutTrackedOrigins]playoutOriginSpan
	head    int
	count   int
	unknown int64 // Evicted prefix, in the same units as frames.
}

func newPlayoutLatency(out msdk.PCM16Writer, stats *LatencyStats) *playoutLatency {
	return &playoutLatency{PCM16Writer: out, stats: stats, packets: make(map[*byte]time.Time)}
}

// The payload is owned before registration and retained by the jitter buffer.
// Pointer keys keep its backing storage alive and cannot collide with another
// live packet, including duplicates or restarted RTP sequence/timestamp values.
// Both dropped packets and consumed keys are retained for at most 128 arrivals.
func (l *playoutLatency) remember(payload []byte, at time.Time) {
	if len(payload) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.packets, l.keys[l.nextKey])
	key := &payload[0]
	l.keys[l.nextKey] = key
	l.nextKey = (l.nextKey + 1) % len(l.keys)
	l.packets[key] = at
}

func (l *playoutLatency) take(payload []byte) time.Time {
	if len(payload) == 0 {
		return time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := &payload[0]
	at := l.packets[key]
	delete(l.packets, key)
	return at
}

// Units are native input samples multiplied by the output sample rate. Using
// this common denominator avoids rounding when the resampler emits partial
// frames or flushes a short final frame.
func (l *playoutLatency) enqueue(origins []playoutOrigin) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(origins) > 0 {
		origin, n := origins[0], 1
		for n < len(origins) && origins[n] == origin {
			n++
		}
		l.enqueueSpan(origin, n)
		origins = origins[n:]
	}
}

// Caller holds mu. Coalesce adjacent equal origins while retaining sample
// boundaries between received audio, generated silence and unknown provenance.
func (l *playoutLatency) enqueueSpan(origin playoutOrigin, samples int) {
	units := int64(samples) * int64(l.SampleRate())
	if l.count > 0 {
		last := &l.frames[(l.head+l.count-1)%len(l.frames)]
		if last.playoutOrigin == origin {
			last.units += units
			return
		}
	}
	if l.count == len(l.frames) {
		l.unknown += l.frames[l.head].units
		l.head = (l.head + 1) % len(l.frames)
		l.count--
	}
	l.frames[(l.head+l.count)%len(l.frames)] = playoutOriginSpan{
		playoutOrigin: origin, units: units,
	}
	l.count++
}

func (l *playoutLatency) consume(samples int) playoutOrigin {
	l.mu.Lock()
	defer l.mu.Unlock()
	left := int64(samples) * playoutRate
	var origin playoutOrigin
	if l.unknown > 0 && left > 0 {
		n := min(left, l.unknown)
		left -= n
		l.unknown -= n
		origin.unknown = true
	}
	for left > 0 && l.count > 0 {
		frame := &l.frames[l.head]
		if !frame.at.IsZero() && (origin.at.IsZero() || frame.at.Before(origin.at)) {
			origin.at = frame.at
		}
		origin.unknown = origin.unknown || frame.unknown
		n := min(left, frame.units)
		left -= n
		frame.units -= n
		if frame.units == 0 {
			*frame = playoutOriginSpan{}
			l.head = (l.head + 1) % len(l.frames)
			l.count--
		}
	}
	origin.unknown = origin.unknown || left > 0
	return origin
}

func (l *playoutLatency) WriteSample(pcm msdk.PCM16Sample) error {
	origin := l.consume(len(pcm))
	err := l.PCM16Writer.WriteSample(pcm)
	if !origin.unknown && !origin.at.IsZero() {
		l.stats.Record(time.Since(origin.at))
	}
	return err
}

type playoutArrivalHandler struct {
	rtp.HandlerCloser
	latency   *playoutLatency
	audioType byte
}

func (h *playoutArrivalHandler) HandleRTP(header *rtp.Header, payload []byte) error {
	at := time.Now()
	ownedHeader, ownedPayload := header.Clone(), bytes.Clone(payload)
	if header.PayloadType == h.audioType {
		h.latency.remember(ownedPayload, at)
	}
	return h.HandlerCloser.HandleRTP(&ownedHeader, ownedPayload)
}
