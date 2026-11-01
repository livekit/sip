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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// A blocked downstream callback must not stall RTP receipt or turn overdue
// media into a burst of callbacks when it returns. The callback already in
// flight can finish; this test measures new callback starts independently.
func TestG711PlayoutExpiresBacklogAfterDownstreamStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutStallSink{entered: make(chan struct{}), release: make(chan struct{})}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		defer release()

		seq, timestamp := uint16(100), uint32(10000)
		send := func(amplitude int16) int16 {
			pcm := make(msdk.PCM16Sample, 160)
			for i := range pcm {
				pcm[i] = amplitude
			}
			var payload g711.ULawSample
			payload.Encode(pcm)
			var decoded [160]int16
			g711.DecodeULawTo(decoded[:], payload)
			require.NoError(t, handler.HandleRTP(&rtp.Header{
				SSRC: 23, SequenceNumber: seq, Timestamp: timestamp,
			}, payload))
			seq++
			timestamp += 160
			return decoded[0]
		}
		var expectedCurrent int16
		for i := range 12 {
			quantized := send(int16((i + 1) * 1000))
			if i == 9 {
				expectedCurrent = quantized // Source frame due at 240 ms.
			}
			time.Sleep(20 * time.Millisecond)
			synctest.Wait()
		}
		select {
		case <-out.entered:
		default:
			t.Fatal("first downstream callback never entered")
		}
		require.Len(t, out.snapshot(), 1, "RTP receipt must continue while the only output worker is blocked")

		resumedAt := time.Now() // The first callback has been blocked for 180 ms.
		release()
		synctest.Wait()
		frames := out.snapshot()
		require.Len(t, frames, 2, "release may start one current frame, never replay the queued backlog")
		require.Equal(t, resumedAt, frames[1].at)
		require.Equal(t, expectedCurrent, frames[1].value, "overdue speech must expire instead of replaying after the stall")
		require.Equal(t, uint64(8), stats.SkippedFrames.Load())
		require.Equal(t, uint64(8*160), stats.LateSamples.Load())

		var resumedMarker int16
		for range 20 {
			resumedMarker = send(-12000)
			time.Sleep(20 * time.Millisecond)
			synctest.Wait()
		}
		frames = out.snapshot()
		markerSeen := false
		for i, frame := range frames {
			require.Equal(t, 160, frame.samples)
			if i >= 2 {
				require.Equal(t, 20*time.Millisecond, frame.at.Sub(frames[i-1].at), "no catch-up callbacks after the current frame")
			}
			markerSeen = markerSeen || frame.value == resumedMarker
		}
		require.True(t, markerSeen, "fresh RTP speech must continue after the stalled output recovers")
		require.Zero(t, stats.Resets.Load(), "a single downstream stall must not reset the sender's clock")
		require.LessOrEqual(t, stats.MaxQueuedSamples.Load(), uint64(playoutCapacity))
	})
}

type playoutStallFrame struct {
	at      time.Time
	value   int16
	samples int
}

type playoutStallSink struct {
	mu      sync.Mutex
	frames  []playoutStallFrame
	entered chan struct{}
	release chan struct{}
}

func (s *playoutStallSink) String() string  { return "stall test sink" }
func (s *playoutStallSink) SampleRate() int { return 8000 }
func (s *playoutStallSink) Close() error    { return nil }
func (s *playoutStallSink) WriteSample(pcm msdk.PCM16Sample) error {
	s.mu.Lock()
	first := len(s.frames) == 0
	s.frames = append(s.frames, playoutStallFrame{at: time.Now(), value: pcm[0], samples: len(pcm)})
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
	}
	return nil
}
func (s *playoutStallSink) snapshot() []playoutStallFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]playoutStallFrame(nil), s.frames...)
}
