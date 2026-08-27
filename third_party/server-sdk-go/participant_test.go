package lksdk

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
)

func TestUpdateInfo(t *testing.T) {
	newParticipant := func() *baseParticipant {
		return newBaseParticipant(NewRoomCallback(), protoLogger.GetLogger())
	}

	t.Run("applies update with higher version for same sid", func(t *testing.T) {
		p := newParticipant()
		require.True(t, p.updateInfo(&livekit.ParticipantInfo{Sid: "sid-1", Identity: "alice", Version: 5}, nil))
		require.True(t, p.updateInfo(&livekit.ParticipantInfo{Sid: "sid-1", Identity: "alice", Version: 6}, nil))
		require.Equal(t, "sid-1", p.sid)
		require.Equal(t, uint32(6), p.info.Version)
	})

	t.Run("drops stale update for same sid", func(t *testing.T) {
		p := newParticipant()
		require.True(t, p.updateInfo(&livekit.ParticipantInfo{Sid: "sid-1", Identity: "alice", Version: 5}, nil))
		require.False(t, p.updateInfo(&livekit.ParticipantInfo{Sid: "sid-1", Identity: "alice", Version: 4}, nil))
		require.Equal(t, uint32(5), p.info.Version)
	})

	t.Run("applies lower version update when sid changes (reconnect)", func(t *testing.T) {
		// Simulates a participant reconnecting under the same identity: the
		// server issues a new Sid and resets Version. The update must be
		// applied even though Version went backward.
		p := newParticipant()
		require.True(t, p.updateInfo(&livekit.ParticipantInfo{Sid: "sid-1", Identity: "alice", Version: 10}, nil))
		require.True(t, p.updateInfo(&livekit.ParticipantInfo{Sid: "sid-2", Identity: "alice", Version: 1}, nil))
		require.Equal(t, "sid-2", p.sid)
		require.Equal(t, uint32(1), p.info.Version)
	})
}

func TestAttributeChanges(t *testing.T) {
	diff := attributeChanges(map[string]string{
		"a": "1",
		"b": "2",
	}, map[string]string{
		"a": "2",
		"c": "3",
	})
	require.Equal(t, map[string]string{
		"a": "2",
		"b": "",
		"c": "3",
	}, diff)
}
