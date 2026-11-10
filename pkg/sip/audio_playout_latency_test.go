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
	"errors"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/g711"
	mediaRTP "github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestG711PlayoutLatencyPreservesBufferedPCMOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPipelineHarness(t, 48000)
		out := &latencyDelayedSink{playoutCodecSink: playoutCodecSink{rate: 48000}, delay: 5 * time.Millisecond}
		h.audioIn.Swap(out)
		configurePlayoutPipeline(t, h, g711.ULawSDPNameOnly, false)
		h.injectRTP(&rtp.Packet{Header: rtp.Header{
			Version: 2, SSRC: 7, PayloadType: h.audio.Type, SequenceNumber: 1, Timestamp: 160,
		}, Payload: make([]byte, 160)})
		synctest.Wait()
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, out.snapshot(), "the real resampler buffers its first frame")
		require.Zero(t, h.conf.stats.LatencyInE2E.Load().Count)
		require.NoError(t, h.pipeline.Close())
		latency := h.conf.stats.LatencyInE2E.Load()
		require.Equal(t, uint64(1), latency.Count, "closing must measure buffered audio at the actual PCM exit")
		require.Equal(t, uint64(65*time.Millisecond), latency.AvgNs,
			"latency includes playout, resampler buffering and the completed room write")
		require.Equal(t, uint64(60*time.Millisecond), h.conf.stats.InputPlayout.Residence.Load().AvgNs,
			"queue residence remains a separate interval ending before output")
		require.Zero(t, out.closeCount, "the pipeline does not own the room sink")
	})
}

func TestG711PlayoutLatencyDoesNotAttributeSplitSilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		var stats LatencyStats
		latency := newPlayoutLatency(out, &stats)
		handler := newG711PlayoutWithLatency(&latencySplitSink{PCM16Writer: latency},
			g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false, latency)
		defer handler.Close()
		entry := &playoutArrivalHandler{HandlerCloser: handler, latency: latency}
		require.NoError(t, entry.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1, Timestamp: 80}, make([]byte, 80)))
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		require.Len(t, out.snapshot(), 2, "one 20 ms playout frame is written in two 10 ms chunks")
		require.Equal(t, uint64(1), stats.Load().Count, "the generated-silence half has no RTP arrival to measure")
		require.Equal(t, uint64(60*time.Millisecond), stats.Load().AvgNs)
	})
}

func TestPlayoutLatencyPacketOriginsAreBoundedAndUnique(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stats LatencyStats
		latency := newPlayoutLatency(&playoutCodecSink{rate: 8000}, &stats)
		var retained [][]byte
		entry := &playoutArrivalHandler{
			HandlerCloser: mediaRTP.NewNopCloser(mediaRTP.HandlerFunc(func(_ *rtp.Header, payload []byte) error {
				retained = append(retained, payload)
				return nil
			})),
			latency: latency,
		}
		payload := []byte{1, 2, 3}
		start := time.Now()
		for range playoutTrackedPackets + 1 {
			// Reusing all RTP counters and the caller's buffer must not merge origins.
			require.NoError(t, entry.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1, Timestamp: 1}, payload))
			time.Sleep(time.Millisecond)
		}
		clear(payload)
		require.LessOrEqual(t, len(latency.packets), playoutTrackedPackets)
		require.True(t, latency.take(retained[0]).IsZero(), "bounded eviction must not return a newer packet's arrival")
		require.Equal(t, start.Add(time.Millisecond), latency.take(retained[1]))
		require.Equal(t, start.Add(playoutTrackedPackets*time.Millisecond), latency.take(retained[len(retained)-1]))
		require.True(t, latency.take(retained[1]).IsZero(), "an origin is consumed only once")
		for _, packet := range retained {
			require.Equal(t, []byte{1, 2, 3}, packet, "retained packets own their payload independently")
		}
	})
}

func TestPlayoutLatencyTracksPartialOutputAndWriteErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("temporary output failure")
		out := &latencyDelayedSink{playoutCodecSink: playoutCodecSink{rate: 44100, writeErr: failure}, delay: 5 * time.Millisecond}
		var stats LatencyStats
		latency := newPlayoutLatency(out, &stats)
		origins := make([]playoutOrigin, 160)
		for i := range 40 {
			origins[i].at = time.Now()
		}
		time.Sleep(10 * time.Millisecond)
		for i := 80; i < len(origins); i++ {
			origins[i].at = time.Now()
		}
		latency.enqueue(origins) // 5 ms received, 5 ms generated silence, 10 ms received.
		require.ErrorIs(t, latency.WriteSample(make(msdk.PCM16Sample, 220)), failure)
		out.writeErr = nil
		require.NoError(t, latency.WriteSample(make(msdk.PCM16Sample, 1))) // Half a sample from each of the first two spans.
		require.NoError(t, latency.WriteSample(make(msdk.PCM16Sample, 220)))
		require.Equal(t, uint64(2), stats.Count.Load(), "pure generated silence does not add an observation")
		require.NoError(t, latency.WriteSample(make(msdk.PCM16Sample, 441)))
		require.Equal(t, uint64(3), stats.Count.Load(), "failed writes consume their duration and retain existing metric semantics")
		require.Equal(t, uint64(55*time.Millisecond), stats.SumNs.Load())
		require.Equal(t, uint64(20*time.Millisecond), stats.MaxNs.Load())
	})
}

func TestPlayoutLatencyOverflowDoesNotShiftLaterOrigins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 48000}
		var stats LatencyStats
		latency := newPlayoutLatency(out, &stats)
		start := time.Now()
		origins := make([]playoutOrigin, playoutTrackedOrigins+1)
		for i := range origins {
			origins[i].at = time.Now()
			time.Sleep(time.Millisecond)
		}
		latency.enqueue(origins)
		require.Equal(t, playoutTrackedOrigins, latency.count)
		// Each native sample maps to six output samples. The first call spans
		// the evicted origin plus half of the next, so neither can be measured.
		require.NoError(t, latency.WriteSample(make(msdk.PCM16Sample, 9)))
		require.Zero(t, stats.Count.Load())
		require.NoError(t, latency.WriteSample(make(msdk.PCM16Sample, 3)))
		require.Equal(t, uint64(1), stats.Count.Load())
		require.Equal(t, uint64(time.Since(start.Add(time.Millisecond))), stats.SumNs.Load(),
			"the remaining half still belongs to the second original sample")
		require.NoError(t, latency.WriteSample(make(msdk.PCM16Sample, (playoutTrackedOrigins-1)*6)))
		require.Equal(t, uint64(2), stats.Count.Load())
		require.Zero(t, latency.count)
		require.Zero(t, latency.unknown)
		require.Len(t, out.snapshot(), 3, "metadata overflow never discards PCM")
	})
}

type latencySplitSink struct{ msdk.PCM16Writer }

func (s *latencySplitSink) WriteSample(pcm msdk.PCM16Sample) error {
	if err := s.PCM16Writer.WriteSample(pcm[:len(pcm)/2]); err != nil {
		return err
	}
	return s.PCM16Writer.WriteSample(pcm[len(pcm)/2:])
}

type latencyDelayedSink struct {
	playoutCodecSink
	delay time.Duration
}

func (s *latencyDelayedSink) WriteSample(pcm msdk.PCM16Sample) error {
	time.Sleep(s.delay)
	return s.playoutCodecSink.WriteSample(pcm)
}
