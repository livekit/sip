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
	"github.com/pkg/errors"
)

func GetServiceConfig(conf *config.Config) (*ServiceConfig, error) {
	s := new(ServiceConfig)
	var err error
	if conf.UseExternalIP {
		if s.SignalingIP, err = getPublicIP(); err != nil {
			return nil, err
		}
		if s.SignalingIPLocal, err = getLocalIP(conf.LocalNet); err != nil {
			return nil, err
		}
	} else if conf.NAT1To1IP != "" {
		ip, err := netip.ParseAddr(conf.NAT1To1IP)
		if err != nil {
			return nil, err
		}
		s.SignalingIP = ip
		s.SignalingIPLocal = s.SignalingIP
	} else {
		if s.SignalingIP, err = getLocalIP(conf.LocalNet); err != nil {
			return nil, err
		}
		s.SignalingIPLocal = s.SignalingIP
	}
	if conf.MediaUseExternalIP && !conf.UseExternalIP {
		if s.MediaIP, err = getPublicIP(); err != nil {
			return nil, err
		}
	} else if conf.MediaNAT1To1IP != "" && conf.MediaNAT1To1IP != conf.NAT1To1IP {
		ip, err := netip.ParseAddr(conf.MediaNAT1To1IP)
		if err != nil {
			return nil, err
		}
		s.MediaIP = ip
	} else {
		s.MediaIP = s.SignalingIP
	}
	return s, nil
}

func getPublicIP() (netip.Addr, error) {
	var err error
	for i := 0; i < 3; i++ {
		var ip string
		ip, err = rtcconfig.GetExternalIP(context.Background(), rtcconfig.DefaultStunServers, nil)
		if err == nil {
			return netip.ParseAddr(ip)
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	logger.Warnw("could not resolve external IP", err)
	return netip.Addr{}, errors.Errorf("could not resolve external IP: %v", err)
}

func getLocalIP(localNet string) (netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, err
	}
	var netw *netip.Prefix
	if localNet != "" {
		nw, err := netip.ParsePrefix(localNet)
		if err != nil {
			return netip.Addr{}, err
		}
		netw = &nw
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}

		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPAddr:
				if v.IP.To4() == nil {
					continue
				}
				ip, ok := netip.AddrFromSlice(v.IP.To4())
				if !ok || ip.IsLoopback() {
					continue
				}
				if netw != nil && !netw.Contains(ip) {
					continue
				}
				return ip, nil
			case *net.IPNet:
				if v.IP.To4() == nil {
					continue
				}
				ip, ok := netip.AddrFromSlice(v.IP.To4())
				if !ok || ip.IsLoopback() {
					continue
				}
				if netw != nil && !netw.Contains(ip) {
					continue
				}
				return ip, nil
			}

		}
	}

	return netip.Addr{}, fmt.Errorf("No local interface found")
}
