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
)

func getPublicIP() (string, error) {
	req, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return "", err
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return "", err
	}

	ip := struct {
		Query string
	}{}
	if err = json.Unmarshal(body, &ip); err != nil {
		return "", err
	}

	if ip.Query == "" {
		return "", fmt.Errorf("Query entry was not populated")
	}

	return ip.Query, nil
}

func getLocalIP(localNet string) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var netw *net.IPNet
	if localNet != "" {
		_, netw, err = net.ParseCIDR(localNet)
		if err != nil {
			return "", err
		}
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}

		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPAddr:
				if netw != nil && !netw.Contains(v.IP) {
					continue
				}
				if !v.IP.IsLoopback() && v.IP.To4() != nil {
					return v.IP.String(), nil
				}
			case *net.IPNet:
				if netw != nil && !netw.Contains(v.IP) {
					continue
				}
				if !v.IP.IsLoopback() && v.IP.To4() != nil {
					return v.IP.String(), nil
				}
			}

		}
	}

	return "", fmt.Errorf("No local interface found")
}
