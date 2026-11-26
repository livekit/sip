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
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/config"
)

func GetServiceConfig(conf *config.Config) (*ServiceConfig, error) {
	return getServiceConfigWith(conf, localInterfaceAddrs, getPublicIP)
}

func getServiceConfigWith(
	conf *config.Config,
	getAddrs func(bool) ([]netip.Addr, error),
	getExtIP func() (netip.Addr, error),
) (*ServiceConfig, error) {
	s := new(ServiceConfig)
	localNet4, localNet6, err := parseLocalNets(conf.LocalNet)
	if err != nil {
		return nil, err
	}

	resolveLocal4 := func() (netip.Addr, error) {
		return getLocalIPWith(localNet4, getAddrs)
	}

	ip4OnlyListener := listenIPv4Only(conf)
	var mediaFallback6 netip.Addr

	resolveLocal6 := func(publicOnly bool) (netip.Addr, error) {
		if !publicOnly {
			if l6 := listenIPv6(conf); l6.IsValid() {
				if localNet6 != nil && !localNet6.Contains(l6) {
					return netip.Addr{}, fmt.Errorf("listen IP %s is outside local_net %s", l6, localNet6)
				}
				return l6, nil
			}
		}
		addrs, err := getAddrs(true)
		if err != nil {
			return netip.Addr{}, err
		}
		ip6, _ := selectLocalIP6(addrs, localNet6, publicOnly)
		if !ip6.IsValid() && localNet6 != nil && !publicOnly {
			return netip.Addr{}, fmt.Errorf("no local interface found")
		}
		return ip6, nil
	}

	if conf.UseExternalIP {
		if s.SignalingIP, err = getExtIP(); err != nil {
			return nil, err
		}
		if s.SignalingIPLocal, err = resolveLocal4(); err != nil {
			return nil, err
		}
		s.SignalingIP4 = s.SignalingIP
		local6, err := resolveLocal6(false)
		if err != nil {
			return nil, err
		}
		pub6, _ := resolveLocal6(true)
		mediaFallback6 = pub6
		if !ip4OnlyListener {
			s.SignalingIPLocal6 = local6
			s.SignalingIP6 = pub6
		}
	} else if conf.NAT1To1IP != "" {
		ip, err := netip.ParseAddr(conf.NAT1To1IP)
		if err != nil {
			return nil, err
		}
		ip = ip.Unmap()
		s.SignalingIP = ip
		s.SignalingIPLocal = ip
		if ip.Is6() {
			s.SignalingIP6 = ip
			s.SignalingIPLocal6 = ip
			mediaFallback6 = ip
			if ip4, err4 := resolveLocal4(); err4 == nil {
				s.SignalingIP4 = ip4
			}
		} else {
			s.SignalingIP4 = ip
			ip6, err := resolveLocal6(false)
			if err != nil {
				return nil, err
			}
			mediaFallback6 = ip6
			if !ip4OnlyListener {
				s.SignalingIP6 = ip6
				s.SignalingIPLocal6 = ip6
			}
		}
	} else {
		ip4, err4 := resolveLocal4()
		ip6, err6 := resolveLocal6(false)
		if err6 != nil {
			return nil, err6
		}
		if err4 != nil {
			if !ip6.IsValid() || ip4OnlyListener {
				return nil, err4
			}
			s.SignalingIP = ip6
			s.SignalingIPLocal = ip6
		} else {
			s.SignalingIP = ip4
			s.SignalingIPLocal = ip4
			s.SignalingIP4 = ip4
		}
		mediaFallback6 = ip6
		if !ip4OnlyListener {
			s.SignalingIP6 = ip6
			s.SignalingIPLocal6 = ip6
		}
	}

	if conf.MediaUseExternalIP && !conf.UseExternalIP {
		if s.MediaIP, err = getExtIP(); err != nil {
			return nil, err
		}
		s.MediaIP4 = s.MediaIP
		if ip6, err := resolveLocal6(true); err == nil && ip6.IsValid() {
			s.MediaIP6 = ip6
		} else if mediaFallback6.IsValid() && !mediaFallback6.IsPrivate() && (localNet6 == nil || localNet6.Contains(mediaFallback6)) {
			s.MediaIP6 = mediaFallback6
		}
	} else if conf.MediaNAT1To1IP != "" && conf.MediaNAT1To1IP != conf.NAT1To1IP {
		ip, err := netip.ParseAddr(conf.MediaNAT1To1IP)
		if err != nil {
			return nil, err
		}
		ip = ip.Unmap()
		s.MediaIP = ip
		if ip.Is6() {
			s.MediaIP6 = ip
			if s.SignalingIP4.IsValid() {
				s.MediaIP4 = s.SignalingIP4
			}
		} else {
			s.MediaIP4 = ip
			s.MediaIP6 = mediaFallback6
		}
	} else {
		s.MediaIP = s.SignalingIP
		s.MediaIP4 = s.SignalingIP4
		s.MediaIP6 = mediaFallback6
	}

	return s, nil
}

func getPublicIP() (netip.Addr, error) {
	var err error
	for i := 0; i < 3; i++ {
		var ip string
		ip, err = rtcconfig.GetExternalIPv4(context.Background(), rtcconfig.DefaultStunServers, nil)
		if err == nil {
			return netip.ParseAddr(ip)
		} else {
			logger.Warnw("external IP lookup failed", err, "attempt", i+1, "stunServers", rtcconfig.DefaultStunServers)
			time.Sleep(500 * time.Millisecond)
		}
	}
	logger.Warnw("could not resolve external IP", err)
	return netip.Addr{}, fmt.Errorf("could not resolve external IP: %v", err)
}

func parseLocalNets(localNet string) (*netip.Prefix, *netip.Prefix, error) {
	if localNet == "" {
		return nil, nil, nil
	}
	p, err := netip.ParsePrefix(localNet)
	if err != nil {
		return nil, nil, err
	}
	if p.Addr().Is6() {
		return nil, &p, nil
	}
	return &p, nil, nil
}

func listenIPv6(conf *config.Config) netip.Addr {
	if conf == nil || conf.UseExternalIP || conf.ListenIP == "" {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(conf.ListenIP)
	if err != nil {
		return netip.Addr{}
	}
	ip = ip.Unmap()
	if !ip.Is6() || ip.IsUnspecified() {
		return netip.Addr{}
	}
	return ip
}

func listenIPv4Only(conf *config.Config) bool {
	if conf == nil || conf.ListenIP == "" {
		return false
	}
	ip, err := netip.ParseAddr(conf.ListenIP)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.Is4() && !ip.IsUnspecified()
}

func netAddrToAddr(a net.Addr) (netip.Addr, bool) {
	var raw net.IP
	switch v := a.(type) {
	case *net.IPAddr:
		raw = v.IP
	case *net.IPNet:
		raw = v.IP
	default:
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func localInterfaceAddrs(upOnly bool) ([]netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, i := range ifaces {
		if upOnly && i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ip, ok := netAddrToAddr(a); ok {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}

func selectLocalIP4(addrs []netip.Addr, netw *netip.Prefix) (netip.Addr, bool) {
	for _, ip := range addrs {
		ip = ip.Unmap()
		if !ip.Is4() || ip.IsLoopback() {
			continue
		}
		if netw != nil && !netw.Contains(ip) {
			continue
		}
		return ip, true
	}
	return netip.Addr{}, false
}

func selectLocalIP6(addrs []netip.Addr, netw *netip.Prefix, publicOnly bool) (netip.Addr, bool) {
	var ula netip.Addr
	for _, ip := range addrs {
		ip = ip.Unmap()
		if !ip.Is6() || !ip.IsGlobalUnicast() {
			continue
		}
		if netw != nil && !netw.Contains(ip) {
			continue
		}
		if ip.IsPrivate() {
			if !publicOnly && !ula.IsValid() {
				ula = ip
			}
			continue
		}
		return ip, true
	}
	if ula.IsValid() {
		return ula, true
	}
	return netip.Addr{}, false
}

func getLocalIPWith(netw *netip.Prefix, getAddrs func(bool) ([]netip.Addr, error)) (netip.Addr, error) {
	addrs, err := getAddrs(false)
	if err != nil {
		return netip.Addr{}, err
	}
	if ip, ok := selectLocalIP4(addrs, netw); ok {
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("no local interface found")
}
