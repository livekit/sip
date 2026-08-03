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

package sip

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sip/pkg/stats"
)

func TestProviderLabel(t *testing.T) {
	cases := []struct {
		name string
		info *livekit.ProviderInfo
		exp  string
	}{
		{
			name: "nil",
			info: nil,
			exp:  stats.ProviderUnknown,
		},
		{
			name: "internal",
			info: &livekit.ProviderInfo{Name: "someCarrier", Type: livekit.ProviderType_PROVIDER_TYPE_INTERNAL},
			exp:  "internal/somecarrier",
		},
		{
			name: "internal without a name",
			info: &livekit.ProviderInfo{Type: livekit.ProviderType_PROVIDER_TYPE_INTERNAL},
			exp:  "internal/unknown",
		},
		{
			name: "external",
			info: &livekit.ProviderInfo{
				Id:   "ST_customerTrunk",
				Name: "Some Customer's Twilio Trunk",
				Type: livekit.ProviderType_PROVIDER_TYPE_EXTERNAL,
			},
			exp: "external",
		},
		{
			name: "external without a name",
			info: &livekit.ProviderInfo{Type: livekit.ProviderType_PROVIDER_TYPE_EXTERNAL},
			exp:  "external",
		},
		{
			name: "unknown type",
			info: &livekit.ProviderInfo{Name: "someCarrier"},
			exp:  stats.ProviderUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.exp, providerLabel(c.info))
		})
	}
}
