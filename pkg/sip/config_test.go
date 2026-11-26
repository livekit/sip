package sip

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/config"
)

func TestSelectLocalIP6(t *testing.T) {
	linkLocal := netip.MustParseAddr("fe80::1")
	loopback := netip.MustParseAddr("::1")
	unspecified := netip.MustParseAddr("::")
	multicast := netip.MustParseAddr("ff02::1")
	v4 := netip.MustParseAddr("192.0.2.10")
	v4Mapped := netip.MustParseAddr("::ffff:192.0.2.10")
	ula := netip.MustParseAddr("fd00::10")
	global1 := netip.MustParseAddr("2001:db8::10")
	global2 := netip.MustParseAddr("2001:db8:1::20")

	t.Run("skips non-global and prefers public over ULA", func(t *testing.T) {
		addrs := []netip.Addr{linkLocal, loopback, unspecified, multicast, v4, v4Mapped, ula, global1}
		got, ok := selectLocalIP6(addrs, nil, false)
		require.True(t, ok)
		require.Equal(t, global1, got)
	})

	t.Run("falls back to ULA when publicOnly is false", func(t *testing.T) {
		addrs := []netip.Addr{linkLocal, loopback, ula}
		got, ok := selectLocalIP6(addrs, nil, false)
		require.True(t, ok)
		require.Equal(t, ula, got)
	})

	t.Run("skips ULA when publicOnly is true", func(t *testing.T) {
		addrs := []netip.Addr{linkLocal, loopback, ula}
		_, ok := selectLocalIP6(addrs, nil, true)
		require.False(t, ok)
	})

	t.Run("filters by IPv6 LocalNet prefix", func(t *testing.T) {
		ulaPrefix := netip.MustParsePrefix("fd00::/8")
		addrs := []netip.Addr{global1, ula, global2}
		got, ok := selectLocalIP6(addrs, &ulaPrefix, false)
		require.True(t, ok)
		require.Equal(t, ula, got)

		missingPrefix := netip.MustParsePrefix("2001:db8:ffff::/48")
		_, ok = selectLocalIP6(addrs, &missingPrefix, false)
		require.False(t, ok)
	})
}

func TestListenIPv6(t *testing.T) {
	require.False(t, listenIPv6(nil).IsValid())
	require.False(t, listenIPv6(&config.Config{}).IsValid())
	require.False(t, listenIPv6(&config.Config{ListenIP: "0.0.0.0"}).IsValid())
	require.False(t, listenIPv6(&config.Config{ListenIP: "::"}).IsValid())
	require.False(t, listenIPv6(&config.Config{ListenIP: "::ffff:127.0.0.1"}).IsValid())
	require.False(t, listenIPv6(&config.Config{ListenIP: "invalid"}).IsValid())
	require.False(t, listenIPv6(&config.Config{ListenIP: "2001:db8::5", UseExternalIP: true}).IsValid())

	require.Equal(t, netip.MustParseAddr("::1"), listenIPv6(&config.Config{ListenIP: "::1"}))
	require.Equal(t, netip.MustParseAddr("2001:db8::5"), listenIPv6(&config.Config{ListenIP: "2001:db8::5"}))
}

func TestGetServiceConfig_IPv6Overrides(t *testing.T) {
	t.Run("NAT1To1IP IPv6 sets SignalingIP6 and MediaIP6", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{NAT1To1IP: "2001:db8::10"})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIP6)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIPLocal6)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.MediaIP6)
		require.True(t, s.SignalingIP.IsValid())
		require.Equal(t, s.SignalingIP, s.MediaIP)
	})

	t.Run("MediaNAT1To1IP IPv6 overrides MediaIP6 only when SignalingIP is IPv4", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{
			NAT1To1IP:      "203.0.113.10",
			MediaNAT1To1IP: "2001:db8::20",
		})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("203.0.113.10"), s.SignalingIP)
		require.Equal(t, netip.MustParseAddr("203.0.113.10"), s.MediaIP)
		require.Equal(t, netip.MustParseAddr("2001:db8::20"), s.MediaIP6)
	})

	t.Run("MediaNAT1To1IP IPv4 keeps SignalingIP6 as MediaIP6", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{
			NAT1To1IP:      "2001:db8::10",
			MediaNAT1To1IP: "203.0.113.20",
		})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIP6)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.MediaIP6)
		require.Equal(t, netip.MustParseAddr("203.0.113.20"), s.MediaIP)
	})

	t.Run("ListenIP IPv6 sets IPv6 defaults without NAT override", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{ListenIP: "::1"})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("::1"), s.SignalingIP6)
		require.Equal(t, netip.MustParseAddr("::1"), s.SignalingIPLocal6)
		require.Equal(t, netip.MustParseAddr("::1"), s.MediaIP6)
	})

	t.Run("unmatched or invalid LocalNet errors", func(t *testing.T) {
		_, err := GetServiceConfig(&config.Config{LocalNet: "invalid"})
		require.Error(t, err)

		_, err = GetServiceConfig(&config.Config{LocalNet: "2001:db8:ffff::/48"})
		require.Error(t, err)
	})
}
