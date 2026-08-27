package lksdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseH264SEIPacketTrailer(t *testing.T) {
	wantMeta := FrameMetadata{UserTimestamp: 1234567890}

	buildNAL := func(uuid [16]byte, meta FrameMetadata) []byte {
		// NAL header for SEI (nal_unit_type = 6).
		nal := []byte{0x06}

		// Build user_data payload: UUID + LKTS trailer.
		trailer := appendPacketTrailer(nil, meta)
		userData := append(uuid[:], trailer...)

		// payloadType = 5, payloadSize = len(userData)
		nal = append(nal, 0x05, byte(len(userData)))
		nal = append(nal, userData...)
		return nal
	}

	t.Run("accepts matching UUID with timestamp only", func(t *testing.T) {
		got, ok := parseH264SEIPacketTrailer(buildNAL(packetTrailerSEIUUID, wantMeta))
		require.True(t, ok)
		require.Equal(t, wantMeta.UserTimestamp, got.UserTimestamp)
		require.Equal(t, uint32(0), got.FrameId)
	})

	t.Run("accepts matching UUID with timestamp and frame_id", func(t *testing.T) {
		meta := FrameMetadata{UserTimestamp: 42, FrameId: 12345}
		got, ok := parseH264SEIPacketTrailer(buildNAL(packetTrailerSEIUUID, meta))
		require.True(t, ok)
		require.Equal(t, uint64(42), got.UserTimestamp)
		require.Equal(t, uint32(12345), got.FrameId)
	})

	t.Run("rejects non-matching UUID", func(t *testing.T) {
		badUUID := packetTrailerSEIUUID
		badUUID[0] ^= 0xFF

		_, ok := parseH264SEIPacketTrailer(buildNAL(badUUID, wantMeta))
		require.False(t, ok)
	})

	t.Run("rejects truncated trailer", func(t *testing.T) {
		nal := buildNAL(packetTrailerSEIUUID, wantMeta)
		// Chop off the last few bytes to corrupt the trailer.
		nal = nal[:len(nal)-3]
		_, ok := parseH264SEIPacketTrailer(nal)
		require.False(t, ok)
	})

	t.Run("rejects wrong payloadType", func(t *testing.T) {
		nal := []byte{0x06, 0x04, 0x18} // payloadType=4, payloadSize=24
		nal = append(nal, make([]byte, 24)...)
		_, ok := parseH264SEIPacketTrailer(nal)
		require.False(t, ok)
	})

	t.Run("rejects nal too short", func(t *testing.T) {
		_, ok := parseH264SEIPacketTrailer([]byte{0x06})
		require.False(t, ok)
	})
}
