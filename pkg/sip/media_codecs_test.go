// Copyright 2024 LiveKit, Inc.
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

//go:build cgo

package sip

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestResolveSDPName(t *testing.T) {
	t.Run("two-part name resolves to three-part", func(t *testing.T) {
		got := resolveSDPName("opus/48000")
		require.Equal(t, OpusSDPName, got)
	})
	t.Run("exact three-part match returns empty", func(t *testing.T) {
		got := resolveSDPName("opus/48000/2")
		require.Empty(t, got)
	})
	t.Run("unknown codec returns empty", func(t *testing.T) {
		got := resolveSDPName("unknown/8000")
		require.Empty(t, got)
	})
}

func TestCodecSetWithOpus(t *testing.T) {
	SetOpusEnabled(true)
	t.Cleanup(func() { SetOpusEnabled(false) })

	m := &livekit.SIPMediaConfig{
		OnlyListedCodecs: true,
		Codecs: []*livekit.SIPCodec{
			{Name: "opus", Rate: 48000},
		},
	}
	s, err := codecSet(m)
	require.NoError(t, err)
	require.True(t, s.IsEnabledByName(OpusSDPName),
		"codecSet should enable opus/48000/2 when opus/48000 is listed")
}
