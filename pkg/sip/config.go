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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
)

func getPublicIP() (netip.Addr, error) {
	req, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return netip.Addr{}, err
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return netip.Addr{}, err
	}

	ip := struct {
		Query string
	}{}
	if err = json.Unmarshal(body, &ip); err != nil {
		return netip.Addr{}, err
	}

	if ip.Query == "" {
		return netip.Addr{}, fmt.Errorf("Query entry was not populated")
	}

	return netip.ParseAddr(ip.Query)
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
