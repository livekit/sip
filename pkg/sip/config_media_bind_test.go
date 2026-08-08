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
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/config"
)

func TestParseSpecificListenIP(t *testing.T) {
	t.Parallel()

	ip, ok := parseSpecificListenIP("127.0.0.1")
	require.True(t, ok)
	require.Equal(t, netip.MustParseAddr("127.0.0.1"), ip)

	_, ok = parseSpecificListenIP("")
	require.False(t, ok)
	_, ok = parseSpecificListenIP("0.0.0.0")
	require.False(t, ok)
	_, ok = parseSpecificListenIP("::")
	require.False(t, ok)
	_, ok = parseSpecificListenIP("not-an-ip")
	require.False(t, ok)
}

func TestIsLocalInterfaceIP(t *testing.T) {
	t.Parallel()

	require.True(t, isLocalInterfaceIP(netip.MustParseAddr("127.0.0.1")))
	require.False(t, isLocalInterfaceIP(netip.MustParseAddr("8.8.8.8")))
}

func TestResolveMediaBindIP(t *testing.T) {
	t.Parallel()

	loopback := netip.MustParseAddr("127.0.0.1")
	publicNAT := netip.MustParseAddr("203.0.113.10") // TEST-NET-3, not local

	t.Run("media_listen_ip wins", func(t *testing.T) {
		got := resolveMediaBindIP(&config.Config{
			MediaListenIP: "127.0.0.1",
			ListenIP:      "127.0.0.1",
		}, &ServiceConfig{SignalingIPLocal: publicNAT})
		require.Equal(t, loopback, got)
	})

	t.Run("listen_ip when media_listen_ip empty", func(t *testing.T) {
		got := resolveMediaBindIP(&config.Config{
			ListenIP: "127.0.0.1",
		}, &ServiceConfig{SignalingIPLocal: publicNAT})
		require.Equal(t, loopback, got)
	})

	t.Run("wildcard listen_ip ignored", func(t *testing.T) {
		got := resolveMediaBindIP(&config.Config{
			ListenIP: "0.0.0.0",
		}, &ServiceConfig{SignalingIPLocal: publicNAT})
		require.False(t, got.IsValid(), "classic nat_1_to_1 public IP must not become BindIP")
	})

	t.Run("local SignalingIPLocal used", func(t *testing.T) {
		got := resolveMediaBindIP(&config.Config{}, &ServiceConfig{SignalingIPLocal: loopback})
		require.Equal(t, loopback, got)
	})

	t.Run("non-local SignalingIPLocal falls back", func(t *testing.T) {
		got := resolveMediaBindIP(&config.Config{
			NAT1To1IP: publicNAT.String(),
		}, &ServiceConfig{
			SignalingIP:      publicNAT,
			SignalingIPLocal: publicNAT,
		})
		require.False(t, got.IsValid())
	})
}
