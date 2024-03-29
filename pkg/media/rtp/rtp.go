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
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media"
)

type Writer interface {
	WriteRTP(p *rtp.Packet) error
}

type Reader interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
}

type Handler interface {
	HandleRTP(p *rtp.Packet) error
}

type HandlerFunc func(p *rtp.Packet) error

func (fnc HandlerFunc) HandleRTP(p *rtp.Packet) error {
	return fnc(p)
}

func HandleLoop(r Reader, h Handler) error {
	for {
		p, _, err := r.ReadRTP()
		if err != nil {
			return err
		}
		err = h.HandleRTP(p)
		if err != nil {
			return err
		}
	}
}

func NewStream(w Writer, packetDur uint32) *Stream {
	s := &Stream{w: w, packetDur: packetDur}
	s.p = rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SSRC:           5000, // TODO: why this magic number?
			Timestamp:      0,
			SequenceNumber: 0,
		},
	}
	return s
}

type Packet = rtp.Packet

type Stream struct {
	mu        sync.Mutex
	w         Writer
	p         Packet
	packetDur uint32
}

func (s *Stream) WritePayload(typ byte, data []byte) error {
	return s.WritePayloadMarker(typ, false, data)
}

func (s *Stream) WritePayloadMarker(typ byte, mark bool, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p.PayloadType = typ
	s.p.Payload = data
	s.p.Marker = mark
	if err := s.w.WriteRTP(&s.p); err != nil {
		return err
	}
	s.p.Header.Timestamp += s.packetDur
	s.p.Header.SequenceNumber++
	return nil
}

func NewMediaStreamOut[T ~[]byte](w Writer, typ byte, packetDur uint32) *MediaStreamOut[T] {
	return NewMediaStreamFor[T](NewStream(w, packetDur), typ)
}

func NewMediaStreamFor[T ~[]byte](s *Stream, typ byte) *MediaStreamOut[T] {
	return &MediaStreamOut[T]{s: s, typ: typ}
}

type MediaStreamOut[T ~[]byte] struct {
	s   *Stream
	typ byte
}

func (s *MediaStreamOut[T]) WriteSample(sample T) error {
	return s.s.WritePayload(s.typ, []byte(sample))
}

func NewMediaStreamIn[T ~[]byte](w media.Writer[T]) *MediaStreamIn[T] {
	return &MediaStreamIn[T]{w: w}
}

type MediaStreamIn[T ~[]byte] struct {
	w media.Writer[T]
}

func (s *MediaStreamIn[T]) HandleRTP(p *rtp.Packet) error {
	return s.w.WriteSample(T(p.Payload))
}
