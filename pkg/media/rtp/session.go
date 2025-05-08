// Copyright 2024 LiveKit, Inc.
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
	"fmt"
	"io"
	"net"
	"slices"
	"sync"

	"github.com/frostbyte73/core"
	"github.com/pion/rtp"

	"github.com/livekit/protocol/logger"
)

const (
	enableZeroCopy = true
	MTUSize        = 1500
)

type Session interface {
	OpenWriteStream() (WriteStream, error)
	AcceptStream() (ReadStream, uint32, error)
	Close() error
}

type WriteStream interface {
	String() string
	// WriteRTP writes RTP packet to the connection.
	WriteRTP(h *rtp.Header, payload []byte) (int, error)
}

type ReadStream interface {
	// ReadRTP reads RTP packet and its header from the connection.
	ReadRTP(h *rtp.Header, payload []byte) (int, error)
}

func NewSession(log logger.Logger, conn net.Conn) Session {
	return &session{
		log:    log,
		conn:   conn,
		w:      &writeStream{conn: conn},
		bySSRC: make(map[uint32]*readStream),
		rbuf:   make([]byte, MTUSize+1), // larger buffer to detect overflow
	}
}

type session struct {
	log    logger.Logger
	conn   net.Conn
	closed core.Fuse
	w      *writeStream

	rmu    sync.Mutex
	rbuf   []byte
	bySSRC map[uint32]*readStream
}

func (s *session) OpenWriteStream() (WriteStream, error) {
	return s.w, nil
}

func (s *session) AcceptStream() (ReadStream, uint32, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	overflow := false
	for {
		n, err := s.conn.Read(s.rbuf[:])
		if err != nil {
			return nil, 0, err
		}
		if n > MTUSize {
			overflow = true
			if !overflow {
				s.log.Errorw("RTP packet is larger than MTU limit", nil)
			}
			continue // ignore partial messages
		}
		buf := s.rbuf[:n]
		var p rtp.Packet
		err = p.Unmarshal(buf)
		if err != nil {
			continue // ignore
		}

		isNew := false
		r := s.bySSRC[p.SSRC]
		if r == nil {
			r = &readStream{
				ssrc:   p.SSRC,
				closed: s.closed.Watch(),
				copied: make(chan int),
				recv:   make(chan *rtp.Packet, 10),
			}
			s.bySSRC[p.SSRC] = r
			isNew = true
		}
		r.write(&p)
		if isNew {
			return r, r.ssrc, nil
		}
	}
}

func (s *session) Close() error {
	var err error
	s.closed.Once(func() {
		err = s.conn.Close()
		s.rmu.Lock()
		defer s.rmu.Unlock()
		s.bySSRC = nil
	})
	return err
}

type writeStream struct {
	mu   sync.Mutex
	buf  []byte
	conn net.Conn
}

func (w *writeStream) String() string {
	return fmt.Sprintf("RTPWriteStream(%s)", w.conn.RemoteAddr())
}

func (w *writeStream) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	hsz := h.MarshalSize()
	sz := hsz + len(payload)
	w.buf = w.buf[:0]
	w.buf = slices.Grow(w.buf, sz)
	buf := w.buf[:sz]
	n, err := h.MarshalTo(buf)
	if err != nil {
		return 0, err
	}
	copy(buf[n:], payload)
	return w.conn.Write(buf)
}

type readStream struct {
	ssrc uint32

	closed  <-chan struct{}
	recv    chan *rtp.Packet
	copied  chan int
	mu      sync.Mutex
	hdr     *rtp.Header
	payload []byte
}

func (r *readStream) write(p *rtp.Packet) {
	if enableZeroCopy {
		r.mu.Lock()
		h, payload := r.hdr, r.payload
		r.hdr, r.payload = nil, nil
		r.mu.Unlock()
		if h != nil {
			// zero copy
			*h = p.Header
			n := copy(payload, p.Payload)
			select {
			case <-r.closed:
			case r.copied <- n:
			}
			return
		}
	}
	p.Payload = slices.Clone(p.Payload)
	select {
	case r.recv <- p:
	default:
	}
}

func (r *readStream) ReadRTP(h *rtp.Header, payload []byte) (int, error) {
	direct := false
	if enableZeroCopy {
		r.mu.Lock()
		if r.hdr == nil {
			r.hdr = h
			r.payload = payload
			direct = true
		}
		r.mu.Unlock()
	}
	if !direct {
		select {
		case p := <-r.recv:
			*h = p.Header
			n := copy(payload, p.Payload)
			return n, nil
		case <-r.closed:
		}
		return 0, io.EOF
	}
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.hdr == h {
			r.hdr, r.payload = nil, nil
		}
	}()
	select {
	case n := <-r.copied:
		return n, nil
	case p := <-r.recv:
		*h = p.Header
		n := copy(payload, p.Payload)
		return n, nil
	case <-r.closed:
	}
	return 0, io.EOF
}
