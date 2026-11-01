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
	"testing"
	"testing/synctest"
	"time"

	mediaRTP "github.com/livekit/media-sdk/rtp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestPlayoutLatencyEmptyPayloadPreservesPendingOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stats LatencyStats
		latency := newPlayoutLatency(&playoutCodecSink{rate: 8000}, &stats)
		var received [][]byte
		entry := &playoutArrivalHandler{
			HandlerCloser: mediaRTP.NewNopCloser(mediaRTP.HandlerFunc(func(_ *rtp.Header, payload []byte) error {
				received = append(received, payload)
				return nil
			})),
			latency: latency,
		}
		arrival := time.Now()
		require.NoError(t, entry.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: 1}, []byte{1, 2, 3}))
		pending := received[0]
		time.Sleep(time.Millisecond)

		// Empty RTP payloads have no sample origin. They must neither consume
		// a pending packet's origin nor evict it from the bounded registry.
		for i := range playoutTrackedPackets + 1 {
			var empty []byte
			if i%2 != 0 {
				empty = pending[:0] // A zero length view still owns backing storage.
			}
			require.NoError(t, entry.HandleRTP(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i + 2)}, empty))
			require.Empty(t, received[len(received)-1], "empty packets remain empty at the downstream handler")
			require.True(t, latency.take(empty).IsZero(), "an empty payload cannot acquire another packet's arrival")
		}
		require.Equal(t, arrival, latency.take(pending), "empty arrivals must not evict or consume pending audio metadata")
		require.True(t, latency.take(pending).IsZero(), "the nonempty packet's origin is still consumed exactly once")
		require.Zero(t, stats.Load().Count, "metadata registration alone is not a completed PCM write")
	})
}
