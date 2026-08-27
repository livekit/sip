package lksdk

import (
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestOnSpeakersChanged(t *testing.T) {
	room := NewRoom(nil)

	// Set up the local participant with a SID.
	room.LocalParticipant.updateInfo(&livekit.ParticipantInfo{
		Sid:      "local",
		Identity: "local-identity",
	})

	// Add three remote participants.
	room.addRemoteParticipant(&livekit.ParticipantInfo{
		Sid:      "remote-1",
		Identity: "remote-1-identity",
	}, false)
	room.addRemoteParticipant(&livekit.ParticipantInfo{
		Sid:      "remote-2",
		Identity: "remote-2-identity",
	}, false)
	room.addRemoteParticipant(&livekit.ParticipantInfo{
		Sid:      "remote-3",
		Identity: "remote-3-identity",
	}, false)

	t.Run("active speakers sorted by audio level descending", func(t *testing.T) {
		room.OnSpeakersChanged([]*livekit.SpeakerInfo{
			{Sid: "remote-2", Level: 0.8, Active: true},
			{Sid: "local", Level: 0.5, Active: true},
			{Sid: "remote-1", Level: 0.2, Active: true},
			{Sid: "remote-3", Level: 0.9, Active: true},
		})

		speakers := room.ActiveSpeakers()
		require.Len(t, speakers, 4)
		require.Equal(t, float32(0.9), speakers[0].AudioLevel())
		require.Equal(t, float32(0.8), speakers[1].AudioLevel())
		require.Equal(t, float32(0.5), speakers[2].AudioLevel())
		require.Equal(t, float32(0.2), speakers[3].AudioLevel())

		require.Equal(t, "remote-3", speakers[0].SID())
		require.Equal(t, "remote-2", speakers[1].SID())
		require.Equal(t, "local", speakers[2].SID())
		require.Equal(t, "remote-1", speakers[3].SID())
	})

	t.Run("inactive speaker removed from list", func(t *testing.T) {
		room.OnSpeakersChanged([]*livekit.SpeakerInfo{
			{Sid: "remote-2", Level: 0, Active: false},
		})

		speakers := room.ActiveSpeakers()
		require.Len(t, speakers, 3)
		for _, s := range speakers {
			require.NotEqual(t, "remote-2", s.SID())
		}
	})

	t.Run("updated levels re-sort speakers", func(t *testing.T) {
		// remote-1 was 0.2, now bump it to 1.0 so it becomes the loudest.
		room.OnSpeakersChanged([]*livekit.SpeakerInfo{
			{Sid: "remote-1", Level: 1.0, Active: true},
		})

		speakers := room.ActiveSpeakers()
		require.Len(t, speakers, 3)
		require.Equal(t, "remote-1", speakers[0].SID())
		require.Equal(t, "remote-3", speakers[1].SID())
		require.Equal(t, "local", speakers[2].SID())
	})

	t.Run("unknown participant is ignored", func(t *testing.T) {
		room.OnSpeakersChanged([]*livekit.SpeakerInfo{
			{Sid: "unknown", Level: 0.5, Active: true},
		})

		// List should be unchanged from the previous subtest.
		speakers := room.ActiveSpeakers()
		require.Len(t, speakers, 3)
	})
}

func TestOnRoomMovedUpdatesNameAndSID(t *testing.T) {
	room := NewRoom(nil)
	room.LocalParticipant.updateInfo(&livekit.ParticipantInfo{Sid: "local", Identity: "local-identity"})

	room.name = "old-room"
	room.setSid("RM_old", false)
	require.Equal(t, "old-room", room.Name())
	require.Equal(t, "RM_old", room.SID())

	room.OnRoomMoved(&livekit.RoomMovedResponse{
		Room:        &livekit.Room{Name: "new-room", Sid: "RM_new"},
		Participant: &livekit.ParticipantInfo{Sid: "local", Identity: "local-identity"},
		Token:       "token",
	})

	require.Equal(t, "new-room", room.Name())
	require.Equal(t, "RM_new", room.SID())
}

func TestOnRoomUpdateDeliversLateSID(t *testing.T) {
	room := NewRoom(nil)

	room.OnRoomUpdate(&livekit.Room{Sid: "RM_late"})

	require.Eventually(t, func() bool {
		return room.SID() == "RM_late"
	}, time.Second, 10*time.Millisecond, "SID() never returned the SID delivered via OnRoomUpdate")
}
