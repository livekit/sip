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

// parseH265SEIPacketTrailer parses H265 prefix SEI NAL units (type 39) carrying
// user_data_unregistered messages with an LKTS packet trailer and returns
// the embedded FrameMetadata when detected.
//
// Expected payload format (after the 2-byte NAL header):
//
//	payloadType  = 5 (user_data_unregistered)
//	payloadSize  = variable
//	UUID         = 16 bytes (3fa85f64-5717-4562-b3fc-2c963f66afa6)
//	trailer      = LKTS TLV-encoded packet trailer (XOR'd with 0xFF)
func parseH265SEIPacketTrailer(nalData []byte) (FrameMetadata, bool) {
	if len(nalData) < 3 {
		return FrameMetadata{}, false
	}

	// Skip 2-byte NAL header.
	payload := nalData[2:]
	i := 0

	// Parse payloadType (can be extended with 0xFF bytes).
	payloadType := 0
	for i < len(payload) && payload[i] == 0xFF {
		payloadType += 255
		i++
	}
	if i >= len(payload) {
		return FrameMetadata{}, false
	}
	payloadType += int(payload[i])
	i++

	if payloadType != 5 {
		return FrameMetadata{}, false
	}

	// Parse payloadSize (can be extended with 0xFF bytes).
	payloadSize := 0
	for i < len(payload) && payload[i] == 0xFF {
		payloadSize += 255
		i++
	}
	if i >= len(payload) {
		return FrameMetadata{}, false
	}
	payloadSize += int(payload[i])
	i++

	if len(payload) < i+payloadSize {
		return FrameMetadata{}, false
	}

	return parseSEIUserData(payload[i : i+payloadSize])
}
