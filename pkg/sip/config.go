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
	s := new(ServiceConfig)
	localNet4, localNet6, err := parseLocalNets(conf.LocalNet)
	if err != nil {
		return nil, err
	}

	resolveLocal6 := func(publicOnly bool) (netip.Addr, error) {
		ip6, err := getLocalIP6(localNet6, publicOnly)
		if err != nil {
			return netip.Addr{}, err
		}
		if !ip6.IsValid() && localNet6 == nil && !publicOnly {
			ip6 = listenIPv6(conf)
		}
		if !ip6.IsValid() && localNet6 != nil && !publicOnly {
			return netip.Addr{}, fmt.Errorf("no local interface found")
		}
		return ip6, nil
	}

	if conf.UseExternalIP {
		if s.SignalingIP, err = getPublicIP(); err != nil {
			return nil, err
		}
		if s.SignalingIPLocal, err = getLocalIP(localNet4); err != nil {
			return nil, err
		}
		if s.SignalingIPLocal6, err = resolveLocal6(false); err != nil {
			return nil, err
		}
		s.SignalingIP6, _ = getLocalIP6(nil, true)
	} else if conf.NAT1To1IP != "" {
		ip, err := netip.ParseAddr(conf.NAT1To1IP)
		if err != nil {
			return nil, err
		}
		ip = ip.Unmap()
		if ip.Is6() {
			s.SignalingIP6 = ip
			s.SignalingIPLocal6 = ip
			if ip4, err4 := getLocalIP(localNet4); err4 == nil {
				s.SignalingIP = ip4
				s.SignalingIPLocal = ip4
			} else {
				s.SignalingIP = ip
				s.SignalingIPLocal = ip
			}
		} else {
			s.SignalingIP = ip
			s.SignalingIPLocal = ip
			if s.SignalingIP6, err = resolveLocal6(false); err != nil {
				return nil, err
			}
			s.SignalingIPLocal6 = s.SignalingIP6
		}
	} else {
		ip4, err4 := getLocalIP(localNet4)
		ip6, err6 := resolveLocal6(false)
		if err6 != nil {
			return nil, err6
		}
		if err4 != nil {
			if !ip6.IsValid() {
				return nil, err4
			}
			s.SignalingIP = ip6
			s.SignalingIPLocal = ip6
		} else {
			s.SignalingIP = ip4
			s.SignalingIPLocal = ip4
		}
		s.SignalingIP6 = ip6
		s.SignalingIPLocal6 = ip6
	}

	if conf.MediaUseExternalIP && !conf.UseExternalIP {
		if s.MediaIP, err = getPublicIP(); err != nil {
			return nil, err
		}
		if ip6, err := getLocalIP6(nil, true); err == nil && ip6.IsValid() {
			s.MediaIP6 = ip6
		} else if s.SignalingIP6.IsValid() && !s.SignalingIP6.IsPrivate() {
			s.MediaIP6 = s.SignalingIP6
		}
	} else if conf.MediaNAT1To1IP != "" && conf.MediaNAT1To1IP != conf.NAT1To1IP {
		ip, err := netip.ParseAddr(conf.MediaNAT1To1IP)
		if err != nil {
			return nil, err
		}
		ip = ip.Unmap()
		if ip.Is6() {
			s.MediaIP6 = ip
			if s.SignalingIP.Is4() {
				s.MediaIP = s.SignalingIP
			} else {
				s.MediaIP = ip
			}
		} else {
			s.MediaIP = ip
			s.MediaIP6 = s.SignalingIP6
		}
	} else {
		s.MediaIP = s.SignalingIP
		s.MediaIP6 = s.SignalingIP6
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

func parseLocalNets(localNet string) (string, *netip.Prefix, error) {
	if localNet == "" {
		return "", nil, nil
	}
	p, err := netip.ParsePrefix(localNet)
	if err != nil {
		return "", nil, err
	}
	if p.Addr().Is6() {
		return "", &p, nil
	}
	return localNet, nil, nil
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

func getLocalIP(localNet string) (netip.Addr, error) {
	var netw *netip.Prefix
	if localNet != "" {
		nw, err := netip.ParsePrefix(localNet)
		if err != nil {
			return netip.Addr{}, err
		}
		netw = &nw
	}
	addrs, err := localInterfaceAddrs(false)
	if err != nil {
		return netip.Addr{}, err
	}
	if ip, ok := selectLocalIP4(addrs, netw); ok {
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("no local interface found")
}

func getLocalIP6(netw *netip.Prefix, publicOnly bool) (netip.Addr, error) {
	addrs, err := localInterfaceAddrs(true)
	if err != nil {
		return netip.Addr{}, err
	}
	ip, _ := selectLocalIP6(addrs, netw, publicOnly)
	return ip, nil
}
