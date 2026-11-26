package sip

import (
	"errors"
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
	t.Run("NAT1To1IP IPv6 sets SignalingIP6 and MediaIP6 while keeping outbound defaults", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{NAT1To1IP: "2001:db8::10"})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIP)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIPLocal)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIP6)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIPLocal6)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.MediaIP)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.MediaIP6)
	})

	t.Run("MediaNAT1To1IP IPv6 preserves MediaIP and sets MediaIP6 while keeping IPv4 inbound fallback", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{
			NAT1To1IP:      "203.0.113.10",
			MediaNAT1To1IP: "2001:db8::20",
		})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("203.0.113.10"), s.SignalingIP)
		require.Equal(t, netip.MustParseAddr("203.0.113.10"), s.SignalingIP4)
		require.Equal(t, netip.MustParseAddr("2001:db8::20"), s.MediaIP)
		require.Equal(t, netip.MustParseAddr("203.0.113.10"), s.MediaIP4)
		require.Equal(t, netip.MustParseAddr("2001:db8::20"), s.MediaIP6)
	})

	t.Run("MediaNAT1To1IP IPv4 keeps SignalingIP6 as MediaIP6", func(t *testing.T) {
		s, err := GetServiceConfig(&config.Config{
			NAT1To1IP:      "2001:db8::10",
			MediaNAT1To1IP: "203.0.113.20",
		})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIP)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.SignalingIP6)
		require.Equal(t, netip.MustParseAddr("2001:db8::10"), s.MediaIP6)
		require.Equal(t, netip.MustParseAddr("203.0.113.20"), s.MediaIP)
		require.Equal(t, netip.MustParseAddr("203.0.113.20"), s.MediaIP4)
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

func TestGetServiceConfig_ListenIPAndLocalNet(t *testing.T) {
	v4 := netip.MustParseAddr("10.0.0.5")
	mgmtV6 := netip.MustParseAddr("2001:db8:1::10")
	carrierV6 := netip.MustParseAddr("2001:db8:2::20")
	ulaV6 := netip.MustParseAddr("fd00::10")
	extV4 := netip.MustParseAddr("203.0.113.10")

	multihomed := func(bool) ([]netip.Addr, error) {
		return []netip.Addr{v4, mgmtV6, carrierV6, ulaV6}, nil
	}
	stubExt := func() (netip.Addr, error) {
		return extV4, nil
	}
	failExt := func() (netip.Addr, error) {
		return netip.Addr{}, errors.New("unexpected external IP lookup")
	}

	t.Run("concrete IPv6 ListenIP wins over global interface addresses", func(t *testing.T) {
		s, err := getServiceConfigWith(&config.Config{ListenIP: ulaV6.String()}, multihomed, failExt)
		require.NoError(t, err)
		require.Equal(t, ulaV6, s.SignalingIP6)
		require.Equal(t, ulaV6, s.SignalingIPLocal6)
		require.Equal(t, ulaV6, s.MediaIP6)
		require.Equal(t, v4, s.SignalingIP)
	})

	t.Run("concrete IPv6 ListenIP inside IPv6 LocalNet succeeds", func(t *testing.T) {
		s, err := getServiceConfigWith(&config.Config{
			ListenIP: ulaV6.String(),
			LocalNet: "fd00::/8",
		}, multihomed, failExt)
		require.NoError(t, err)
		require.Equal(t, ulaV6, s.SignalingIP6)
		require.Equal(t, ulaV6, s.SignalingIPLocal6)
	})

	t.Run("concrete IPv6 ListenIP outside IPv6 LocalNet errors", func(t *testing.T) {
		_, err := getServiceConfigWith(&config.Config{
			ListenIP: ulaV6.String(),
			LocalNet: "2001:db8:1::/48",
		}, multihomed, failExt)
		require.Error(t, err)
	})

	t.Run("UseExternalIP respects IPv6 LocalNet across multihomed interfaces", func(t *testing.T) {
		s, err := getServiceConfigWith(&config.Config{
			UseExternalIP: true,
			LocalNet:      "2001:db8:2::/48",
		}, multihomed, stubExt)
		require.NoError(t, err)
		require.Equal(t, extV4, s.SignalingIP)
		require.Equal(t, extV4, s.SignalingIP4)
		require.Equal(t, v4, s.SignalingIPLocal)
		require.Equal(t, carrierV6, s.SignalingIP6)
		require.Equal(t, carrierV6, s.SignalingIPLocal6)
		require.Equal(t, carrierV6, s.MediaIP6)
	})

	t.Run("MediaUseExternalIP respects IPv6 LocalNet across multihomed interfaces", func(t *testing.T) {
		s, err := getServiceConfigWith(&config.Config{
			MediaUseExternalIP: true,
			LocalNet:           "2001:db8:2::/48",
		}, multihomed, stubExt)
		require.NoError(t, err)
		require.Equal(t, extV4, s.MediaIP)
		require.Equal(t, extV4, s.MediaIP4)
		require.Equal(t, carrierV6, s.SignalingIP6)
		require.Equal(t, carrierV6, s.MediaIP6)
	})

	t.Run("UseExternalIP with ULA LocalNet does not leak other interface public IPv6", func(t *testing.T) {
		s, err := getServiceConfigWith(&config.Config{
			UseExternalIP: true,
			LocalNet:      "fd00::/8",
		}, multihomed, stubExt)
		require.NoError(t, err)
		require.Equal(t, ulaV6, s.SignalingIPLocal6)
		require.False(t, s.SignalingIP6.IsValid())
		require.False(t, s.MediaIP6.IsValid())
	})

	t.Run("dual stack IPv6 NAT overrides preserve both outbound IPv6 and inbound IPv4", func(t *testing.T) {
		natSig6 := netip.MustParseAddr("2001:db8:9::1")
		natMed6 := netip.MustParseAddr("2001:db8:9::2")
		s, err := getServiceConfigWith(&config.Config{
			NAT1To1IP:      natSig6.String(),
			MediaNAT1To1IP: natMed6.String(),
		}, multihomed, failExt)
		require.NoError(t, err)
		require.Equal(t, natSig6, s.SignalingIP)
		require.Equal(t, natSig6, s.SignalingIP6)
		require.Equal(t, v4, s.SignalingIP4)
		require.Equal(t, natMed6, s.MediaIP)
		require.Equal(t, natMed6, s.MediaIP6)
		require.Equal(t, v4, s.MediaIP4)
	})

	t.Run("concrete IPv4 ListenIP suppresses auto-detected IPv6 signaling while keeping IPv6 media", func(t *testing.T) {
		s, err := getServiceConfigWith(&config.Config{
			ListenIP: v4.String(),
		}, multihomed, failExt)
		require.NoError(t, err)
		require.Equal(t, v4, s.SignalingIP)
		require.Equal(t, v4, s.SignalingIP4)
		require.False(t, s.SignalingIP6.IsValid())
		require.False(t, s.SignalingIPLocal6.IsValid())
		require.Equal(t, mgmtV6, s.MediaIP6)

		sExt, err := getServiceConfigWith(&config.Config{
			ListenIP:      v4.String(),
			UseExternalIP: true,
		}, multihomed, stubExt)
		require.NoError(t, err)
		require.Equal(t, extV4, sExt.SignalingIP)
		require.False(t, sExt.SignalingIP6.IsValid())
		require.False(t, sExt.SignalingIPLocal6.IsValid())
		require.Equal(t, mgmtV6, sExt.MediaIP6)
	})
}
