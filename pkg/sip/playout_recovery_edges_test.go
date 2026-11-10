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
	"io"
	"slices"
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

func TestG711PlayoutSwitchesIdleSourceWithoutRevivingRetiredAudio(t *testing.T) {
	for _, codec := range []string{g711.ULawSDPNameAndRate, g711.ALawSDPNameAndRate} {
		t.Run(codec, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out := &playoutCodecSink{rate: 8000}
				handler := newG711Playout(out, codec, nil, logger.GetLogger(), false)
				defer handler.Close()
				first, firstMarker := recoveryPayload(codec, 4000)
				early, earlyMarker := recoveryPayload(codec, -4000)
				second, secondMarker := recoveryPayload(codec, -8000)
				send := func(ssrc uint32, seq uint16, timestamp uint32, payload []byte) {
					require.NoError(t, handler.HandleRTP(&rtp.Header{
						SSRC: ssrc, SequenceNumber: seq, Timestamp: timestamp,
					}, payload))
				}
				send(1, 1, 160, first)
				time.Sleep(20 * time.Millisecond)
				send(2, 1, 160, early) // Another source must not interrupt the active one.
				time.Sleep(40 * time.Millisecond)
				synctest.Wait()
				frames := out.snapshot()
				require.NotEmpty(t, frames)
				require.Equal(t, firstMarker, frames[0].pcm[0])
				time.Sleep(220 * time.Millisecond)
				synctest.Wait()
				for _, frame := range out.snapshot() {
					require.NotContains(t, frame.pcm, earlyMarker)
				}
				switchedAt := time.Now()
				send(2, 2, 320, second)
				time.Sleep(60 * time.Millisecond)
				synctest.Wait()
				frames = out.snapshot()
				require.Equal(t, switchedAt.Add(60*time.Millisecond), frames[len(frames)-1].at)
				require.Equal(t, secondMarker, frames[len(frames)-1].pcm[0], "the new source must become audible after the idle switch")

				// The replacement is now idle too. A late packet from the retired
				// source must not satisfy the idle gate and take the stream back.
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				lateAt := time.Now()
				send(1, 2, 320, first)
				time.Sleep(80 * time.Millisecond)
				synctest.Wait()
				for _, frame := range out.snapshot() {
					if !frame.at.Before(lateAt) {
						require.NotContains(t, frame.pcm, firstMarker, "retired audio must stay inaudible even when the replacement is idle")
					}
				}
				resumedAt := time.Now()
				send(2, 3, 320+uint32(resumedAt.Sub(switchedAt)*8000/time.Second), second)
				time.Sleep(60 * time.Millisecond)
				synctest.Wait()
				frames = out.snapshot()
				require.Contains(t, frames[len(frames)-1].pcm, secondMarker,
					"the replacement remains audible within 60 ms, including arrivals between output deadlines")
			})
		})
	}
}

func TestG711PlayoutRecoversFromTimestampOnlyDiscontinuity(t *testing.T) {
	for _, codec := range []string{g711.ULawSDPNameAndRate, g711.ALawSDPNameAndRate} {
		for _, direction := range []string{"backward", "large_forward"} {
			t.Run(codec+"/"+direction, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					out := &playoutCodecSink{rate: 8000}
					handler := newG711Playout(out, codec, nil, logger.GetLogger(), false)
					defer handler.Close()
					old, _ := recoveryPayload(codec, 4000)
					fresh, marker := recoveryPayload(codec, -8000)
					seq, timestamp := uint16(100), uint32(10000)
					send := func(payload []byte) {
						require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 7, SequenceNumber: seq, Timestamp: timestamp}, payload))
						seq++
						timestamp += 160
						time.Sleep(20 * time.Millisecond)
						synctest.Wait()
					}
					for range 8 {
						send(old)
					}
					if direction == "backward" {
						timestamp = 160
					} else {
						timestamp += 2 * 8000 // The source clock jumps; wall time does not.
					}
					changedAt := time.Now()
					for range 10 {
						send(fresh) // Sequence numbers continue without restarting.
					}
					var recovered time.Time
					var previous time.Time
					for _, frame := range out.snapshot() {
						if frame.at.Before(changedAt) || !slices.Contains(frame.pcm, marker) {
							continue
						}
						if recovered.IsZero() {
							recovered = frame.at
						} else {
							require.Equal(t, 20*time.Millisecond, frame.at.Sub(previous), "clock recovery must preserve playout cadence")
						}
						previous = frame.at
					}
					require.False(t, recovered.IsZero(), "timestamp discontinuity must not permanently mute progressive packets")
					require.LessOrEqual(t, recovered.Sub(changedAt), 60*time.Millisecond)
				})
			})
		}
	}
}

func TestG711PlayoutCloseJoinsFailingWriteWithoutRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &recoveryCloseErrorSink{
			playoutCodecSink: playoutCodecSink{rate: 8000},
			entered:          make(chan struct{}), release: make(chan struct{}), failure: errors.New("output closed during write"),
		}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		defer release()
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1, Timestamp: 160}, make([]byte, 160)))
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		select {
		case <-out.entered:
		default:
			t.Fatal("output write did not start")
		}
		done := make(chan struct{})
		go func() {
			handler.Close()
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Close returned before the in-flight output write finished")
		default:
		}
		release()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Close did not finish after the output write returned its error")
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		require.Len(t, out.snapshot(), 1, "a failed write during shutdown must not be retried")
		require.Zero(t, out.closeCount, "the caller retains ownership of the shared sink")
		require.ErrorIs(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 2, Timestamp: 320}, make([]byte, 160)), io.ErrClosedPipe)
	})
}

func recoveryPayload(codec string, amplitude int16) ([]byte, int16) {
	pcm := make(msdk.PCM16Sample, 160)
	for i := range pcm {
		pcm[i] = amplitude
	}
	payload, quantized := playoutCodecEncode(codec, pcm)
	return payload, quantized[0]
}

type recoveryCloseErrorSink struct {
	playoutCodecSink
	entered chan struct{}
	release chan struct{}
	failure error
	once    sync.Once
}

func (s *recoveryCloseErrorSink) WriteSample(pcm msdk.PCM16Sample) error {
	if err := s.playoutCodecSink.WriteSample(pcm); err != nil {
		return err
	}
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.failure
}
