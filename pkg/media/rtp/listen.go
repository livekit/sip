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

package rtp

import (
	"errors"
	"math/rand"
	"net"
)

var ListenErr = errors.New("failed to listen on udp port")

func ListenUDPPortRange(portMin, portMax int, IP net.IP) (*net.UDPConn, error) {
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
		return nil, ListenErr
	}

	portStart := rand.Intn(portMax-portMin+1) + portMin
	portCurrent := portStart

	for {
		c, e := net.ListenUDP("udp", &net.UDPAddr{IP: IP, Port: portCurrent})
		if e == nil {
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
	return nil, ListenErr
}
