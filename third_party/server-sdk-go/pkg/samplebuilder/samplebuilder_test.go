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

package samplebuilder

import (
	"reflect"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/require"
)

type testDepacketizer struct {
	headBytes []byte
}

func (d *testDepacketizer) Unmarshal(r []byte) ([]byte, error) {
	return r, nil
}

func (d *testDepacketizer) IsPartitionHead(payload []byte) bool {
	if d.headBytes == nil || len(payload) < len(d.headBytes) {
		return false
	}
	for i, b := range d.headBytes {
		if payload[i] != b {
			return false
		}
	}
	return true
}

func (d *testDepacketizer) IsPartitionTail(marker bool, payload []byte) bool {
	return marker
}

func TestSampleBuilder(t *testing.T) {
	t.Run("out of order packets", func(t *testing.T) {
		sb := New(10, &testDepacketizer{}, 30)
		sb.Push(testPacketWithSN(5))
		require.Nil(t, sb.Pop())
		sb.Push(testPacketWithSN(3))
		sb.Push(testPacketWithSN(1))
		require.Nil(t, sb.Pop())
		require.NoError(t, sb.check())
		sb.Push(testPacketWithSN(2))
		sb.Push(testPacketWithSN(4))
		require.NoError(t, sb.check())
		for i, pkt := range sb.PopPackets() {
			require.Equal(t, i+1, pkt.SequenceNumber)
		}
	})

	t.Run("does not pop missing packets", func(t *testing.T) {
		sb := New(10, &testDepacketizer{}, 30)
		sb.Push(testHeadPacketWithSN(1))
		sb.Push(testPacketWithSN(5))
		require.Nil(t, sb.Pop())
	})

	t.Run("assembles samples", func(t *testing.T) {
		sb := New(10, &testDepacketizer{
			headBytes: headerBytes,
		}, 30)

		// first sample
		sb.Push(testPacketWithSN(2))
		sb.Push(testHeadPacketWithSN(1))
		sb.Push(testTailPacketWithSN(3))

		// second sample
		sb.Push(testHeadPacketWithSN(4))
		sb.Push(testTailPacketWithSN(5))
		sb.Push(testHeadPacketWithSN(6))
		require.NoError(t, sb.check())

		sample := sb.Pop()
		require.NotNil(t, sample)
		require.Equal(t, defaultPacketSize*3, len(sample.Data))
		require.Equal(t, headerBytes[0], sample.Data[0])

		sample2 := sb.Pop()
		require.NotNil(t, sample2)
		require.Equal(t, defaultPacketSize*2, len(sample2.Data))
	})
}

const defaultPacketSize = 200

var headerBytes = []byte{0xaa, 0xaa}

func testPacketWithSN(sn uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: sn,
		},
		Payload: make([]byte, defaultPacketSize),
	}
}

func testHeadPacketWithSN(sn uint16) *rtp.Packet {
	p := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: sn,
		},
		Payload: make([]byte, defaultPacketSize),
	}
	copy(p.Payload, headerBytes)
	return p
}

func testTailPacketWithSN(sn uint16) *rtp.Packet {
	p := &rtp.Packet{
		Header: rtp.Header{
			Marker:         true,
			SequenceNumber: sn,
		},
		Payload: make([]byte, defaultPacketSize),
	}
	return p
}

// tests from pion/galene

type fakeDepacketizer struct {
	headChecker func([]byte) bool
	tailChecker func([]byte, bool) bool
}

func (f *fakeDepacketizer) Unmarshal(r []byte) ([]byte, error) {
	return r, nil
}

func (f *fakeDepacketizer) IsPartitionHead(payload []byte) bool {
	if f.headChecker != nil {
		return f.headChecker(payload)
	}
	return false
}

func (f *fakeDepacketizer) IsPartitionTail(marker bool, payload []byte) bool {
	if f.tailChecker != nil {
		return f.tailChecker(payload, marker)
	}
	return false
}

// for compatibility with Pion brain-damage
func (f *fakeDepacketizer) IsDetectedFinalPacketInSequence(rtpPacketMarketBit bool) bool {
	return rtpPacketMarketBit
}

type test struct {
	name        string
	maxLate     uint16
	headBytes   []byte
	tailChecker func([]byte, bool) bool
	packets     []*rtp.Packet
	samples     []*media.Sample
	timestamps  []uint32
}

// some tests stolen from Pion's samplebuilder
var tests = []test{
	{
		name: "One",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
		},
		samples:    []*media.Sample{},
		timestamps: []uint32{},
		maxLate:    50,
	},
	{
		name: "OnePartitionCheckerTrue",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
		},
		headBytes: []byte{0x01},
		tailChecker: func(payload []byte, marker bool) bool {
			return true
		},
		samples: []*media.Sample{
			{Data: []byte{0x01}},
		},
		timestamps: []uint32{5},
		maxLate:    50,
	},
	{
		name: "Sequential",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 7}, Payload: []byte{0x03}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x02}, Duration: time.Second},
		},
		timestamps: []uint32{
			6,
		},
		maxLate: 50,
	},
	{
		name: "Duplicate",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 6}, Payload: []byte{0x03}},
			{Header: rtp.Header{SequenceNumber: 5003, Timestamp: 7}, Payload: []byte{0x04}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x02, 0x03}, Duration: time.Second},
		},
		timestamps: []uint32{
			6,
		},
		maxLate: 50,
	},
	{
		name: "Gap",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5007, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5008, Timestamp: 7}, Payload: []byte{0x03}},
		},
		samples:    []*media.Sample{},
		timestamps: []uint32{},
		maxLate:    50,
	},
	{
		name: "GapPartitionHeadCheckerTrue",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5007, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5008, Timestamp: 7}, Payload: []byte{0x03}},
		},
		headBytes: []byte{0x02},
		samples: []*media.Sample{
			{Data: []byte{0x02}, Duration: time.Second},
		},
		timestamps: []uint32{
			6,
		},
		maxLate: 5,
	},
	{
		name: "GapPartitionHeadCheckerFalse",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5007, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5008, Timestamp: 7}, Payload: []byte{0x03}},
		},
		headBytes:  []byte{},
		samples:    []*media.Sample{},
		timestamps: []uint32{},
		maxLate:    5,
	},
	{
		name: "Multiple",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 1}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 2}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 3}, Payload: []byte{0x03}},
			{Header: rtp.Header{SequenceNumber: 5003, Timestamp: 4}, Payload: []byte{0x04}},
			{Header: rtp.Header{SequenceNumber: 5004, Timestamp: 5}, Payload: []byte{0x05}},
			{Header: rtp.Header{SequenceNumber: 5005, Timestamp: 6}, Payload: []byte{0x06}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x02}, Duration: time.Second},
			{Data: []byte{0x03}, Duration: time.Second},
			{Data: []byte{0x04}, Duration: time.Second},
			{Data: []byte{0x05}, Duration: time.Second},
		},
		timestamps: []uint32{
			2,
			3,
			4,
			5,
		},
		maxLate: 5,
	},
	{
		name: "MultipleDisordered",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 1}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5003, Timestamp: 4}, Payload: []byte{0x04}},
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 3}, Payload: []byte{0x03}},
			{Header: rtp.Header{SequenceNumber: 5004, Timestamp: 5}, Payload: []byte{0x05}},
			{Header: rtp.Header{SequenceNumber: 5005, Timestamp: 6}, Payload: []byte{0x06}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 2}, Payload: []byte{0x02}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x02}, Duration: time.Second},
			{Data: []byte{0x03}, Duration: time.Second},
			{Data: []byte{0x04}, Duration: time.Second},
			{Data: []byte{0x05}, Duration: time.Second},
		},
		timestamps: []uint32{
			2,
			3,
			4,
			5,
		},
		maxLate: 5,
	},
	{
		name: "MultipleDisordered2",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 3}, Payload: []byte{0x03}},
			{Header: rtp.Header{SequenceNumber: 5003, Timestamp: 4}, Payload: []byte{0x04}},
			{Header: rtp.Header{SequenceNumber: 5004, Timestamp: 5}, Payload: []byte{0x05}},
			{Header: rtp.Header{SequenceNumber: 5005, Timestamp: 6}, Payload: []byte{0x06}},
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 1}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 2}, Payload: []byte{0x02}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x02}, Duration: time.Second},
			{Data: []byte{0x03}, Duration: time.Second},
			{Data: []byte{0x04}, Duration: time.Second},
			{Data: []byte{0x05}, Duration: time.Second},
		},
		timestamps: []uint32{
			2,
			3,
			4,
			5,
		},
		maxLate: 8,
	},
	{
		name: "PartitionTailChecker",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 7}, Payload: []byte{0x03}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x02}, Duration: time.Second},
			{Data: []byte{0x03}, Duration: time.Second},
		},
		timestamps: []uint32{
			6, 7,
		},
		tailChecker: func(payload []byte, marker bool) bool {
			return true
		},
		maxLate: 50,
	},
	{
		name: "Checkers",
		packets: []*rtp.Packet{
			{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 5}, Payload: []byte{0x01}},
			{Header: rtp.Header{SequenceNumber: 5001, Timestamp: 6}, Payload: []byte{0x02}},
			{Header: rtp.Header{SequenceNumber: 5002, Timestamp: 7}, Payload: []byte{0x03}},
		},
		samples: []*media.Sample{
			{Data: []byte{0x01}, Duration: 0},
			{Data: []byte{0x02}, Duration: time.Second},
			{Data: []byte{0x03}, Duration: time.Second},
		},
		timestamps: []uint32{
			5, 6, 7,
		},
		headBytes: []byte{1},
		tailChecker: func(payload []byte, marker bool) bool {
			return true
		},
		maxLate: 50,
	},
}

func TestSamplebuilder(t *testing.T) {
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := New(test.maxLate, &fakeDepacketizer{
				headChecker: func(data []byte) bool {
					for _, b := range test.headBytes {
						if data[0] == b {
							return true
						}
					}
					return false
				},
				tailChecker: test.tailChecker,
			}, 1)
			samples := []*media.Sample{}
			timestamps := []uint32{}

			for _, p := range test.packets {
				s.Push(p)
				require.NoError(t, s.check())
			}
			for {
				sample, timestamp := s.ForcePopWithTimestamp()
				require.NoError(t, s.check())
				if sample == nil {
					break
				}
				samples = append(samples, sample)
				timestamps = append(timestamps, timestamp)
			}
			if !reflect.DeepEqual(samples, test.samples) {
				t.Errorf("got %#v, expected %#v",
					samples, test.samples,
				)
			}
			if !reflect.DeepEqual(timestamps, test.timestamps) {
				t.Errorf("got %v, expected %v",
					timestamps, test.timestamps,
				)
			}

		})
	}
}

func TestSampleBuilderSequential(t *testing.T) {
	s := New(10, &fakeDepacketizer{}, 1)
	j := 0
	for i := 0; i < 0x20000; i++ {
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i + 42),
			},
			Payload: []byte{byte(i)},
		}
		s.Push(&p)
		require.NoError(t, s.check())
		for {
			sample, ts := s.PopWithTimestamp()
			require.NoError(t, s.check())
			if sample == nil {
				break
			}
			if ts != uint32(j+43) {
				t.Errorf(
					"wrong timestamp (got %v, expected %v)",
					ts, uint32(j+43),
				)
			}
			if len(sample.Data) != 1 {
				t.Errorf(
					"bad data length (got %v, expected 1)",
					len(sample.Data),
				)
			}
			if sample.Data[0] != byte(j+1) {
				t.Errorf(
					"bad data (got %v, expected %v)",
					sample.Data[0], byte(j+1))
			}
			j++
		}
	}
	// only the first and last packet should be dropped
	if j != 0x1FFFE {
		t.Errorf("Got %v, expected %v", j, 0x1FFFE)
	}
}

func TestSampleBuilderLoss(t *testing.T) {
	s := New(10, &fakeDepacketizer{}, 1)
	j := 0
	for i := 0; i < 0x20000; i++ {
		if i%3 == 2 {
			continue
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i + 42),
			},
			Payload: []byte{byte(i)},
		}
		s.Push(&p)
		require.NoError(t, s.check())
		for {
			sample, ts := s.PopWithTimestamp()
			require.NoError(t, s.check())
			if sample == nil {
				break
			}
			if ts != uint32(j+43) {
				t.Errorf(
					"wrong timestamp (got %v, expected %v)",
					ts, uint32(j+43),
				)
			}
			if len(sample.Data) != 1 {
				t.Errorf(
					"bad data length (got %v, expected 1)",
					len(sample.Data),
				)
			}
			if sample.Data[0] != byte(j+1) {
				t.Errorf(
					"bad data (got %v, expected %v)",
					sample.Data[0], byte(j+1))
			}
			j++
		}
	}
	// since packets are discontigious and there's no partition
	// checker, all packets should be dropped.
	if j != 0 {
		t.Errorf("Got %v, expected %v", j, 0)
	}
}

func TestSampleBuilderLossChecker(t *testing.T) {
	s := New(10, &fakeDepacketizer{
		headChecker: func(_ []byte) bool { return true },
		tailChecker: func(_ []byte, _ bool) bool { return true },
	}, 1)
	j := 0
	for i := 0; i < 0x20000; i++ {
		if i%3 == 2 {
			continue
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i + 42),
			},
			Payload: []byte{byte(i)},
		}
		s.Push(&p)
		require.NoError(t, s.check())
		for {
			sample, ts := s.PopWithTimestamp()
			require.NoError(t, s.check())
			if sample == nil {
				break
			}
			k := j/2*3 + j%2
			if ts != uint32(k+42) {
				t.Errorf(
					"wrong timestamp (got %v, expected %v)",
					ts, uint32(k+44),
				)
			}
			if len(sample.Data) != 1 {
				t.Errorf(
					"bad data length (got %v, expected 1)",
					len(sample.Data),
				)
			}
			if sample.Data[0] != byte(k) {
				t.Errorf(
					"bad data (got %v, expected %v)",
					sample.Data[0], byte(k))
			}
			j++
		}
	}
	// since packets are discontigious and there's no partition
	// checker, all packets should be dropped.
	if j != 0x1FFFE/3*2-4 {
		t.Errorf("Got %v, expected %v", j, 0x1FFFE/3*2-4)
	}
}

func TestSampleBuilderDisordered(t *testing.T) {
	s := New(10, &fakeDepacketizer{}, 1)
	j := 0
	for i := 0; i < 0x20000; i++ {
		k := i
		if i%4 == 1 || i%4 == 3 {
			k = i ^ 2
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(k),
				Timestamp:      uint32(k + 42),
			},
			Payload: []byte{byte(k)},
		}
		s.Push(&p)
		require.NoError(t, s.check())
		for {
			sample, ts := s.PopWithTimestamp()
			require.NoError(t, s.check())
			if sample == nil {
				break
			}
			if ts != uint32(j+43) {
				t.Errorf(
					"wrong timestamp (got %v, expected %v)",
					ts, uint32(j+43),
				)
			}
			if len(sample.Data) != 1 {
				t.Errorf(
					"bad data length (got %v, expected 1)",
					len(sample.Data),
				)
			}
			if sample.Data[0] != byte(j+1) {
				t.Errorf(
					"bad data (got %v, expected %v)",
					sample.Data[0], byte(j+1))
			}
			j++
		}
	}
	// only the first and last packet should be dropped
	if j != 0x1FFFE {
		t.Errorf("Got %v, expected %v", j, 0x1FFFE)
	}
}

func TestSampleBuilderDisorderedChecker(t *testing.T) {
	s := New(10, &fakeDepacketizer{
		headChecker: func(_ []byte) bool { return true },
		tailChecker: func(_ []byte, _ bool) bool { return true },
	}, 1)
	j := 0
	for i := 0; i < 0x20000; i++ {
		k := i
		if i%4 == 1 || i%4 == 3 {
			k = i ^ 2
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(k),
				Timestamp:      uint32(k + 42),
			},
			Payload: []byte{byte(k)},
		}
		s.Push(&p)
		require.NoError(t, s.check())
		for {
			sample, ts := s.PopWithTimestamp()
			require.NoError(t, s.check())
			if sample == nil {
				break
			}
			if ts != uint32(j+42) {
				t.Errorf(
					"wrong timestamp (got %v, expected %v)",
					ts, uint32(j+42),
				)
			}
			if len(sample.Data) != 1 {
				t.Errorf(
					"bad data length (got %v, expected 1)",
					len(sample.Data),
				)
			}
			if sample.Data[0] != byte(j) {
				t.Errorf(
					"bad data (got %v, expected %v)",
					sample.Data[0], byte(j))
			}
			j++
		}
	}
	// no packet drops
	if j != 0x20000 {
		t.Errorf("Got %v, expected %v", j, 0x1FFFE)
	}
}

func TestSampleBuilderDisorderedLossChecker(t *testing.T) {
	s := New(10, &fakeDepacketizer{
		headChecker: func(_ []byte) bool { return true },
		tailChecker: func(_ []byte, _ bool) bool { return true },
	}, 1)
	j := 0
	previous := uint32(42)
	for i := 0; i < 0x20000; i++ {
		if i%5 == 2 {
			continue
		}
		k := i
		if i%4 == 1 || i%4 == 3 {
			k = i ^ 2
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(k),
				Timestamp:      uint32(k + 42),
			},
			Payload: []byte{byte(k)},
		}
		s.Push(&p)
		require.NoError(t, s.check())
		for {
			sample, ts := s.PopWithTimestamp()
			require.NoError(t, s.check())
			if sample == nil {
				break
			}
			if ts-previous > 2 {
				t.Errorf("wrong timestamp "+
					"(got %v, expected roughly %v)",
					ts, previous)
			}
			previous = ts
			if len(sample.Data) != 1 {
				t.Errorf(
					"bad data length (got %v, expected 1)",
					len(sample.Data),
				)
			}
			if sample.Data[0] != byte(ts-42) {
				t.Errorf(
					"bad data (got %v, expected %v)",
					sample.Data[0], byte(ts-42))
			}
			j++
		}
	}
	if j != 0x20000*4/5-7 {
		t.Errorf("Got %v, expected %v", j, 0x20000*4/5-7)
	}
}

func TestSampleBuilderFull(t *testing.T) {
	s := New(10, &fakeDepacketizer{}, 1)
	s.Push(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 5000, Timestamp: 5},
		Payload: []byte{0},
	})
	for i := uint16(5001); i < 5100; i++ {
		s.Push(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: i, Timestamp: 5},
			Payload: []byte{1},
		})
		require.NoError(t, s.check())
	}
	sample, _ := s.ForcePopWithTimestamp()
	require.NoError(t, s.check())
	if sample != nil {
		t.Errorf("Got %v, expected nil", sample)
	}
}

func TestSampleBuilderForce(t *testing.T) {
	s := New(20, &fakeDepacketizer{
		headChecker: func(body []byte) bool {
			return body[0] == 0
		},
		tailChecker: func(body []byte, _ bool) bool {
			return body[0] == 0
		},
	}, 1)
	for i, ts := range []uint32{1, 2, 2, 3, 0, 3, 4, 4, 5} {
		if ts == 0 {
			continue
		}
		s.Push(&rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      ts,
			},
			Payload: []byte{byte(i)},
		})
		require.NoError(t, s.check())
	}

	var normal, forced []uint32
	for {
		sample, ts := s.PopWithTimestamp()
		require.NoError(t, s.check())
		if sample == nil {
			break
		}
		normal = append(normal, ts)
	}
	expected := []uint32{1, 2}
	if !reflect.DeepEqual(normal, expected) {
		t.Errorf("Got %#v, expected %#v", normal, expected)
	}
	for {
		sample, ts := s.ForcePopWithTimestamp()
		require.NoError(t, s.check())
		if sample == nil {
			break
		}
		forced = append(forced, ts)
	}
	expected = []uint32{4}
	if !reflect.DeepEqual(forced, expected) {
		t.Errorf("Got %#v, expected %#v", forced, expected)
	}
}

func BenchmarkSampleBuilderSequential(b *testing.B) {
	s := New(100, &fakeDepacketizer{}, 1)
	b.ResetTimer()
	j := 0
	for i := 0; i < b.N; i++ {
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i + 42),
			},
			Payload: make([]byte, 50),
		}
		s.Push(&p)
		for {
			s := s.Pop()
			if s == nil {
				break
			}
			j++
		}
	}
	if b.N > 200 && j < b.N-100 {
		b.Errorf("Got %v (N=%v)", j, b.N)
	}
}

func BenchmarkSampleBuilderLoss(b *testing.B) {
	s := New(100, &fakeDepacketizer{}, 1)
	b.ResetTimer()
	j := 0
	for i := 0; i < b.N; i++ {
		if i%13 == 0 {
			continue
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i + 42),
			},
			Payload: make([]byte, 50),
		}
		s.Push(&p)
		for {
			s := s.Pop()
			if s == nil {
				break
			}
			j++
		}
	}
	if b.N > 200 && j < b.N/2-100 {
		b.Errorf("Got %v (N=%v)", j, b.N)
	}
}

func BenchmarkSampleBuilderReordered(b *testing.B) {
	s := New(100, &fakeDepacketizer{}, 1)
	b.ResetTimer()
	j := 0
	for i := 0; i < b.N; i++ {
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i ^ 3),
				Timestamp:      uint32((i ^ 3) + 42),
			},
			Payload: make([]byte, 50),
		}
		s.Push(&p)
		for {
			s := s.Pop()
			if s == nil {
				break
			}
			j++
		}
	}
	if b.N > 2 && j < b.N-5 && j > b.N {
		b.Errorf("Got %v (N=%v)", j, b.N)
	}
}

func BenchmarkSampleBuilderFragmented(b *testing.B) {
	s := New(100, &fakeDepacketizer{}, 1)
	b.ResetTimer()
	j := 0
	for i := 0; i < b.N; i++ {
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i/2 + 42),
			},
			Payload: make([]byte, 50),
		}
		s.Push(&p)
		for {
			s := s.Pop()
			if s == nil {
				break
			}
			j++
		}
	}
	if b.N > 200 && j < b.N/2-100 {
		b.Errorf("Got %v (N=%v)", j, b.N)
	}
}

func BenchmarkSampleBuilderFragmentedLoss(b *testing.B) {
	s := New(100, &fakeDepacketizer{}, 1)
	b.ResetTimer()
	j := 0
	for i := 0; i < b.N; i++ {
		if i%13 == 0 {
			continue
		}
		p := rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i/2 + 42),
			},
			Payload: make([]byte, 50),
		}
		s.Push(&p)
		for {
			s := s.Pop()
			if s == nil {
				break
			}
			j++
		}
	}
	if b.N > 200 && j < b.N/3-100 {
		b.Errorf("Got %v (N=%v)", j, b.N)
	}
}
