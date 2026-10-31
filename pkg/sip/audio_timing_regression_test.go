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
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// This is synthetic local RTP/PCM traffic, not a replay of a customer call.
// The real PCMU decoder and default mixer expose the consequence of replaying
// already-elapsed DTX silence as a batch when the next speech packet arrives.
func TestSilenceSuppressionElapsedGapDoesNotOverflowMixer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			sampleRate       = 8000
			samplesPerFrame  = sampleRate / rtp.DefFramesPerSec
			suppressedFrames = 25
		)
		out := &audioTimingTrace{sampleRate: sampleRate}
		var mixStats mixer.Stats
		mix, err := mixer.NewMixer(out, rtp.DefFrameDur, 1,
			mixer.WithStats(&mixStats), mixer.WithOutputChannel())
		require.NoError(t, err)
		defer mix.Stop()
		in := &audioTimingTrace{sampleRate: sampleRate, sink: mix.NewInput()}
		defer in.Close()
		handler := newG711Playout(in, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()

		var seq uint16 = 100
		var timestamp uint32 = 10000
		send := func(amplitude int16) {
			pcm := make(msdk.PCM16Sample, samplesPerFrame)
			for i := range pcm {
				pcm[i] = amplitude
			}
			var payload g711.ULawSample
			payload.Encode(pcm)
			require.NoError(t, handler.HandleRTP(&rtp.Header{
				Version: 2, PayloadType: 0, SequenceNumber: seq, Timestamp: timestamp,
			}, payload))
			seq++
			timestamp += uint32(samplesPerFrame)
		}

		// Establish and drain an ordinary paced stream before silence suppression.
		for range 5 {
			send(8000)
			time.Sleep(rtp.DefFrameDur)
			synctest.Wait()
		}
		require.Zero(t, mixStats.InputFramesDropped.Load(), "paced warmup must not overflow")

		// No RTP sequence numbers are missing. The timestamp and virtual clock
		// both advance across 500 ms of silence; that time has already passed.
		time.Sleep(suppressedFrames * rtp.DefFrameDur)
		synctest.Wait()
		timestamp += uint32(suppressedFrames * samplesPerFrame)
		resumedAt := time.Now()
		send(-8000)
		for range 15 {
			time.Sleep(rtp.DefFrameDur)
			synctest.Wait()
			send(-8000)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		var firstResumedOutput time.Time
		for _, frame := range out.snapshot() {
			if !frame.at.Before(resumedAt) && frame.negative {
				firstResumedOutput = frame.at
				break
			}
		}
		var maxBatch int
		var firstResumedPCM time.Time
		batches := make(map[time.Time]int)
		for _, frame := range in.snapshot() {
			batches[frame.at]++
			maxBatch = max(maxBatch, batches[frame.at])
			if firstResumedPCM.IsZero() && !frame.at.Before(resumedAt) && frame.negative {
				firstResumedPCM = frame.at
			}
		}
		t.Logf("PCM frames at one instant: %d; mixer frames discarded: %d; resumed PCM delay: %s; resumed output delay: %s",
			maxBatch, mixStats.InputFramesDropped.Load(), firstResumedPCM.Sub(resumedAt), firstResumedOutput.Sub(resumedAt))
		require.LessOrEqual(t, maxBatch, 1, "elapsed silence must not be replayed as a PCM burst")
		require.False(t, firstResumedPCM.IsZero(), "resumed speech must reach the mixer input")
		require.LessOrEqual(t, firstResumedPCM.Sub(resumedAt), 60*time.Millisecond,
			"fresh speech must stay within the playout delay budget")
		require.False(t, firstResumedOutput.IsZero(), "resumed speech must reach the mixer output")
		// The real mixer primes three frames. Depending on which goroutine runs
		// first at a shared deadline, it adds up to 60 ms after the 60 ms playout
		// budget. Test both boundaries so mixer phase cannot hide playout delay.
		require.LessOrEqual(t, firstResumedOutput.Sub(resumedAt), 120*time.Millisecond,
			"old silence must not postpone fresh speech")
		require.Zero(t, mixStats.InputFramesDropped.Load(),
			"already-elapsed silence must not overflow the downstream mixer")
	})
}

type audioTimingObservation struct {
	at       time.Time
	negative bool
}

type audioTimingTrace struct {
	sampleRate int
	sink       msdk.PCM16Writer
	mu         sync.Mutex
	frames     []audioTimingObservation
}

func (w *audioTimingTrace) String() string  { return "audio timing trace" }
func (w *audioTimingTrace) SampleRate() int { return w.sampleRate }

func (w *audioTimingTrace) WriteSample(sample msdk.PCM16Sample) error {
	frame := audioTimingObservation{at: time.Now()}
	for _, value := range sample {
		frame.negative = frame.negative || value < 0
	}
	w.mu.Lock()
	w.frames = append(w.frames, frame)
	w.mu.Unlock()
	if w.sink != nil {
		return w.sink.WriteSample(sample)
	}
	return nil
}

func (w *audioTimingTrace) Close() error {
	if w.sink != nil {
		return w.sink.Close()
	}
	return nil
}

func (w *audioTimingTrace) snapshot() []audioTimingObservation {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]audioTimingObservation(nil), w.frames...)
}
