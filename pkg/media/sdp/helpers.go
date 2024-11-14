// Copyright 2024 LiveKit, Inc.
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

package sdp

import (
	"net/netip"

	"github.com/pion/sdp/v3"
)

func GetAudio(s *sdp.SessionDescription) *sdp.MediaDescription {
	for _, m := range s.MediaDescriptions {
		if m.MediaName.Media == "audio" {
			return m
		}
	}
	return nil
}

func GetAudioDest(s *sdp.SessionDescription, audio *sdp.MediaDescription) netip.AddrPort {
	if audio == nil || s == nil {
		return netip.AddrPort{}
	}
	ci := s.ConnectionInformation
	if ci == nil || ci.NetworkType != "IN" {
		return netip.AddrPort{}
	}
	ip, err := netip.ParseAddr(ci.Address.Address)
	if err != nil {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(ip, uint16(audio.MediaName.Port.Value))
}
