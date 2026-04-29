// Copyright 2026 LiveKit, Inc.
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
	"errors"
	"io"
	"net"
	"sync/atomic"
)

// srtpConn wraps the UDP conn passed to a pion SRTP session and translates
// net.ErrClosed from Read into io.EOF after Close has been called. pion/srtp's
// read goroutine logs every non-EOF read error at ERROR level (see pion/srtp/v3
// session.go), and the only way to wake that goroutine for shutdown is to close
// the conn, which produces net.ErrClosed on every normal call teardown.
// Translating to io.EOF lets pion's existing EOF filter swallow the message
// without affecting any other error path.
type srtpConn struct {
	net.Conn
	closing atomic.Bool
}

func (c *srtpConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && c.closing.Load() && errors.Is(err, net.ErrClosed) {
		return n, io.EOF
	}
	return n, err
}

func (c *srtpConn) Close() error {
	c.closing.Store(true)
	return c.Conn.Close()
}
