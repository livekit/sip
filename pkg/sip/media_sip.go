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
	"net"
	"sync"
	"sync/atomic"

	"github.com/frostbyte73/core"

	"github.com/livekit/sip/pkg/media/rtp"
)

var _ rtp.Writer = (*MediaConn)(nil)

func NewMediaConn() *MediaConn {
	return &MediaConn{}
}

type MediaConn struct {
	wmu    sync.Mutex
	conn   *net.UDPConn
	closed core.Fuse

	dest  atomic.Pointer[net.UDPAddr]
	onRTP atomic.Pointer[rtp.Handler]
}

func (c *MediaConn) LocalAddr() *net.UDPAddr {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr().(*net.UDPAddr)
}

func (c *MediaConn) DestAddr() *net.UDPAddr {
	return c.dest.Load()
}

func (c *MediaConn) SetDestAddr(addr *net.UDPAddr) {
	c.dest.Store(addr)
}

func (c *MediaConn) OnRTP(h rtp.Handler) {
	if c == nil {
		return
	}
	if h == nil {
		c.onRTP.Store(nil)
	} else {
		c.onRTP.Store(&h)
	}
}

func (c *MediaConn) Close() error {
	if c == nil {
		return nil
	}
	c.closed.Once(func() {
		c.conn.Close()
	})
	return nil
}

func (c *MediaConn) Start(portMin, portMax int, listenAddr string) error {
	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}

	var err error
	c.conn, err = listenUDPInPortRange(portMin, portMax, net.ParseIP(listenAddr))

	if err != nil {
		return err
	}
	go c.readLoop()
	return nil
}

func (c *MediaConn) readLoop() {
	conn := c.conn
	buf := make([]byte, 1500) // MTU
	var p rtp.Packet
	for {
		n, srcAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		c.dest.Store(srcAddr)

		p = rtp.Packet{}
		if err := p.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if h := c.onRTP.Load(); h != nil {
			_ = (*h).HandleRTP(&p)
		}
	}
}

func (c *MediaConn) WriteRTP(p *rtp.Packet) error {
	addr := c.dest.Load()
	if addr == nil {
		return nil
	}
	data, err := p.Marshal()
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.WriteTo(data, addr)
	return err
}
