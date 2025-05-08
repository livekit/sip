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

	"github.com/livekit/protocol/logger"
)

var _ Writer = (*Conn)(nil)

type ConnConfig struct {
	Log                 logger.Logger
	MediaTimeoutInitial time.Duration
	MediaTimeout        time.Duration
	TimeoutCallback     func()
}

func NewConn(conf *ConnConfig) *Conn {
	return NewConnWith(nil, conf)
}

func NewConnWith(conn UDPConn, conf *ConnConfig) *Conn {
	if conf == nil {
		conf = &ConnConfig{}
	}
	if conf.MediaTimeoutInitial <= 0 {
		conf.MediaTimeoutInitial = 30 * time.Second
	}
	if conf.MediaTimeout <= 0 {
		conf.MediaTimeout = 15 * time.Second
	}
	if conf.Log == nil {
		conf.Log = logger.GetLogger()
	}
	c := &Conn{
		log:            conf.Log,
		readBuf:        make([]byte, 1500), // MTU
		received:       make(chan struct{}),
		conn:           conn,
		timeout:        conf.MediaTimeout,
		timeoutInitial: conf.MediaTimeoutInitial,
		timeoutReset:   make(chan struct{}, 1),
	}
	if conf.TimeoutCallback != nil {
		c.EnableTimeout(true)
		go c.onTimeout(conf.TimeoutCallback)
	}
	return c
}

type UDPConn interface {
	LocalAddr() net.Addr
	WriteToUDP(b []byte, addr *net.UDPAddr) (int, error)
	ReadFromUDP(b []byte) (n int, addr *net.UDPAddr, err error)
	Close() error
}

type Conn struct {
	log            logger.Logger
	wmu            sync.Mutex
	conn           UDPConn
	closed         core.Fuse
	readBuf        []byte
	packetCount    atomic.Uint64
	received       chan struct{}
	timeoutStart   atomic.Pointer[time.Time]
	timeoutReset   chan struct{}
	timeoutInitial time.Duration
	timeout        time.Duration

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

// Received chan is closed once Conn receives at least one RTP packet.
func (c *Conn) Received() <-chan struct{} {
	return c.received
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
	c.OnRTP(nil)
	c.closed.Once(func() {
		c.conn.Close()
	})
	return nil
}

func (c *Conn) Listen(portMin, portMax int, listenAddr string) error {
	if c.conn != nil {
		return nil
	}
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

		if c.packetCount.Add(1) == 1 { // first packet
			close(c.received)
		}
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
	_, err = c.conn.WriteToUDP(data, addr)
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

func (c *Conn) EnableTimeout(enabled bool) {
	if !enabled {
		c.timeoutStart.Store(nil)
		return
	}
	select {
	case c.timeoutReset <- struct{}{}:
	default:
	}
	now := time.Now()
	c.timeoutStart.Store(&now)
	c.log.Infow("media timeout enabled",
		"packets", c.packetCount.Load(),
	)
}

func (c *Conn) onTimeout(timeoutCallback func()) {
	tickInterval := c.timeout
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	var (
		lastPackets  uint64
		startPackets uint64
		lastTime     time.Time
	)
	for {
		select {
		case <-c.closed.Watch():
			return
		case <-c.timeoutReset:
			ticker.Reset(tickInterval)
			startPackets = c.packetCount.Load()
			lastTime = time.Now()
		case <-ticker.C:
			curPackets := c.packetCount.Load()
			if curPackets != lastPackets {
				lastPackets = curPackets
				lastTime = time.Now()
				continue // wait for the next tick
			}
			startPtr := c.timeoutStart.Load()
			if startPtr == nil {
				continue // timeout disabled
			}

			// First timeout is allowed to be longer. Skip ticks if it's too early.
			sinceStart := time.Since(*startPtr)
			if lastPackets == startPackets && sinceStart < c.timeoutInitial {
				continue
			}

			// Ticker is allowed to fire earlier than the full timeout interval. Skip if it's not a full timeout yet.
			sinceLast := time.Since(lastTime)
			if sinceLast < c.timeout {
				continue
			}
			c.log.Infow("triggering media timeout",
				"packets", lastPackets,
				"startPackets", startPackets,
				"sinceStart", sinceStart,
				"sinceLast", sinceLast,
				"initial", c.timeoutInitial,
				"timeout", c.timeout,
			)
			timeoutCallback()
			return
		}
	}
}
