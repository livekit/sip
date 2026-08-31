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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
)

const (
	testRemoteIdentity = "agent"
	testRemoteSID      = "PA_remote"
	testRemoteTrackSID = "TR_remote_audio"
)

func testRoomInfo() *livekit.Room {
	return &livekit.Room{Sid: "RM_test", Name: "test-room"}
}

func testLocalInfo(sid string) *livekit.ParticipantInfo {
	return &livekit.ParticipantInfo{
		Sid:      sid,
		Identity: "sip-participant",
		Kind:     livekit.ParticipantInfo_SIP,
	}
}

// testRemoteInfo is the other party in the room, holding one audio track. It
// never reconnects, so its SIDs stay the same across our reconnect, which is
// what makes the SDK treat its track as already known.
func testRemoteInfo() []*livekit.ParticipantInfo {
	return []*livekit.ParticipantInfo{{
		Sid:      testRemoteSID,
		Identity: testRemoteIdentity,
		State:    livekit.ParticipantInfo_ACTIVE,
		Tracks: []*livekit.TrackInfo{{
			Sid:  testRemoteTrackSID,
			Type: livekit.TrackType_AUDIO,
			Name: "microphone",
		}},
	}}
}

// --- reconnect ---------------------------------------------------------------

type reconnectFixture struct {
	room      *Room
	sdk       *lksdk.Room
	published *atomic.Int32 // times SIP's OnTrackPublished handler ran
}

func newReconnectFixture(t *testing.T) *reconnectFixture {
	t.Helper()

	r := NewRoom(logger.GetLogger(), &RoomStats{})
	t.Cleanup(func() { _ = r.Close() })

	cb := r.newRoomCallback(&config.Config{}, RoomConfig{})

	// Wrap the callback before handing it to the SDK: NewRoom copies the fields
	// via Merge, so wrapping afterwards would not be observed.
	var published atomic.Int32
	inner := cb.ParticipantCallback.OnTrackPublished
	cb.ParticipantCallback.OnTrackPublished = func(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		published.Add(1)
		inner(pub, rp)
	}

	sdk := lksdk.NewRoom(cb)
	r.room.Store(sdk)
	r.ready.Break()

	return &reconnectFixture{room: r, sdk: sdk, published: &published}
}

// join brings the fixture to the state of an answered call: joined, remote
// participant present, and subscribing enabled.
func (f *reconnectFixture) join(t *testing.T) {
	t.Helper()

	f.sdk.OnRoomJoined(testRoomInfo(), testLocalInfo("PA_sip_1"), testRemoteInfo(), &livekit.ServerInfo{}, nil)
	require.EqualValues(t, 1, f.published.Load(), "expected the initial publication to be announced")

	f.room.Subscribe()
	require.True(t, f.room.subscribe.Load())
	require.Len(t, f.sdk.GetRemoteParticipants(), 1)

	f.published.Store(0) // only count what happens from here on
}

// reconnectEscalated simulates a failed resume escalating to a reconnect, where
// the SDK skips OnRestarting.
func (f *reconnectFixture) reconnectEscalated() {
	f.sdk.OnResuming()
	f.sdk.OnRoomJoined(testRoomInfo(), testLocalInfo("PA_sip_2"), testRemoteInfo(), &livekit.ServerInfo{}, nil)
	f.sdk.OnRestarted(testRoomInfo(), testLocalInfo("PA_sip_2"), testRemoteInfo())
}

// reconnectServerInitiated simulates the server asking for a reconnect directly,
// where OnRestarting does run.
func (f *reconnectFixture) reconnectServerInitiated() {
	f.sdk.OnRestarting()
	f.sdk.OnRoomJoined(testRoomInfo(), testLocalInfo("PA_sip_2"), testRemoteInfo(), &livekit.ServerInfo{}, nil)
	f.sdk.OnRestarted(testRoomInfo(), testLocalInfo("PA_sip_2"), testRemoteInfo())
}

// TestRoomReconnect covers what happens to a call when the SIP pod loses
// its signal connection to the server and recovers it.
//
// These subtests call the SDK's exported reconnect handlers directly. A real
// reconnect needs a live peer connection to succeed, so a fake signal server
// would have to complete ICE/DTLS to reach the same states.
//
// Only participants connected to the affected pod reconnect. Everyone other
// participant in that room is unaffected and keep their session and their SIDs.
// So "clearing participants" below means dropping our own view of them, not
// removing anyone from the room. There are two ways to reach a reconnect:
//
//	  1- server-initiated: the reconnect is attempted right away, affected participants
//			drop their participant map, and then rebuild it from the OnRoomJoined
//			snapshot and re-announce their tracks.
//	  2- resume-then-escalate: resume is first attempted. If it fails, then switch
//			to a reconnect, and the stale participant map survives so no tracks are
//			announced.
func TestRoomReconnect(t *testing.T) {
	t.Run("escalated reconnect does not re-announce tracks from remote participants", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		f.reconnectEscalated()

		require.EqualValues(t, 0, f.published.Load(),
			"OnTrackPublished must not re-fire on the escalated path")
	})

	// The other path does re-announce, which is why lost audio is intermittent.
	t.Run("server initiated reconnect re-announces tracks from remote participants", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		f.reconnectServerInitiated()

		require.EqualValues(t, 1, f.published.Load(),
			"OnTrackPublished is expected to re-fire when OnRestarting cleared the participants")
	})

	// However the reconnect was reached, SIP must re-issue its subscriptions.
	t.Run("resubscribes after escalated reconnect", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		before := f.room.stats.TrackSubscribes.Load()
		f.reconnectEscalated()

		require.Eventually(t, func() bool {
			return f.room.stats.TrackSubscribes.Load() > before
		}, time.Second, 10*time.Millisecond,
			"SIP must re-subscribe to remote tracks after a reconnect")
	})

	// An outbound call joins the room and publishes before it starts dialing, but
	// defers Subscribe() until the callee answers. A reconnect in that window
	// must not start pulling room audio toward a leg nobody has picked up.
	t.Run("does not subscribe before the call is answered", func(t *testing.T) {
		f := newReconnectFixture(t)

		// Joined, but Subscribe() has not been called yet.
		f.sdk.OnRoomJoined(testRoomInfo(), testLocalInfo("PA_sip_1"), testRemoteInfo(), &livekit.ServerInfo{}, nil)
		require.False(t, f.room.subscribe.Load())

		before := f.room.stats.TrackSubscribes.Load()
		f.reconnectEscalated()

		require.Never(t, func() bool {
			return f.room.stats.TrackSubscribes.Load() > before
		}, 200*time.Millisecond, 20*time.Millisecond,
			"reconnect before answer must not subscribe")
		require.False(t, f.room.subscribe.Load(), "reconnect must not flip the subscribe flag")
	})

	// A resume keeps its subscriptions and the SDK replays them itself.
	t.Run("resume does not resubscribe", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		before := f.room.stats.TrackSubscribes.Load()
		f.sdk.OnResuming()
		f.sdk.OnResumed()

		require.Never(t, func() bool {
			return f.room.stats.TrackSubscribes.Load() > before
		}, 200*time.Millisecond, 20*time.Millisecond,
			"re-subscribing on resume would race the SDK's own sendSyncState")
	})

	// The two counters are mutually exclusive, so a recovery lands in exactly one.
	t.Run("counts resumes and reconnects separately", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		f.reconnectEscalated()
		require.EqualValues(t, 1, f.room.stats.Reconnects.Load())
		require.EqualValues(t, 0, f.room.stats.Resumes.Load())

		f.sdk.OnResuming()
		f.sdk.OnResumed()
		require.EqualValues(t, 1, f.room.stats.Reconnects.Load(), "a resume must not count as a reconnect")
		require.EqualValues(t, 1, f.room.stats.Resumes.Load())
		require.False(t, f.room.stats.Recovering.Load())
	})

	// PublishedFrames and PublishTX keep climbing during a gap whether or not
	// audio reaches the room, so the gap itself has to be visible.
	t.Run("snapshot exposes the gap", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		f.sdk.OnResuming()
		require.True(t, f.room.stats.Load().Recovering, "gap must be visible while it is happening")

		f.sdk.OnResumed()
		require.False(t, f.room.stats.Load().Recovering)
		require.EqualValues(t, 1, f.room.stats.Load().Resumes)
	})

	// A SIP leg can hang up at any point, including mid-recovery, so teardown
	// runs concurrently with the reconnect handlers. This is a smoke test for
	// that overlap, not a race guard: the SDK locks the two goroutines take
	// incidentally order them, so -race does not reliably see the field access.
	t.Run("survives teardown during recovery", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			f.reconnectEscalated()
		}()
		go func() {
			defer wg.Done()
			_ = f.room.CloseWithReason(livekit.DisconnectReason_CLIENT_INITIATED)
		}()
		wg.Wait()

		require.Nil(t, f.room.Room(), "close must clear the room handle")
	})

	// The SID changes on every reconnect and feeds call state.
	t.Run("refreshes participant SID after reconnect", func(t *testing.T) {
		f := newReconnectFixture(t)
		f.join(t)
		f.room.setParticipantFromRoom()
		require.Equal(t, "PA_sip_1", f.room.Participant().ID)

		f.reconnectEscalated()

		require.Eventually(t, func() bool {
			return f.room.Participant().ID == "PA_sip_2"
		}, time.Second, 10*time.Millisecond,
			"cached participant SID must be refreshed after a reconnect")
	})
}
