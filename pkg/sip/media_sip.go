package sip

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/livekit/sip/pkg/media/rtp"
)

var _ rtp.Writer = (*MediaConn)(nil)

func NewMediaConn() *MediaConn {
	return &MediaConn{}
}

type MediaConn struct {
	wmu  sync.Mutex
	conn *net.UDPConn

	dest  atomic.Pointer[net.UDPAddr]
	onRTP rtp.Handler
}

func (c *MediaConn) LocalAddr() *net.UDPAddr {
	return c.conn.LocalAddr().(*net.UDPAddr)
}

func (c *MediaConn) DestAddr() *net.UDPAddr {
	return c.dest.Load()
}

func (c *MediaConn) SetDestAddr(addr *net.UDPAddr) {
	c.dest.Store(addr)
}

func (c *MediaConn) OnRTP(h rtp.Handler) {
	c.onRTP = h
}

func (c *MediaConn) Close() error {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	return nil
}

func (c *MediaConn) Start(listenAddr string) error {
	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}
	var err error
	c.conn, err = net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(listenAddr),
		Port: 0,
	})
	if err != nil {
		return err
	}
	go c.readLoop()
	return nil
}

func (c *MediaConn) readLoop() {
	buf := make([]byte, 1500) // MTU
	var p rtp.Packet
	for {
		n, srcAddr, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		c.dest.Store(srcAddr)

		p = rtp.Packet{}
		if err := p.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if c.onRTP != nil {
			_ = c.onRTP.HandleRTP(&p)
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
