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
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

const (
	playoutRate         = 8000
	playoutFrameSamples = playoutRate / rtp.DefFramesPerSec
	playoutDelay        = 60 * time.Millisecond
	playoutCapacity     = playoutRate / 5 // 200 ms, including holes in the media timeline.
	playoutSourceIdle   = 200 * time.Millisecond
)

// PlayoutStats distinguishes discarded received PCM from silence generated at
// playout time. None of these counters measures lost speech perceptually.
type PlayoutStats struct {
	Enabled          atomic.Bool
	AudioSamples     atomic.Uint64
	SilenceSamples   atomic.Uint64
	LateSamples      atomic.Uint64
	OverflowSamples  atomic.Uint64
	ResetSamples     atomic.Uint64
	DuplicatePackets atomic.Uint64
	ReorderedPackets atomic.Uint64
	SourcePackets    atomic.Uint64
	Resets           atomic.Uint64
	SkippedFrames    atomic.Uint64
	WriteErrors      atomic.Uint64
	MaxQueuedSamples atomic.Uint64
	Residence        LatencyStats
}

type PlayoutStatsSnapshot struct {
	Enabled          bool                 `json:"enabled"`
	AudioSamples     uint64               `json:"audio_samples"`
	SilenceSamples   uint64               `json:"silence_samples"`
	LateSamples      uint64               `json:"late_samples_dropped"`
	OverflowSamples  uint64               `json:"overflow_samples_dropped"`
	ResetSamples     uint64               `json:"reset_samples_dropped"`
	DuplicatePackets uint64               `json:"duplicate_or_old_packets"`
	ReorderedPackets uint64               `json:"reordered_packets"`
	SourcePackets    uint64               `json:"other_source_packets"`
	Resets           uint64               `json:"resets"`
	SkippedFrames    uint64               `json:"skipped_playout_frames"`
	WriteErrors      uint64               `json:"write_errors"`
	MaxQueuedSamples uint64               `json:"max_queued_samples"`
	Residence        LatencyStatsSnapshot `json:"queue_residence"`
}

func (s *PlayoutStats) Load() PlayoutStatsSnapshot {
	return PlayoutStatsSnapshot{
		Enabled:      s.Enabled.Load(),
		AudioSamples: s.AudioSamples.Load(), SilenceSamples: s.SilenceSamples.Load(),
		LateSamples: s.LateSamples.Load(), OverflowSamples: s.OverflowSamples.Load(),
		ResetSamples: s.ResetSamples.Load(), DuplicatePackets: s.DuplicatePackets.Load(),
		ReorderedPackets: s.ReorderedPackets.Load(),
		SourcePackets:    s.SourcePackets.Load(), Resets: s.Resets.Load(),
		SkippedFrames: s.SkippedFrames.Load(), WriteErrors: s.WriteErrors.Load(),
		MaxQueuedSamples: s.MaxQueuedSamples.Load(), Residence: s.Residence.Load(),
	}
}

// g711Playout schedules native 8 kHz PCM by RTP timestamp. G711 has one decoded
// sample per payload byte, so 10/20/40/60 ms packets can share the same timeline
// without assuming a packet duration or attributing buffered decoder output to
// the wrong packet. Other codecs deliberately keep their existing input path.
//
// The receive callback only decodes and copies into a bounded ring. A single
// worker owns the downstream writer and emits 20 ms blocks, including silence
// for unfilled intervals. An elapsed DTX gap is never inserted retrospectively.
type g711Playout struct {
	mu      sync.Mutex
	out     msdk.PCM16Writer
	codec   string
	decode  func([]int16, []byte)
	stats   *PlayoutStats
	log     logger.Logger
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	closed  bool
	err     error
	started bool
	ordered bool // The existing jitter buffer has selected and ordered this stream.

	next         time.Time
	cursor       int64
	lastTS       uint32
	lastSeq      uint16
	lastPos      int64
	lastArrival  time.Time
	aheadSince   time.Time
	aheadCount   int
	behindSince  time.Time
	behindCount  int
	restartSeq   uint16
	restartTS    uint32
	restartAt    time.Time
	restartCount int
	ssrc         uint32
	retired      []uint32 // Bounded recent history prevents old-source switch-back.
	pcm          [playoutCapacity]int16
	arrival      [playoutCapacity]time.Time // Zero denotes an unfilled sample.
}

// newG711Playout does not own out. In particular, replacing an input handler
// must not close the shared MediaPort SwitchWriter used by the replacement.
func newG711Playout(out msdk.PCM16Writer, codecName string, stats *PlayoutStats, log logger.Logger, ordered bool) rtp.HandlerCloser {
	if stats == nil {
		stats = &PlayoutStats{}
	}
	stats.Enabled.Store(true)
	h := &g711Playout{
		out:   msdk.ResampleWriter(msdk.NopCloser(out), playoutRate),
		codec: codecName, stats: stats, log: log,
		ordered: ordered,
		wake:    make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	switch codecName {
	case g711.ULawSDPNameAndRate:
		h.decode = g711.DecodeULawTo
	case g711.ALawSDPNameAndRate:
		h.decode = g711.DecodeALawTo
	default:
		panic("G711 playout requires PCMU/8000 or PCMA/8000")
	}
	go h.run()
	return h
}

func (h *g711Playout) String() string {
	return fmt.Sprintf("Playout(%s,60ms,200ms) -> %s", h.codec, h.out)
}

func (h *g711Playout) Close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.stop)
	}
	h.mu.Unlock()
	<-h.done // No downstream writes may occur after Close returns.
}

// resetLocked discards queued old-timeline audio. A frame already handed to
// the downstream writer can finish; Close joins that write before returning.
func (h *g711Playout) resetLocked(header *rtp.Header, now time.Time) {
	if h.started {
		h.stats.Resets.Add(1)
		for _, at := range h.arrival {
			if !at.IsZero() {
				h.stats.ResetSamples.Add(1)
			}
		}
	}
	clear(h.arrival[:])
	h.started, h.cursor, h.lastPos = true, 0, 0
	h.next = now.Add(playoutDelay)
	h.ssrc, h.lastTS, h.lastSeq = header.SSRC, header.Timestamp, header.SequenceNumber
	h.lastArrival = now
	h.aheadSince, h.aheadCount = time.Time{}, 0
	h.behindSince, h.behindCount = time.Time{}, 0
	h.restartAt, h.restartCount = time.Time{}, 0
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *g711Playout) HandleRTP(header *rtp.Header, payload []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return io.ErrClosedPipe
	}
	if h.err != nil {
		return h.err
	}
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > rtp.MTUSize {
		return fmt.Errorf("G711 payload exceeds RTP MTU: %d", len(payload))
	}
	now := time.Now()
	pos := int64(0)
	progressing := false
	if !h.started {
		h.resetLocked(header, now)
	} else if header.SSRC != h.ssrc && h.ordered {
		// The outer jitter buffer owns source selection. Accept its selected
		// source immediately rather than imposing a second switch probation.
		h.resetLocked(header, now)
	} else if header.SSRC != h.ssrc {
		for _, ssrc := range h.retired {
			if header.SSRC == ssrc {
				h.stats.SourcePackets.Add(1)
				return nil
			}
		}
		// A shared SIP port can receive multiple SSRCs. Do not let interleaved
		// sources continually erase each other's queued audio.
		if now.Sub(h.lastArrival) < playoutSourceIdle {
			h.stats.SourcePackets.Add(1)
			return nil
		}
		if len(h.retired) == 8 {
			h.retired = h.retired[1:]
		}
		h.retired = append(h.retired, h.ssrc)
		h.log.Debugw("audio playout source changed", "ssrc", header.SSRC)
		h.resetLocked(header, now)
	} else {
		// Signed modular differences handle ordinary sequence and timestamp
		// wrap. Packets separated by over half a sequence range are not reordered.
		seqDelta := int16(header.SequenceNumber - h.lastSeq)
		delta := int64(int32(header.Timestamp - h.lastTS))
		pos = h.lastPos + delta
		if seqDelta == 0 {
			h.stats.DuplicatePackets.Add(1)
			return nil
		}
		if seqDelta < 0 && h.ordered {
			// An old sequence accepted by the outer buffer belongs to a sender
			// restart that buffer already confirmed.
			h.resetLocked(header, now)
			pos = 0
		} else if seqDelta < 0 {
			if delta < 0 && pos+int64(len(payload)) > h.cursor && pos < h.cursor+playoutCapacity {
				// The ring is also the G711 jitter buffer: reordered packets can
				// still fill holes before their playout deadline. Do not move
				// the active clock or sequence anchor backwards.
				h.stats.ReorderedPackets.Add(1)
			} else if h.confirmRestartLocked(header, now) {
				h.log.Debugw("audio playout sender restarted", "ssrc", header.SSRC)
				h.resetLocked(header, now)
				pos = 0
			} else {
				h.stats.DuplicatePackets.Add(1)
				return nil
			}
		} else if delta == 0 {
			h.stats.DuplicatePackets.Add(1)
			return nil // A stationary RTP clock is invalid G711 media.
		} else if delta < 0 || (time.Duration(delta)*time.Second/playoutRate-now.Sub(h.lastArrival) > time.Second && pos > h.cursor+playoutCapacity) {
			// A reset is a clock discontinuity, not just an out-of-window
			// packet. In particular, real elapsed DTX is preserved.
			h.log.Debugw("audio playout timestamp reset", "rtp_timestamp", header.Timestamp)
			h.resetLocked(header, now)
			pos = 0
		} else {
			h.lastTS, h.lastSeq, h.lastPos = header.Timestamp, header.SequenceNumber, pos
			h.lastArrival = now
			h.restartAt, h.restartCount = time.Time{}, 0
			progressing = true
		}
	}

	// A modest timestamp jump or sustained clock drift can leave every packet
	// beyond the bounded window. Recover only after the offset persists in wall
	// time; a single receive burst must not erase valid audio already queued.
	if pos >= h.cursor+playoutCapacity {
		if h.aheadSince.IsZero() {
			h.aheadSince = now
		}
		h.aheadCount++
		if h.aheadCount >= 3 && now.Sub(h.aheadSince) >= playoutDelay {
			h.log.Debugw("audio playout persistent clock lead", "rtp_timestamp", header.Timestamp)
			h.resetLocked(header, now)
			pos = 0
		}
	} else {
		h.aheadSince, h.aheadCount = time.Time{}, 0
	}
	// A sender can also fall permanently behind the local clock, for example
	// when it pauses transmission without advancing its RTP timestamp. Rebuffer
	// only a progressive all-late stream sustained over wall time; isolated late
	// or reordered packets must not move the active timeline backwards.
	if progressing && pos+int64(len(payload)) <= h.cursor {
		if h.behindSince.IsZero() {
			h.behindSince = now
		}
		h.behindCount++
		if h.behindCount >= 3 && now.Sub(h.behindSince) >= playoutDelay {
			h.log.Debugw("audio playout persistent clock lag", "rtp_timestamp", header.Timestamp)
			h.resetLocked(header, now)
			pos = 0
		}
	} else if progressing {
		h.behindSince, h.behindCount = time.Time{}, 0
	}

	// Decode synchronously: neither the caller's reusable payload/header nor
	// a codec-owned PCM buffer is retained by the worker.
	var decoded [rtp.MTUSize]int16
	h.decode(decoded[:len(payload)], payload)
	start, end := max(pos, h.cursor), min(pos+int64(len(payload)), h.cursor+playoutCapacity)
	if pos < h.cursor {
		h.stats.LateSamples.Add(uint64(min(int64(len(payload)), h.cursor-pos)))
	}
	if pos+int64(len(payload)) > h.cursor+playoutCapacity {
		h.stats.OverflowSamples.Add(uint64(min(int64(len(payload)), pos+int64(len(payload))-h.cursor-playoutCapacity)))
	}
	for i := start; i < end; i++ {
		index := i % playoutCapacity
		if h.arrival[index].IsZero() { // Do not overwrite previously accepted audio.
			h.pcm[index], h.arrival[index] = decoded[i-pos], now
		}
	}
	if end > start {
		queued := uint64(end - h.cursor)
		if queued > h.stats.MaxQueuedSamples.Load() {
			h.stats.MaxQueuedSamples.Store(queued)
		}
	}
	return nil
}

// confirmRestartLocked distinguishes a sender that restarted both its clock
// and sequence from a delayed old packet. Require an idle active source and a
// progressive replacement stream spanning wall time, not a single burst.
func (h *g711Playout) confirmRestartLocked(header *rtp.Header, now time.Time) bool {
	if now.Sub(h.lastArrival) < playoutSourceIdle {
		return false
	}
	seqDelta := int16(header.SequenceNumber - h.restartSeq)
	tsDelta := int32(header.Timestamp - h.restartTS)
	if h.restartCount == 0 || seqDelta <= 0 || seqDelta > 10 || tsDelta <= 0 || tsDelta > playoutRate {
		h.restartAt, h.restartCount = now, 1
	} else {
		h.restartCount++
	}
	h.restartSeq, h.restartTS = header.SequenceNumber, header.Timestamp
	return h.restartCount >= 3 && now.Sub(h.restartAt) >= playoutDelay
}

func (h *g711Playout) run() {
	const maxErrors = 50 // Match rtpReadLoop's consecutive-error limit at 20 ms/frame.

	defer close(h.done)
	defer h.out.Close() // Flush/free only the owned resampler, not the shared sink.
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var errorCnt int
	for {
		h.mu.Lock()
		started, next, closed := h.started, h.next, h.closed
		h.mu.Unlock()
		if closed {
			return
		}
		if !started {
			select {
			case <-h.stop:
				return
			case <-h.wake:
				continue
			}
		}
		if delay := time.Until(next); delay > 0 {
			timer.Reset(delay)
			select {
			case <-h.stop:
				return
			case <-h.wake:
				continue
			case <-timer.C:
			}
		}
		frame, ok := h.readFrame(time.Now())
		if !ok {
			continue
		}
		if err := h.out.WriteSample(frame); err != nil {
			errorCnt++
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return
			}
			if errorCnt >= maxErrors {
				h.err = err
			}
			h.mu.Unlock()
			h.stats.WriteErrors.Add(1)
			if errorCnt >= maxErrors {
				h.log.Warnw("stopping audio playout due to persisted write errors", err, "errorCount", errorCnt)
				return
			}
			h.log.Debugw("audio playout write failed", "error", err, "errorCount", errorCnt)
			// The failed frame has expired. Try the next frame at its normal
			// deadline instead of retrying stale audio in a tight loop.
			continue
		}
		errorCnt = 0
	}
}

func (h *g711Playout) readFrame(now time.Time) (msdk.PCM16Sample, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || !h.started || now.Before(h.next) {
		return nil, false
	}
	// Missed deadlines expire media; never replay a scheduler stall as a burst.
	if skipped := int64(now.Sub(h.next) / rtp.DefFrameDur); skipped > 0 {
		samples := skipped * int64(playoutFrameSamples)
		for i := int64(0); i < min(samples, playoutCapacity); i++ {
			index := (h.cursor + i) % playoutCapacity
			if !h.arrival[index].IsZero() {
				h.stats.LateSamples.Add(1)
				h.arrival[index] = time.Time{}
			}
		}
		h.cursor += samples
		h.stats.SkippedFrames.Add(uint64(skipped))
		h.next = now // Resume cadence instead of immediately catching up again.
	}
	frame := make(msdk.PCM16Sample, playoutFrameSamples)
	var audio uint64
	var oldest time.Time
	for i := range frame {
		index := (h.cursor + int64(i)) % playoutCapacity
		if at := h.arrival[index]; !at.IsZero() {
			frame[i] = h.pcm[index]
			audio++
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
			h.arrival[index] = time.Time{}
		}
	}
	h.stats.AudioSamples.Add(audio)
	h.stats.SilenceSamples.Add(uint64(playoutFrameSamples) - audio)
	if !oldest.IsZero() {
		h.stats.Residence.Record(now.Sub(oldest))
	}
	h.cursor += int64(playoutFrameSamples)
	h.next = h.next.Add(rtp.DefFrameDur)
	return frame, true
}
