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
	"sync"
)

var ErrListen = errors.New("failed to listen on udp port")

// portRangeKey 将最小最大端口转换为单个uint32键值
// 因为端口是uint16，所以高16位存储最小端口，低16位存储最大端口
func portRangeKey(min, max int) uint32 {
	return (uint32(min&0xFFFF) << 16) | uint32(max&0xFFFF)
}

// 使用map存储不同端口范围的上次分配端口
var lastAllocatedPorts sync.Map // key: uint32, value: uint16

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
		j = 0xFFFF // 65535
	}

	if i > j {
		return nil, ErrListen
	}

	// 生成当前端口范围的键
	rangeKey := portRangeKey(i, j)

	// 尝试获取此范围的上次分配端口
	var portStart int
	if lastPort, ok := lastAllocatedPorts.Load(rangeKey); ok {
		// 如果找到了这个范围的记录，使用上次端口+1
		lastPortVal := lastPort.(int)
		if lastPortVal >= i && lastPortVal < j {
			portStart = lastPortVal + 1
		} else {
			portStart = rand.Intn(j-i+1) + i
		}
	} else {
		// 如果没有这个范围的记录，随机选择一个端口号
		portStart = rand.Intn(j-i+1) + i
	}

	portCurrent := portStart

	// 循环尝试选择一个端口号
	for {
		c, e := net.ListenUDP("udp", &net.UDPAddr{IP: IP, Port: portCurrent})
		if e == nil {
			// 更新此范围的上次成功分配端口
			lastAllocatedPorts.Store(rangeKey, portCurrent)
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
	return nil, ErrListen
}
