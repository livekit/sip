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
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

func TestG711PlayoutRejectsUnsupportedCodec(t *testing.T) {
	out := &playoutCodecSink{rate: 8000}
	require.PanicsWithValue(t, "G711 playout requires PCMU/8000 or PCMA/8000", func() {
		newG711Playout(out, "opus/48000", nil, logger.GetLogger(), false)
	})
	require.Zero(t, out.closeCount, "invalid configuration does not take ownership of the sink")
}

func TestG711PlayoutInvalidInputDoesNotStartOrPoisonStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		header := &rtp.Header{SSRC: 7, SequenceNumber: 1, Timestamp: 160}
		require.NoError(t, handler.HandleRTP(header, nil))
		require.ErrorContains(t, handler.HandleRTP(header, make([]byte, rtp.MTUSize+1)), "exceeds RTP MTU")
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, out.snapshot(), "empty and oversized input must not start the output clock")
		payload, marker := recoveryPayload(g711.ULawSDPNameAndRate, 8000)
		require.NoError(t, handler.HandleRTP(header, payload))
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		frames := out.snapshot()
		require.Len(t, frames, 1)
		require.Equal(t, marker, frames[0].pcm[0], "valid RTP with the same counters remains usable")
	})
}

func TestG711PlayoutExactDuplicateKeepsFirstAcceptedAudio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		header := &rtp.Header{SSRC: 7, SequenceNumber: 100, Timestamp: 10000}
		first, firstMarker := recoveryPayload(g711.ULawSDPNameAndRate, 8000)
		duplicate, duplicateMarker := recoveryPayload(g711.ULawSDPNameAndRate, -8000)
		require.NoError(t, handler.HandleRTP(header, first))
		time.Sleep(20 * time.Millisecond)
		require.NoError(t, handler.HandleRTP(header, duplicate))
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		frames := out.snapshot()
		require.NotEmpty(t, frames)
		require.Equal(t, firstMarker, frames[0].pcm[0])
		for _, frame := range frames {
			require.NotContains(t, frame.pcm, duplicateMarker, "a repeated packet cannot replace accepted PCM")
		}
	})
}

func TestG711PlayoutOldSequenceCannotRestartActiveSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		active, activeMarker := recoveryPayload(g711.ULawSDPNameAndRate, 8000)
		old, oldMarker := recoveryPayload(g711.ULawSDPNameAndRate, -8000)
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 7, SequenceNumber: 100, Timestamp: 10000}, active))
		time.Sleep(20 * time.Millisecond)
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 7, SequenceNumber: 1, Timestamp: 160}, old))
		time.Sleep(40 * time.Millisecond)
		synctest.Wait()
		frames := out.snapshot()
		require.Len(t, frames, 1)
		require.Equal(t, activeMarker, frames[0].pcm[0], "an unconfirmed old sequence must not reset active playout")
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		for _, frame := range out.snapshot() {
			require.NotContains(t, frame.pcm, oldMarker)
		}
	})
}

func TestG711PlayoutCanReuseSourceBeyondRetiredHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		sendSource := func(ssrc uint32) {
			payload, marker := recoveryPayload(g711.ULawSDPNameAndRate, int16(1000*ssrc))
			require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: ssrc, SequenceNumber: 1, Timestamp: 160}, payload))
			time.Sleep(60 * time.Millisecond)
			synctest.Wait()
			frames := out.snapshot()
			require.Equal(t, marker, frames[len(frames)-1].pcm[0], "each idle source handover must deliver the selected source")
			time.Sleep(160 * time.Millisecond)
			synctest.Wait()
		}
		for ssrc := uint32(1); ssrc <= 10; ssrc++ {
			sendSource(ssrc)
		}
		// Recent sources remain protected, but source IDs are not banned for
		// the lifetime of the call after leaving the bounded recent history.
		sendSource(1)
	})
}

func TestG711PlayoutCloseJoinsSuccessfulWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &recoveryCloseErrorSink{
			playoutCodecSink: playoutCodecSink{rate: 8000}, entered: make(chan struct{}), release: make(chan struct{}),
		}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		defer release()
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1, Timestamp: 160}, make([]byte, 160)))
		time.Sleep(60 * time.Millisecond)
		<-out.entered
		done := make(chan struct{})
		go func() { handler.Close(); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Close returned before the successful in-flight write finished")
		default:
		}
		release()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Close failed to join the completed write")
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		require.Len(t, out.snapshot(), 1)
		require.Zero(t, out.closeCount)
	})
}

func TestG711PlayoutCoalescesResetsDuringBlockedOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &recoveryCloseErrorSink{
			playoutCodecSink: playoutCodecSink{rate: 8000}, entered: make(chan struct{}), release: make(chan struct{}),
		}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, logger.GetLogger(), false)
		defer handler.Close()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		defer release()
		initial, _ := recoveryPayload(g711.ULawSDPNameAndRate, 1000)
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1, Timestamp: 160}, initial))
		time.Sleep(60 * time.Millisecond)
		<-out.entered
		intermediate, intermediateMarker := recoveryPayload(g711.ULawSDPNameAndRate, 4000)
		latest, latestMarker := recoveryPayload(g711.ULawSDPNameAndRate, -8000)
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 2, Timestamp: 100000}, intermediate))
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 3, Timestamp: 160}, intermediate))
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 4, Timestamp: 200000}, latest))
		release()
		synctest.Wait()
		time.Sleep(59 * time.Millisecond)
		synctest.Wait()
		require.Len(t, out.snapshot(), 1, "reset notifications cannot bypass the new playout delay")
		time.Sleep(time.Millisecond)
		synctest.Wait()
		frames := out.snapshot()
		require.Len(t, frames, 2)
		require.Equal(t, latestMarker, frames[1].pcm[0])
		require.NotContains(t, frames[1].pcm, intermediateMarker, "coalesced resets retain only the latest timeline")
	})
}

func TestG711PlayoutRechecksDeadlineAfterSlowReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &playoutCodecSink{rate: 8000}
		log := &playoutResetBarrierLogger{Logger: logger.GetLogger(), entered: make(chan struct{}), release: make(chan struct{})}
		handler := newG711Playout(out, g711.ULawSDPNameAndRate, nil, log, false)
		defer handler.Close()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(log.release) }) }
		defer release()
		initial, _ := recoveryPayload(g711.ULawSDPNameAndRate, 4000)
		latest, marker := recoveryPayload(g711.ULawSDPNameAndRate, -8000)
		require.NoError(t, handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1, Timestamp: 160}, initial))
		synctest.Wait() // The worker is waiting for the original 60 ms deadline.
		time.Sleep(20 * time.Millisecond)
		result := make(chan error, 1)
		go func() {
			result <- handler.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 2, Timestamp: 100000}, latest)
		}()
		<-log.entered // A real reset holds the input lock while logging is slow.
		time.Sleep(40 * time.Millisecond)
		// The old timer is now due. Give its worker a turn while the reset
		// still owns the lock; mutex waits are not durable synctest waits.
		for range 8 {
			runtime.Gosched()
		}
		release()
		require.NoError(t, <-result)
		synctest.Wait()
		require.Empty(t, out.snapshot(), "the expired old timer cannot emit the new timeline early")
		time.Sleep(19 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, out.snapshot())
		time.Sleep(time.Millisecond)
		synctest.Wait()
		frames := out.snapshot()
		require.Len(t, frames, 1)
		require.Equal(t, marker, frames[0].pcm[0])
	})
}

type playoutResetBarrierLogger struct {
	logger.Logger
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *playoutResetBarrierLogger) Debugw(message string, values ...any) {
	if message == "audio playout timestamp reset" {
		l.once.Do(func() { close(l.entered); <-l.release })
	}
	l.Logger.Debugw(message, values...)
}
