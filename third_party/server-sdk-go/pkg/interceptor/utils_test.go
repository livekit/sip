// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This code copy from github.com/pion/interceptor/internal/test, as it is not exported

package interceptor

import (
	"errors"
	"io"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// MockStream is a helper struct for testing interceptors.
type MockStream struct {
	interceptor interceptor.Interceptor

	rtcpReader interceptor.RTCPReader
	rtcpWriter interceptor.RTCPWriter
	rtpReader  interceptor.RTPReader
	rtpWriter  interceptor.RTPWriter

	rtcpIn chan []rtcp.Packet
	rtpIn  chan *rtp.Packet

	rtcpOutModified chan []rtcp.Packet
	rtpOutModified  chan *rtp.Packet

	rtcpInModified chan RTCPWithError
	rtpInModified  chan RTPWithError
}

// RTPWithError is used to send an rtp packet or an error on a channel
type RTPWithError struct {
	Packet *rtp.Packet
	Err    error
}

// RTCPWithError is used to send a batch of rtcp packets or an error on a channel
type RTCPWithError struct {
	Packets []rtcp.Packet
	Err     error
}

// NewMockStream creates a new MockStream
func NewMockStream(info *interceptor.StreamInfo, i interceptor.Interceptor) *MockStream { // nolint
	s := &MockStream{
		interceptor:     i,
		rtcpIn:          make(chan []rtcp.Packet, 1000),
		rtpIn:           make(chan *rtp.Packet, 1000),
		rtcpOutModified: make(chan []rtcp.Packet, 1000),
		rtpOutModified:  make(chan *rtp.Packet, 1000),
		rtcpInModified:  make(chan RTCPWithError, 1000),
		rtpInModified:   make(chan RTPWithError, 1000),
	}
	s.rtcpWriter = i.BindRTCPWriter(interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		select {
		case s.rtcpOutModified <- pkts:
		default:
		}

		return 0, nil
	}))
	s.rtcpReader = i.BindRTCPReader(interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		pkts, ok := <-s.rtcpIn
		if !ok {
			return 0, nil, io.EOF
		}

		marshaled, err := rtcp.Marshal(pkts)
		if err != nil {
			return 0, nil, io.EOF
		} else if len(marshaled) > len(b) {
			return 0, nil, io.ErrShortBuffer
		}

		copy(b, marshaled)
		return len(marshaled), a, err
	}))
	s.rtpWriter = i.BindLocalStream(info, interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		select {
		case s.rtpOutModified <- &rtp.Packet{Header: *header, Payload: payload}:
		default:
		}

		return 0, nil
	}))
	s.rtpReader = i.BindRemoteStream(info, interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		p, ok := <-s.rtpIn
		if !ok {
			return 0, nil, io.EOF
		}

		marshaled, err := p.Marshal()
		if err != nil {
			return 0, nil, io.EOF
		} else if len(marshaled) > len(b) {
			return 0, nil, io.ErrShortBuffer
		}

		copy(b, marshaled)
		return len(marshaled), a, err
	}))

	go func() {
		buf := make([]byte, 1500)
		for {
			i, _, err := s.rtcpReader.Read(buf, interceptor.Attributes{})
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.rtcpInModified <- RTCPWithError{Err: err}
				}
				return
			}

			pkts, err := rtcp.Unmarshal(buf[:i])
			if err != nil {
				s.rtcpInModified <- RTCPWithError{Err: err}
				return
			}

			s.rtcpInModified <- RTCPWithError{Packets: pkts}
		}
	}()
	go func() {
		buf := make([]byte, 1500)
		for {
			i, _, err := s.rtpReader.Read(buf, interceptor.Attributes{})
			if err != nil {
				if errors.Is(err, io.EOF) {
					s.rtpInModified <- RTPWithError{Err: err}
				}
				return
			}

			p := &rtp.Packet{}
			if err := p.Unmarshal(buf[:i]); err != nil {
				s.rtpInModified <- RTPWithError{Err: err}
				return
			}

			s.rtpInModified <- RTPWithError{Packet: p}
		}
	}()

	return s
}

// WriteRTCP writes a batch of rtcp packet to the stream, using the interceptor
func (s *MockStream) WriteRTCP(pkts []rtcp.Packet) error {
	_, err := s.rtcpWriter.Write(pkts, interceptor.Attributes{})
	return err
}

// WriteRTP writes an rtp packet to the stream, using the interceptor
func (s *MockStream) WriteRTP(p *rtp.Packet) error {
	_, err := s.rtpWriter.Write(&p.Header, p.Payload, interceptor.Attributes{})
	return err
}

// ReceiveRTCP schedules a new rtcp batch, so it can be read be the stream
func (s *MockStream) ReceiveRTCP(pkts []rtcp.Packet) {
	s.rtcpIn <- pkts
}

// ReceiveRTP schedules a rtp packet, so it can be read be the stream
func (s *MockStream) ReceiveRTP(packet *rtp.Packet) {
	s.rtpIn <- packet
}

// WrittenRTCP returns a channel containing the rtcp batches written, modified by the interceptor
func (s *MockStream) WrittenRTCP() chan []rtcp.Packet {
	return s.rtcpOutModified
}

// WrittenRTP returns a channel containing rtp packets written, modified by the interceptor
func (s *MockStream) WrittenRTP() chan *rtp.Packet {
	return s.rtpOutModified
}

// ReadRTCP returns a channel containing the rtcp batched read, modified by the interceptor
func (s *MockStream) ReadRTCP() chan RTCPWithError {
	return s.rtcpInModified
}

// ReadRTP returns a channel containing the rtp packets read, modified by the interceptor
func (s *MockStream) ReadRTP() chan RTPWithError {
	return s.rtpInModified
}

// Close closes the stream and the underlying interceptor
func (s *MockStream) Close() error {
	close(s.rtcpIn)
	close(s.rtpIn)
	return s.interceptor.Close()
}
