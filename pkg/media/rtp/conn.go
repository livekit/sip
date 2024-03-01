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
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/rtp"
)

var _ Writer = (*Conn)(nil)

const (
	timeoutCheckInterval = time.Second * 30
)

func NewConn(timeoutCallback func()) *Conn {
	c := &Conn{
		readBuf: make([]byte, 1500), // MTU
	}
	if timeoutCallback != nil {
		c.onTimeout(timeoutCallback)
	}
	return c
}

type Conn struct {
	wmu         sync.Mutex
	conn        *net.UDPConn
	closed      core.Fuse
	readBuf     []byte
	packetCount atomic.Uint64

	dest  atomic.Pointer[net.UDPAddr]
	onRTP atomic.Pointer[Handler]
}

func (c *Conn) LocalAddr() *net.UDPAddr {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr().(*net.UDPAddr)
}

func (c *Conn) DestAddr() *net.UDPAddr {
	return c.dest.Load()
}

func (c *Conn) SetDestAddr(addr *net.UDPAddr) {
	c.dest.Store(addr)
}

func (c *Conn) OnRTP(h Handler) {
	if c == nil {
		return
	}
	if h == nil {
		c.onRTP.Store(nil)
	} else {
		c.onRTP.Store(&h)
	}
}

func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
	c.closed.Once(func() {
		c.conn.Close()
	})
	return nil
}

func (c *Conn) Listen(portMin, portMax int, listenAddr string) error {
	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}

	var err error
	c.conn, err = ListenUDPPortRange(portMin, portMax, net.ParseIP(listenAddr))
	if err != nil {
		return err
	}
	return nil
}

func (c *Conn) ListenAndServe(portMin, portMax int, listenAddr string) error {
	if err := c.Listen(portMin, portMax, listenAddr); err != nil {
		return err
	}
	go c.readLoop()
	return nil
}

func (c *Conn) readLoop() {
	conn, buf := c.conn, c.readBuf
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

		c.packetCount.Add(1)
		if h := c.onRTP.Load(); h != nil {
			_ = (*h).HandleRTP(&p)
		}
	}
}

func (c *Conn) WriteRTP(p *rtp.Packet) error {
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

func (c *Conn) ReadRTP() (*rtp.Packet, *net.UDPAddr, error) {
	buf := c.readBuf
	n, addr, err := c.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, nil, err
	}
	var p rtp.Packet
	if err = p.Unmarshal(buf[:n]); err != nil {
		return nil, addr, err
	}
	return &p, addr, nil
}

func (c *Conn) onTimeout(timeoutCallback func()) {
	go func() {
		ticker := time.NewTicker(timeoutCheckInterval)
		defer ticker.Stop()

		var lastPacketCount uint64
		for {
			select {
			case <-c.closed.Watch():
				return
			case <-ticker.C:
				currentPacketCount := c.packetCount.Load()
				if currentPacketCount == lastPacketCount {
					timeoutCallback()
					return
				}

				lastPacketCount = currentPacketCount
			}
		}
	}()
}
