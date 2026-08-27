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

package lksdk

import (
	"bytes"
	"encoding/binary"
)

// packetTrailerSEIUUID is the UUID embedded in H.264/H.265 SEI
// user_data_unregistered messages that carry an LKTS packet trailer.
var packetTrailerSEIUUID = [16]byte{
	0x3f, 0xa8, 0x5f, 0x64, 0x57, 0x17, 0x45, 0x62,
	0xb3, 0xfc, 0x2c, 0x96, 0x3f, 0x66, 0xaf, 0xa6,
}

const (
	// packetTrailerMagic must remain consistent with kPacketTrailerMagic
	// in rust-sdks/webrtc-sys/include/livekit/packet_trailer.h.
	packetTrailerMagic = "LKTS"

	// TLV tag IDs (XORed with 0xFF on the wire).
	tagUserTimestamp = 0x01 // value: 8 bytes big-endian uint64
	tagFrameId       = 0x02 // value: 4 bytes big-endian uint32

	// TLV element sizes: tag(1) + len(1) + value.
	timestampTlvSize = 10 // 1 + 1 + 8
	frameIdTlvSize   = 6  // 1 + 1 + 4

	// Trailer envelope: [trailer_len: 1B XORed] [magic: 4B raw] = 5 bytes.
	trailerEnvelopeSize = 5

	packetTrailerMinSize = timestampTlvSize + trailerEnvelopeSize
	packetTrailerMaxSize = timestampTlvSize + frameIdTlvSize + trailerEnvelopeSize
)

// FrameMetadata holds the metadata embedded in a packet trailer.
type FrameMetadata struct {
	UserTimestamp uint64
	FrameId       uint32
}

// appendPacketTrailer returns a new slice containing data followed by a
// TLV-encoded packet trailer. All TLV bytes are XORed with 0xFF to prevent
// H.264 NAL start-code sequences from appearing inside the trailer.
//
// Wire layout:
//
//	[original data]
//	[TLV: tag=0x01 ^ 0xFF, len=8 ^ 0xFF, 8-byte BE timestamp ^ 0xFF]  (omitted when UserTimestamp == 0)
//	[TLV: tag=0x02 ^ 0xFF, len=4 ^ 0xFF, 4-byte BE frame_id ^ 0xFF]   (omitted when FrameId == 0)
//	[trailer_len ^ 0xFF]
//	[magic "LKTS" raw]
//
// When both UserTimestamp and FrameId are zero, no trailer is appended and
// the original data is returned unchanged.
func appendPacketTrailer(data []byte, meta FrameMetadata) []byte {
	hasTimestamp := meta.UserTimestamp != 0
	hasFrameId := meta.FrameId != 0
	if !hasTimestamp && !hasFrameId {
		return data
	}

	trailerLen := trailerEnvelopeSize
	if hasTimestamp {
		trailerLen += timestampTlvSize
	}
	if hasFrameId {
		trailerLen += frameIdTlvSize
	}

	out := make([]byte, len(data)+trailerLen)
	copy(out, data)
	pos := len(data)

	// TLV: timestamp_us (only when non-zero)
	if hasTimestamp {
		out[pos] = byte(tagUserTimestamp) ^ 0xFF
		out[pos+1] = 8 ^ 0xFF
		binary.BigEndian.PutUint64(out[pos+2:], ^meta.UserTimestamp)
		pos += timestampTlvSize
	}

	// TLV: frame_id (only when non-zero)
	if hasFrameId {
		out[pos] = byte(tagFrameId) ^ 0xFF
		out[pos+1] = 4 ^ 0xFF
		binary.BigEndian.PutUint32(out[pos+2:], ^meta.FrameId)
		pos += frameIdTlvSize
	}

	// Envelope: trailer_len (XORed) + magic (raw)
	out[pos] = byte(trailerLen) ^ 0xFF
	copy(out[pos+1:], packetTrailerMagic)

	return out
}

// parsePacketTrailer attempts to parse a packet trailer from the end of the
// provided buffer. It returns the extracted FrameMetadata and true when a
// valid trailer is present.
func parsePacketTrailer(data []byte) (FrameMetadata, bool) {
	if len(data) < trailerEnvelopeSize {
		return FrameMetadata{}, false
	}

	magicStart := len(data) - len(packetTrailerMagic)
	if string(data[magicStart:]) != packetTrailerMagic {
		return FrameMetadata{}, false
	}

	trailerLen := int(data[magicStart-1] ^ 0xFF)
	if trailerLen < trailerEnvelopeSize || trailerLen > len(data) {
		return FrameMetadata{}, false
	}

	tlvStart := len(data) - trailerLen
	tlvRegionLen := trailerLen - trailerEnvelopeSize

	var meta FrameMetadata
	foundAny := false
	pos := 0

	for pos+2 <= tlvRegionLen {
		tag := data[tlvStart+pos] ^ 0xFF
		length := int(data[tlvStart+pos+1] ^ 0xFF)
		pos += 2

		if pos+length > tlvRegionLen {
			break
		}

		valStart := tlvStart + pos

		switch {
		case tag == tagUserTimestamp && length == 8:
			var ts uint64
			for i := 0; i < 8; i++ {
				ts = (ts << 8) | uint64(data[valStart+i]^0xFF)
			}
			meta.UserTimestamp = ts
			foundAny = true
		case tag == tagFrameId && length == 4:
			var fid uint32
			for i := 0; i < 4; i++ {
				fid = (fid << 8) | uint32(data[valStart+i]^0xFF)
			}
			meta.FrameId = fid
			foundAny = true
		}

		pos += length
	}

	if !foundAny {
		return FrameMetadata{}, false
	}
	return meta, true
}

// stripPacketTrailer returns the data with the packet trailer removed, if
// present. If no valid trailer is found, the original slice is returned.
func stripPacketTrailer(data []byte) []byte {
	if len(data) < trailerEnvelopeSize {
		return data
	}

	magicStart := len(data) - len(packetTrailerMagic)
	if string(data[magicStart:]) != packetTrailerMagic {
		return data
	}

	trailerLen := int(data[magicStart-1] ^ 0xFF)
	if trailerLen < trailerEnvelopeSize || trailerLen > len(data) {
		return data
	}

	return data[:len(data)-trailerLen]
}

// parseSEIUserData validates the UUID prefix of an SEI user_data_unregistered
// payload and parses the remaining bytes as an LKTS packet trailer.
// userData must start at the UUID (i.e. the first 16 bytes are the UUID).
func parseSEIUserData(userData []byte) (FrameMetadata, bool) {
	if len(userData) < 16+packetTrailerMinSize {
		return FrameMetadata{}, false
	}
	if !bytes.Equal(userData[:16], packetTrailerSEIUUID[:]) {
		return FrameMetadata{}, false
	}
	return parsePacketTrailer(userData[16:])
}
