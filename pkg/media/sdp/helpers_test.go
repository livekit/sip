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
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"
)

func TestGetAudioDest(t *testing.T) {
	tests := []struct {
		name     string
		session  *sdp.SessionDescription
		audio    *sdp.MediaDescription
		expected netip.AddrPort
		error    bool
	}{
		{
			name: "media level connection info",
			session: &sdp.SessionDescription{
				MediaDescriptions: []*sdp.MediaDescription{
					{
						MediaName: sdp.MediaName{
							Media: "audio",
							Port:  sdp.RangedPort{Value: 1234},
						},
					},
				},
			},
			audio: &sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Media: "audio",
					Port:  sdp.RangedPort{Value: 1234},
				},
				ConnectionInformation: &sdp.ConnectionInformation{
					NetworkType: "IN",
					AddressType: "IP4",
					Address:     &sdp.Address{Address: "1.2.3.4"},
				},
			},
			expected: netip.MustParseAddrPort("1.2.3.4:1234"),
		},
		{
			name: "session level connection info",
			session: &sdp.SessionDescription{
				ConnectionInformation: &sdp.ConnectionInformation{
					NetworkType: "IN",
					AddressType: "IP4",
					Address:     &sdp.Address{Address: "1.2.3.4"},
				},
				MediaDescriptions: []*sdp.MediaDescription{
					{
						MediaName: sdp.MediaName{
							Media: "audio",
							Port:  sdp.RangedPort{Value: 1234},
						},
					},
				},
			},
			audio: &sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Media: "audio",
					Port:  sdp.RangedPort{Value: 1234},
				},
			},
			expected: netip.MustParseAddrPort("1.2.3.4:1234"),
		},
		{
			name:     "nil session",
			session:  nil,
			audio:    &sdp.MediaDescription{},
			expected: netip.AddrPort{},
			error:    true,
		},
		{
			name:     "nil audio",
			session:  &sdp.SessionDescription{},
			audio:    nil,
			expected: netip.AddrPort{},
			error:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := GetAudioDest(test.session, test.audio)
			if test.error {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, got)
		})
	}
}
