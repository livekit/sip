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
	"fmt"
	"math/rand/v2"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/livekit/sip/pkg/media"
)

type BytesFrame interface {
	~[]byte
	media.Frame
}

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

// Buffer is a Writer that clones and appends RTP packets into a slice.
type Buffer []*Packet

func (b *Buffer) WriteRTP(p *Packet) error {
	p2 := p.Clone()
	*b = append(*b, p2)
	return nil
}

// NewSeqWriter creates an RTP writer that automatically increments the sequence number.
func NewSeqWriter(w Writer) *SeqWriter {
	s := &SeqWriter{w: w}
	s.p = rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SSRC:           rand.Uint32(),
			SequenceNumber: 0,
		},
	}
	return s
}

type Packet = rtp.Packet

type Event struct {
	Type      byte
	Timestamp uint32
	Payload   []byte
	Marker    bool
}

type SeqWriter struct {
	mu sync.Mutex
	w  Writer
	p  Packet
}

func (s *SeqWriter) WriteEvent(ev *Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p.PayloadType = ev.Type
	s.p.Payload = ev.Payload
	s.p.Marker = ev.Marker
	s.p.Timestamp = ev.Timestamp
	if err := s.w.WriteRTP(&s.p); err != nil {
		return err
	}
	s.p.Header.SequenceNumber++
	return nil
}

// NewStream creates a new media stream in RTP and tracks timestamps associated with it.
func (s *SeqWriter) NewStream(typ byte, clockRate int) *Stream {
	return s.NewStreamWithDur(typ, uint32(clockRate/DefFramesPerSec))
}

func (s *SeqWriter) NewStreamWithDur(typ byte, packetDur uint32) *Stream {
	st := &Stream{s: s, packetDur: packetDur}
	st.ev.Type = typ
	return st
}

type Stream struct {
	s         *SeqWriter
	packetDur uint32
	mu        sync.Mutex
	ev        Event
}

func (s *Stream) writePayload(inc bool, data []byte, marker bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ev.Payload = data
	s.ev.Marker = marker
	if err := s.s.WriteEvent(&s.ev); err != nil {
		return err
	}
	if inc {
		s.ev.Timestamp += s.packetDur
	}
	return nil
}

// WritePayload writes the payload to RTP and increments the timestamp.
func (s *Stream) WritePayload(data []byte, marker bool) error {
	return s.writePayload(true, data, marker)
}

// WritePayloadAtCurrent writes the payload to RTP at the current timestamp.
func (s *Stream) WritePayloadAtCurrent(data []byte, marker bool) error {
	return s.writePayload(false, data, marker)
}

func (s *Stream) Delay(dur uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ev.Timestamp += dur
}

func (s *Stream) ResetTimestamp(ts uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ev.Timestamp = ts
}

func (s *Stream) GetCurrentTimestamp() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ev.Timestamp
}

func NewMediaStreamOut[T BytesFrame](s *Stream, sampleRate int) *MediaStreamOut[T] {
	return &MediaStreamOut[T]{s: s, sampleRate: sampleRate}
}

type MediaStreamOut[T BytesFrame] struct {
	s          *Stream
	sampleRate int
}

func (s *MediaStreamOut[T]) String() string {
	return fmt.Sprintf("RTP(%d)", s.sampleRate)
}

func (s *MediaStreamOut[T]) SampleRate() int {
	return s.sampleRate
}

func (s *MediaStreamOut[T]) Close() error {
	return nil
}

func (s *MediaStreamOut[T]) WriteSample(sample T) error {
	return s.s.WritePayload([]byte(sample), false)
}

func NewMediaStreamIn[T BytesFrame](w media.Writer[T]) *MediaStreamIn[T] {
	return &MediaStreamIn[T]{Writer: w}
}

type MediaStreamIn[T BytesFrame] struct {
	Writer media.Writer[T]
}

func (s *MediaStreamIn[T]) String() string {
	return fmt.Sprintf("RTP(%d) -> %s", s.Writer.SampleRate(), s.Writer)
}

func (s *MediaStreamIn[T]) HandleRTP(p *rtp.Packet) error {
	return s.Writer.WriteSample(T(p.Payload))
}
