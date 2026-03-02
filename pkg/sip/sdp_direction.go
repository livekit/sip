// Copyright 2023 LiveKit, Inc.
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
	"net/netip"

	lksdp "github.com/livekit/media-sdk/sdp"
	psdp "github.com/pion/sdp/v3"
)

// getMediaDirection extracts the SDP direction attribute from the audio media description.
// Falls back to session-level attributes. Returns "sendrecv" if no direction attribute is found (RFC 3264 default).
func getMediaDirection(sd *psdp.SessionDescription) string {
	audio := lksdp.GetAudio(sd)
	if audio != nil {
		for _, attr := range audio.Attributes {
			switch attr.Key {
			case "sendonly", "recvonly", "sendrecv", "inactive":
				return attr.Key
			}
		}
	}
	// Check session-level attributes as fallback
	for _, attr := range sd.Attributes {
		switch attr.Key {
		case "sendonly", "recvonly", "sendrecv", "inactive":
			return attr.Key
		}
	}
	return "sendrecv"
}

// complementDirection returns the RFC 3264 complement direction.
// sendonly ↔ recvonly, sendrecv ↔ sendrecv, inactive ↔ inactive.
func complementDirection(dir string) string {
	switch dir {
	case "sendonly":
		return "recvonly"
	case "recvonly":
		return "sendonly"
	case "inactive":
		return "inactive"
	default:
		return "sendrecv"
	}
}

// setMediaDirection parses the SDP, replaces the direction attribute on the audio media
// description with the given direction, and re-marshals the SDP.
func setMediaDirection(sdpData []byte, direction string) ([]byte, error) {
	sd := new(psdp.SessionDescription)
	if err := sd.Unmarshal(sdpData); err != nil {
		return nil, err
	}
	audio := lksdp.GetAudio(sd)
	if audio == nil {
		return sdpData, nil
	}
	// Remove existing direction attributes and add the new one
	filtered := make([]psdp.Attribute, 0, len(audio.Attributes))
	for _, attr := range audio.Attributes {
		switch attr.Key {
		case "sendonly", "recvonly", "sendrecv", "inactive":
			continue
		default:
			filtered = append(filtered, attr)
		}
	}
	filtered = append(filtered, psdp.Attribute{Key: direction})
	audio.Attributes = filtered

	return sd.Marshal()
}

// getRemoteMediaAddr extracts the remote audio media address from the parsed SDP.
func getRemoteMediaAddr(sd *psdp.SessionDescription) (netip.AddrPort, bool) {
	audio := lksdp.GetAudio(sd)
	if audio == nil {
		return netip.AddrPort{}, false
	}
	addr, err := lksdp.GetAudioDest(sd, audio)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return addr, true
}
