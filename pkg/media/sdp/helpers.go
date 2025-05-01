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
	"errors"
	"fmt"
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

// GetAudioDest returns the RTP dst address:port for an audio m=
// it first uses media-level c=, then session-level c=.
func GetAudioDest(s *sdp.SessionDescription, audio *sdp.MediaDescription) (netip.AddrPort, error) {
	if audio == nil || s == nil {
		return netip.AddrPort{}, errors.New("no audio in sdp")
	}

	// pick media-level c=; if absent, fall back to session-level c=
	ci := audio.ConnectionInformation
	if ci == nil {
		ci = s.ConnectionInformation
	}
	var addr string
	if ci != nil && ci.NetworkType == "IN" {
		addr = ci.Address.Address
	} else if s.Origin.NetworkType == "IN" {
		addr = s.Origin.UnicastAddress
	}
	if addr == "" {
		return netip.AddrPort{}, errors.New("no destination address in sdp")
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid destination address %q: %w", addr, err)
	}
	return netip.AddrPortFrom(ip, uint16(audio.MediaName.Port.Value)), nil
}
