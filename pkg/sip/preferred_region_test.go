// Copyright 2026 LiveKit, Inc.
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
)

func TestWithPreferredRegion(t *testing.T) {
	const wsURL = "wss://project.example.com"
	region := &livekit.RegionInfo{Region: "region-a", Url: "https://project.region-a.example.com"}

	cm := &callManager{}
	require.Nil(t, cm.withPreferredRegion(RoomConfig{WsUrl: wsURL}).PreferredRegion)

	var gotURL string
	WithPreferredRegion(func(u string) *livekit.RegionInfo { gotURL = u; return region })(cm)
	require.Same(t, region, cm.withPreferredRegion(RoomConfig{WsUrl: wsURL}).PreferredRegion)
	require.Equal(t, wsURL, gotURL)
}
