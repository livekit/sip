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
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/opus"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// These tests exercise actual G711, native soxr and Opus implementations.
// Synthetic tones establish signal preservation; they are not customer audio
// or an end-to-end SIP/SFU call-quality qualification.
func TestG711PlayoutCodecRoundTripAfterElapsedSilence(t *testing.T) {
	for _, codec := range []string{g711.ULawSDPNameAndRate, g711.ALawSDPNameAndRate} {
		t.Run(codec, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				baselineDelay := playoutCodecBaselineDelay(t, codec)
				decoded := &playoutCodecSink{rate: 48000}
				decoder, err := opus.Decode(decoded, 1, logger.GetLogger())
				require.NoError(t, err)
				encoder, err := opus.Encode(decoder, 1, logger.GetLogger())
				require.NoError(t, err)
				defer encoder.Close()
				var stats PlayoutStats
				handler := newG711Playout(encoder, codec, &stats, logger.GetLogger(), false)
				defer handler.Close()
				seq, stamp := uint16(100), uint32(10000)
				phase := 0
				send := func(frequency float64) {
					pcm := make(msdk.PCM16Sample, 160)
					for i := range pcm {
						pcm[i] = int16(10000 * math.Sin(2*math.Pi*frequency*float64(phase+i)/8000))
					}
					phase += len(pcm)
					payload, _ := playoutCodecEncode(codec, pcm)
					require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: seq, Timestamp: stamp, SSRC: 7}, payload))
					// UDP receive buffers are reused immediately after the callback.
					clear(payload)
					seq++
					stamp += uint32(len(pcm))
				}
				for range 20 {
					send(440)
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
				}
				// Contiguous sequence numbers, but 500 ms of elapsed timestamp time.
				time.Sleep(500 * time.Millisecond)
				stamp += 4000
				synctest.Wait()
				resumed := time.Now()
				for range 20 {
					send(880)
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
				}
				time.Sleep(100 * time.Millisecond)
				synctest.Wait()

				frames := decoded.snapshot()
				var initial, resumedFrames int
				var firstResumed time.Time
				for i, frame := range frames {
					require.Len(t, frame.pcm, 960, "Opus output must remain 20 ms at 48 kHz")
					if i > 0 {
						require.GreaterOrEqual(t, frame.at.Sub(frames[i-1].at), 20*time.Millisecond,
							"resampling and codec publication must not recreate a catch-up burst")
					}
					if frame.at.Before(resumed) && playoutToneFraction(frame.pcm, 48000, 440) > 0.70 {
						initial++
					}
					if !frame.at.Before(resumed) && playoutToneFraction(frame.pcm, 48000, 880) > 0.70 {
						resumedFrames++
						if firstResumed.IsZero() {
							firstResumed = frame.at
						}
					}
				}
				require.GreaterOrEqual(t, initial, 15, "initial reference tone must survive real codecs")
				require.GreaterOrEqual(t, resumedFrames, 15, "resumed reference tone must survive real codecs")
				require.False(t, firstResumed.IsZero())
				require.LessOrEqual(t, firstResumed.Sub(resumed)-baselineDelay, 80*time.Millisecond,
					"playout may add its 60 ms queue plus one 20 ms frame phase to the measured real-codec baseline")
				require.Zero(t, stats.LateSamples.Load())
				require.Zero(t, stats.OverflowSamples.Load())
				require.Zero(t, stats.WriteErrors.Load())
				t.Logf("actual %s -> soxr -> Opus roundtrip: initial=%d resumed=%d first resumed=%s baseline=%s added=%s",
					codec, initial, resumedFrames, firstResumed.Sub(resumed), baselineDelay, firstResumed.Sub(resumed)-baselineDelay)
			})
		})
	}
}

// Measure the same tone transition through the real codec/resampler chain
// without playout. Opus speech-mode convergence is not playout queue latency.
func playoutCodecBaselineDelay(t *testing.T, codec string) time.Duration {
	t.Helper()
	out := &playoutCodecSink{rate: 48000}
	decoder, err := opus.Decode(out, 1, logger.GetLogger())
	require.NoError(t, err)
	encoder, err := opus.Encode(decoder, 1, logger.GetLogger())
	require.NoError(t, err)
	payloadType := byte(0)
	codecName := g711.ULawSDPNameOnly
	if codec == g711.ALawSDPNameAndRate {
		payloadType = 8
		codecName = g711.ALawSDPNameOnly
	}
	handler, err := rtp.DecodePCMWithCodec(encoder, audioCodecByName(t, codecName), msdk.CodecConfig{SampleRate: 8000}, payloadType)
	require.NoError(t, err)
	defer handler.Close()
	seq, stamp, phase := uint16(100), uint32(10000), 0
	send := func(frequency float64) {
		pcm := make(msdk.PCM16Sample, 160)
		for i := range pcm {
			pcm[i] = int16(10000 * math.Sin(2*math.Pi*frequency*float64(phase+i)/8000))
		}
		phase += len(pcm)
		payload, _ := playoutCodecEncode(codec, pcm)
		require.NoError(t, handler.HandleRTP(&rtp.Header{PayloadType: payloadType, SequenceNumber: seq, Timestamp: stamp}, payload))
		seq++
		stamp += 160
		time.Sleep(20 * time.Millisecond)
	}
	for range 20 {
		send(440)
	}
	for range 25 {
		send(0)
	}
	resumed := time.Now()
	for range 20 {
		send(880)
	}
	for _, frame := range out.snapshot() {
		if !frame.at.Before(resumed) && playoutToneFraction(frame.pcm, 48000, 880) > 0.70 {
			return frame.at.Sub(resumed)
		}
	}
	t.Fatal("codec-only control did not recover the reference tone")
	return 0
}

func TestG711PlayoutRecoversAfterForwardClockJump(t *testing.T) {
	for _, jump := range []time.Duration{300 * time.Millisecond, 500 * time.Millisecond} {
		t.Run(jump.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out := &playoutCodecSink{rate: 8000}
				var stats PlayoutStats
				handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
				defer handler.Close()
				seq, stamp := uint16(100), uint32(10000)
				send := func(amplitude int16) int16 {
					pcm := make(msdk.PCM16Sample, 160)
					for i := range pcm {
						pcm[i] = amplitude
					}
					payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
					require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: seq, Timestamp: stamp, SSRC: 5}, payload))
					seq++
					stamp += 160
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
					return quantized[0]
				}
				for range 10 {
					send(8000)
				}
				stamp += uint32(jump * 8000 / time.Second)
				jumpedAt := time.Now()
				var marker int16
				for range 100 {
					marker = send(12000)
				}
				var recovered time.Time
				for _, frame := range out.snapshot() {
					if !frame.at.Before(jumpedAt) && slices.Contains(frame.pcm, marker) {
						recovered = frame.at
						break
					}
				}
				require.False(t, recovered.IsZero(), "forward clock jump followed by normal paced packets must not mute the source indefinitely")
				require.LessOrEqual(t, recovered.Sub(jumpedAt), 500*time.Millisecond,
					"discontinuous media must recover within a bounded interval")
			})
		})
	}
}

func TestG711PlayoutVariablePacketDurationAndWrap(t *testing.T) {
	for _, codec := range []string{g711.ULawSDPNameAndRate, g711.ALawSDPNameAndRate} {
		for _, packetMS := range []int{10, 20, 30, 40, 60} {
			t.Run(fmt.Sprintf("%s/%dms", codec, packetMS), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					out := &playoutCodecSink{rate: 8000}
					var stats PlayoutStats
					handler := newG711Playout(out, codec, &stats, logger.GetLogger(), false)
					defer handler.Close()
					seq, stamp := uint16(65530), uint32(0xfffffff0)
					var expected msdk.PCM16Sample
					for n := range 20 {
						pcm := make(msdk.PCM16Sample, packetMS*8)
						for i := range pcm {
							pcm[i] = int16(1000 + 100*n + i%20)
						}
						payload, quantized := playoutCodecEncode(codec, pcm)
						expected = append(expected, quantized...)
						require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: seq, Timestamp: stamp, SSRC: 9}, payload))
						clear(payload)
						seq++
						stamp += uint32(len(pcm))
						time.Sleep(time.Duration(packetMS) * time.Millisecond)
						synctest.Wait()
					}
					time.Sleep(100 * time.Millisecond)
					synctest.Wait()
					var received msdk.PCM16Sample
					for _, frame := range out.snapshot() {
						require.Len(t, frame.pcm, 160)
						received = append(received, frame.pcm...)
					}
					require.GreaterOrEqual(t, len(received), len(expected))
					require.Equal(t, expected, received[:len(expected)], "accepted G711 samples must survive without duplication or truncation")
					require.Zero(t, stats.LateSamples.Load())
					require.Zero(t, stats.OverflowSamples.Load())
					require.Zero(t, stats.Resets.Load(), "ordinary RTP timestamp and sequence wrap must preserve the timeline")
				})
			})
		}
	}
}

func TestG711PlayoutWriterErrorAndOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("test sink failure")
		out := &playoutErrorSink{
			playoutCodecSink: playoutCodecSink{rate: 8000},
			failure:          failure,
			fail:             func(int) bool { return true },
		}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: 1, Timestamp: 160}, make([]byte, 160)))
		time.Sleep(1020 * time.Millisecond) // First attempt at 60 ms, then 48 more at 20 ms intervals.
		synctest.Wait()
		require.Equal(t, uint64(49), stats.WriteErrors.Load())
		require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: 2, Timestamp: 320}, make([]byte, 160)))
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		require.ErrorIs(t, handler.HandleRTP(&rtp.Header{SequenceNumber: 3, Timestamp: 480}, make([]byte, 160)), failure)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, uint64(50), stats.WriteErrors.Load(), "persistent failure stops after 50 consecutive attempts")
		out.mu.Lock()
		attempts := slices.Clone(out.attempts)
		out.mu.Unlock()
		require.Len(t, attempts, 50)
		for i := 1; i < len(attempts); i++ {
			require.Equal(t, 20*time.Millisecond, attempts[i].Sub(attempts[i-1]))
		}
		handler.Close()
		handler.Close()
		require.Zero(t, out.closeCount, "caller retains ownership of the shared output writer")
		require.ErrorIs(t, handler.HandleRTP(&rtp.Header{}, make([]byte, 160)), io.ErrClosedPipe)
	})
}

func TestG711PlayoutRecoversAfterTransientWriterError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutErrorSink{
			playoutCodecSink: playoutCodecSink{rate: 8000},
			failure:          errors.New("temporary output failure"),
			fail:             func(attempt int) bool { return attempt == 1 },
		}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		start := time.Now()
		pcm := make(msdk.PCM16Sample, 160)
		for i := range pcm {
			pcm[i] = 8000
		}
		payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
		for i := range 10 {
			require.NoError(t, handler.HandleRTP(&rtp.Header{
				SSRC: 7, SequenceNumber: uint16(i + 1), Timestamp: uint32((i + 1) * 160),
			}, payload), "one downstream error must not permanently reject later RTP")
			time.Sleep(20 * time.Millisecond)
			synctest.Wait()
		}
		frames := out.snapshot()
		require.NotEmpty(t, frames, "healthy output must resume after the failed frame")
		require.Equal(t, quantized, frames[0].pcm)
		require.Equal(t, start.Add(80*time.Millisecond), frames[0].at,
			"retry the next frame at its deadline, not in a catch-up burst")
		require.Equal(t, uint64(1), stats.WriteErrors.Load())
		out.mu.Lock()
		attempts := slices.Clone(out.attempts)
		out.mu.Unlock()
		for i := 1; i < len(attempts); i++ {
			require.Equal(t, 20*time.Millisecond, attempts[i].Sub(attempts[i-1]))
		}
	})
}

func TestG711PlayoutWriterSuccessResetsConsecutiveErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutErrorSink{
			playoutCodecSink: playoutCodecSink{rate: 8000},
			failure:          errors.New("temporary output failure"),
			fail:             func(attempt int) bool { return attempt%50 != 0 },
		}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		pcm := make(msdk.PCM16Sample, 160)
		for i := range pcm {
			pcm[i] = 8000
		}
		payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
		for i := range 102 { // 100 output deadlines, including two runs of 49 failures.
			require.NoError(t, handler.HandleRTP(&rtp.Header{
				SSRC: 7, SequenceNumber: uint16(i + 1), Timestamp: uint32((i + 1) * 160),
			}, payload), "a successful write must reset the consecutive-error limit")
			time.Sleep(20 * time.Millisecond)
			synctest.Wait()
		}
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 7, SequenceNumber: 103, Timestamp: 103 * 160}, payload))
		frames := out.snapshot()
		require.Len(t, frames, 2)
		for _, frame := range frames {
			require.Equal(t, quantized, frame.pcm)
		}
		require.Equal(t, uint64(98), stats.WriteErrors.Load(), "total failures must not be mistaken for consecutive failures")
	})
}

type playoutErrorSink struct {
	playoutCodecSink
	failure  error
	fail     func(attempt int) bool
	attempts []time.Time
}

func (s *playoutErrorSink) WriteSample(pcm msdk.PCM16Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.attempts = append(s.attempts, now)
	if s.fail(len(s.attempts)) {
		return s.failure
	}
	s.frames = append(s.frames, playoutCodecFrame{at: now, pcm: slices.Clone(pcm)})
	return nil
}

func TestG711PlayoutRecoversAfterSenderRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		seq, stamp := uint16(5000), uint32(500000)
		send := func(amplitude int16) int16 {
			pcm := make(msdk.PCM16Sample, 160)
			for i := range pcm {
				pcm[i] = amplitude
			}
			payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
			require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: seq, Timestamp: stamp, SSRC: 17}, payload))
			seq++
			stamp += 160
			time.Sleep(20 * time.Millisecond)
			synctest.Wait()
			return quantized[0]
		}
		for range 10 {
			send(8000)
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		seq, stamp = 1, 160 // Same source restarts both RTP counters after an idle gap.
		restartedAt := time.Now()
		var marker int16
		for range 50 {
			marker = send(12000)
		}
		var recovered time.Time
		for _, frame := range out.snapshot() {
			if !frame.at.Before(restartedAt) && slices.Contains(frame.pcm, marker) {
				recovered = frame.at
				break
			}
		}
		require.False(t, recovered.IsZero(), "a sender restart must not mute its subsequent progressive media indefinitely")
		require.LessOrEqual(t, recovered.Sub(restartedAt), 500*time.Millisecond)
	})
}

func TestG711PlayoutReordersWithinDelayWithoutReplacingAcceptedAudio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		var expected msdk.PCM16Sample
		payloads := make([][]byte, 4)
		for n := range payloads {
			pcm := make(msdk.PCM16Sample, 160)
			for i := range pcm {
				pcm[i] = int16(4000 + n*1000)
			}
			payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
			payloads[n] = payload
			expected = append(expected, quantized...)
		}
		send := func(n int, payload []byte) {
			require.NoError(t, handler.HandleRTP(&rtp.Header{
				SequenceNumber: uint16(100 + n), Timestamp: uint32(10000 + n*160), SSRC: 23,
			}, payload))
		}
		send(0, payloads[0])
		time.Sleep(20 * time.Millisecond)
		send(2, payloads[2])
		time.Sleep(10 * time.Millisecond)
		send(1, payloads[1]) // Arrives after packet 2, before its own playout deadline.
		time.Sleep(10 * time.Millisecond)
		contradictory := make(msdk.PCM16Sample, 160)
		for i := range contradictory {
			contradictory[i] = -8000
		}
		duplicate, _ := playoutCodecEncode(g711.ULawSDPNameAndRate, contradictory)
		send(1, duplicate) // Duplicate metadata must not overwrite accepted audio.
		time.Sleep(20 * time.Millisecond)
		send(3, payloads[3])
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		var received msdk.PCM16Sample
		for _, frame := range out.snapshot() {
			received = append(received, frame.pcm...)
		}
		require.GreaterOrEqual(t, len(received), len(expected))
		require.Equal(t, expected, received[:len(expected)],
			"reordered samples must retain timestamp order and first accepted content")
		require.Zero(t, stats.LateSamples.Load())
		require.Zero(t, stats.OverflowSamples.Load())
		require.Zero(t, stats.Resets.Load())
	})
}

func TestG711PlayoutRecoversAfterArrivalPauseWithoutTimestampAdvance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		var stats PlayoutStats
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, &stats, logger.GetLogger(), false)
		defer handler.Close()
		seq, stamp := uint16(100), uint32(10000)
		send := func(amplitude int16) int16 {
			pcm := make(msdk.PCM16Sample, 160)
			for i := range pcm {
				pcm[i] = amplitude
			}
			payload, quantized := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
			require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: seq, Timestamp: stamp, SSRC: 31}, payload))
			seq++
			stamp += 160
			time.Sleep(20 * time.Millisecond)
			synctest.Wait()
			return quantized[0]
		}
		for range 10 {
			send(8000)
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		// The sender resumes its old media clock without catch-up packets.
		// Later packets are fresh and paced, although the old wall-clock mapping
		// would classify every one as late forever.
		resumedAt := time.Now()
		var marker int16
		for range 100 {
			marker = send(12000)
		}
		var recovered time.Time
		for _, frame := range out.snapshot() {
			if !frame.at.Before(resumedAt) && slices.Contains(frame.pcm, marker) {
				recovered = frame.at
				break
			}
		}
		require.False(t, recovered.IsZero(), "fresh progressive media behind an obsolete clock mapping must recover")
		require.LessOrEqual(t, recovered.Sub(resumedAt), 500*time.Millisecond)
	})
}

func TestG711PlayoutConcurrentCloseAndReceive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		require.NoError(t, handler.HandleRTP(&rtp.Header{SequenceNumber: 1, Timestamp: 160}, make([]byte, 160)))
		time.Sleep(80 * time.Millisecond)
		synctest.Wait()
		var wg sync.WaitGroup
		for n := range 16 {
			wg.Go(func() {
				if n%2 == 0 {
					handler.Close()
				} else {
					_ = handler.HandleRTP(&rtp.Header{SequenceNumber: uint16(n + 2), Timestamp: uint32((n + 2) * 160)}, make([]byte, 160))
				}
			})
		}
		wg.Wait()
		before := len(out.snapshot())
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		require.Len(t, out.snapshot(), before, "closed handler must never write downstream again")
		require.Zero(t, out.closeCount)
	})
}

type playoutCodecFrame struct {
	at  time.Time
	pcm msdk.PCM16Sample
}

type playoutCodecSink struct {
	mu         sync.Mutex
	rate       int
	frames     []playoutCodecFrame
	writeErr   error
	closeCount int
}

func (s *playoutCodecSink) String() string  { return "codec test sink" }
func (s *playoutCodecSink) SampleRate() int { return s.rate }
func (s *playoutCodecSink) WriteSample(pcm msdk.PCM16Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.frames = append(s.frames, playoutCodecFrame{at: time.Now(), pcm: slices.Clone(pcm)})
	return nil
}
func (s *playoutCodecSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCount++
	return nil
}
func (s *playoutCodecSink) snapshot() []playoutCodecFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.frames)
}

func playoutCodecEncode(codec string, pcm msdk.PCM16Sample) ([]byte, msdk.PCM16Sample) {
	if codec == g711.ULawSDPNameAndRate {
		var payload g711.ULawSample
		payload.Encode(pcm)
		return payload, payload.Decode()
	}
	var payload g711.ALawSample
	payload.Encode(pcm)
	return payload, payload.Decode()
}

// Project onto both phases of a reference tone, allowing codec phase delay.
// Require adequate absolute power so silence cannot satisfy the signal check.
func playoutToneFraction(pcm msdk.PCM16Sample, rate int, frequency float64) float64 {
	var sine, cosine, power float64
	for i, sample := range pcm {
		v := float64(sample)
		angle := 2 * math.Pi * frequency * float64(i) / float64(rate)
		sine += v * math.Sin(angle)
		cosine += v * math.Cos(angle)
		power += v * v
	}
	if power < float64(len(pcm))*1000000 {
		return 0
	}
	return 2 * (sine*sine + cosine*cosine) / (float64(len(pcm)) * power)
}

// An upstream jitter buffer has already selected its source and confirmed any
// sequence restart. Playout must not impose a second source probation period.
func TestG711PlayoutAcceptsOrderedSourceAndSequenceRestart(t *testing.T) {
	for _, changeSource := range []bool{false, true} {
		name := "sequence_restart"
		if changeSource {
			name = "selected_source"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out := &playoutCodecSink{rate: 8000}
				handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), true)
				defer handler.Close()
				ssrc, seq, stamp := uint32(7), uint16(100), uint32(10000)
				send := func(amplitude int16) {
					pcm := make(msdk.PCM16Sample, 160)
					for i := range pcm {
						pcm[i] = amplitude
					}
					payload, _ := playoutCodecEncode(g711.ULawSDPNameAndRate, pcm)
					require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: ssrc, SequenceNumber: seq, Timestamp: stamp}, payload))
					seq++
					stamp += 160
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
				}
				for range 8 {
					send(8000)
				}
				if changeSource {
					ssrc++
				}
				seq, stamp = 1, 160
				restartedAt := time.Now()
				for range 8 {
					send(-8000)
				}
				var recovered time.Time
				for _, frame := range out.snapshot() {
					if !frame.at.Before(restartedAt) && slices.ContainsFunc(frame.pcm, func(v int16) bool { return v < -5000 }) {
						recovered = frame.at
						break
					}
				}
				require.False(t, recovered.IsZero(), "selected source must become audible")
				require.LessOrEqual(t, recovered.Sub(restartedAt), 60*time.Millisecond, "source selection was already confirmed upstream")
			})
		})
	}
}
