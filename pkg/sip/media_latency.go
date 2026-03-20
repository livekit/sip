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
	"github.com/livekit/media-sdk/rtp"
)

// LatencyStats is a lock-free accumulator for per-frame latency measurements.
// It tracks count, sum (for average), and max.
type LatencyStats struct {
	Count atomic.Uint64
	SumNs atomic.Uint64
	MaxNs atomic.Uint64
}

func (s *LatencyStats) Record(d time.Duration) {
	ns := uint64(d.Nanoseconds())
	s.Count.Add(1)
	s.SumNs.Add(ns)
	for {
		old := s.MaxNs.Load()
		if ns <= old || s.MaxNs.CompareAndSwap(old, ns) {
			break
		}
	}
}

type LatencyStatsSnapshot struct {
	Count uint64 `json:"count"`
	AvgNs uint64 `json:"avg_ns"`
	MaxNs uint64 `json:"max_ns"`
}

func (s *LatencyStats) Load() LatencyStatsSnapshot {
	count := s.Count.Load()
	var avg uint64
	if count > 0 {
		avg = s.SumNs.Load() / count
	}
	return LatencyStatsSnapshot{
		Count: count,
		AvgNs: avg,
		MaxNs: s.MaxNs.Load(),
	}
}

// latencyRTPEntry wraps an RTP handler chain, recording the entry timestamp
// on a shared atomic for the corresponding exit wrapper to compute the delta.
func newLatencyRTPEntry(h rtp.HandlerCloser, entryTime *atomic.Int64) rtp.HandlerCloser {
	return &latencyRTPEntry{h: h, entryTime: entryTime}
}

type latencyRTPEntry struct {
	h         rtp.HandlerCloser
	entryTime *atomic.Int64
}

func (e *latencyRTPEntry) String() string {
	return fmt.Sprintf("LatencyEntry -> %s", e.h.String())
}

func (e *latencyRTPEntry) HandleRTP(h *rtp.Header, payload []byte) error {
	e.entryTime.Store(time.Now().UnixNano())
	return e.h.HandleRTP(h, payload)
}

func (e *latencyRTPEntry) Close() {
	e.h.Close()
}

// latencyRTPExit wraps an RTP write stream, reading the shared entry timestamp
// to compute and record the processing latency of the encode pipeline.
func newLatencyRTPExit(w rtp.WriteStream, entryTime *atomic.Int64, stats *LatencyStats) rtp.WriteStream {
	if stats == nil {
		return w
	}
	return &latencyRTPExit{w: w, entryTime: entryTime, stats: stats}
}

type latencyRTPExit struct {
	w         rtp.WriteStream
	entryTime *atomic.Int64
	stats     *LatencyStats
}

func (e *latencyRTPExit) String() string {
	return fmt.Sprintf("LatencyExit -> %s", e.w.String())
}

func (e *latencyRTPExit) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	// for exit, we first write the RTP packet and then record the latency.
	n, err := e.w.WriteRTP(h, payload)
	if t := e.entryTime.Load(); t > 0 {
		e.stats.Record(time.Duration(time.Now().UnixNano() - t))
	}
	return n, err
}

// latencyPCMEntry wraps a PCM writer, recording the entry timestamp for the
// outbound encode pipeline (mixer output -> codec encode -> RTP write).
func newLatencyPCMEntry(w msdk.PCM16Writer, entryTime *atomic.Int64) msdk.PCM16Writer {
	return &latencyPCMEntry{w: w, entryTime: entryTime}
}

type latencyPCMEntry struct {
	w         msdk.PCM16Writer
	entryTime *atomic.Int64
}

func (e *latencyPCMEntry) String() string {
	return fmt.Sprintf("LatencyEntry -> %s", e.w)
}

func (e *latencyPCMEntry) SampleRate() int {
	return e.w.SampleRate()
}

func (e *latencyPCMEntry) Close() error {
	return e.w.Close()
}

func (e *latencyPCMEntry) WriteSample(sample msdk.PCM16Sample) error {
	e.entryTime.Store(time.Now().UnixNano())
	return e.w.WriteSample(sample)
}

// latencyPCMExit wraps a PCM writer, reading the shared entry timestamp to
// compute and record the processing latency.
func newLatencyPCMExit(w msdk.PCM16Writer, entryTime *atomic.Int64, stats *LatencyStats) msdk.PCM16Writer {
	if stats == nil {
		return w
	}
	return &latencyPCMExit{w: w, entryTime: entryTime, stats: stats}
}

type latencyPCMExit struct {
	w         msdk.PCM16Writer
	entryTime *atomic.Int64
	stats     *LatencyStats
}

func (e *latencyPCMExit) String() string {
	return fmt.Sprintf("LatencyExit -> %s", e.w)
}

func (e *latencyPCMExit) SampleRate() int {
	return e.w.SampleRate()
}

func (e *latencyPCMExit) Close() error {
	return e.w.Close()
}

func (e *latencyPCMExit) WriteSample(sample msdk.PCM16Sample) error {
	// for exit, we first write the sample and then record the latency.
	err := e.w.WriteSample(sample)
	if t := e.entryTime.Load(); t > 0 {
		e.stats.Record(time.Duration(time.Now().UnixNano() - t))
	}
	return err
}
