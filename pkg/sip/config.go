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
	"math/rand"
	"net"
	"net/http"
	"time"

	"github.com/livekit/protocol/logger"
)

var source *rand.Rand

func init() {
	source = rand.New(rand.NewSource(time.Now().UnixNano()))
}

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

var listenErr = fmt.Errorf("Failed to listen on UDP Port")

func listenUDPInPortRange(portMin, portMax int, IP net.IP) (*net.UDPConn, error) {
	if portMin == 0 && portMax == 0 {
		return net.ListenUDP("udp", &net.UDPAddr{
			IP:   IP,
			Port: 0,
		})
	}

	i := portMin
	if i == 0 {
		i = 1
	}

	j := portMax
	if j == 0 {
		j = 0xFFFF
	}

	if i > j {
		return nil, listenErr
	}

	portStart := source.Intn(portMax-portMin+1) + portMin
	portCurrent := portStart

	for {
		c, e := net.ListenUDP("udp", &net.UDPAddr{IP: IP, Port: portCurrent})
		if e == nil {
			logger.Debugw("begin listening on UDP", "port", portCurrent)
			return c, nil
		}

		portCurrent++
		if portCurrent > j {
			portCurrent = i
		}
		if portCurrent == portStart {
			break
		}
	}
	return nil, listenErr
}
