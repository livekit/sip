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

package sip

import (
	"fmt"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/livekit/media-sdk/rtp"
)

type testHeader struct {
	sequenceNumber uint16
	timestamp      uint32
	delta          time.Duration
	skip           bool
}

func (h *testHeader) Get() *rtp.Header {
	return &rtp.Header{
		SequenceNumber: h.sequenceNumber,
		Timestamp:      h.timestamp,
	}
}

type testWriter struct {
	tsHop    uint32
	timeHop  time.Duration
	header   testHeader
	baseTime time.Time
}

type testWriterOption func(*testWriter)

func WithStartSeq(seq uint16) testWriterOption {
	return func(w *testWriter) {
		w.header.sequenceNumber = seq
	}
}

func WithStartTs(ts uint32) testWriterOption {
	return func(w *testWriter) {
		w.header.timestamp = ts
	}
}

func NewTestWriter(tsHop uint32, timeHop time.Duration, opts ...testWriterOption) *testWriter {
	w := &testWriter{
		tsHop:    tsHop,
		timeHop:  timeHop,
		header:   testHeader{sequenceNumber: 1000, timestamp: 10000, delta: timeHop, skip: false},
		baseTime: time.Now(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

func (w *testWriter) GenerateHeader() *testHeader {
	copy := w.header
	w.header.sequenceNumber++
	w.header.timestamp += w.tsHop
	return &copy
}

func (w *testWriter) GenerateArray(count int) []*testHeader {
	headers := make([]*testHeader, count)
	for i := 0; i < count; i++ {
		headers[i] = w.GenerateHeader()
	}
	return headers
}

type validateFunc func(i int, gauge *rtpGauge, result *rtpGaugeData) error

func testVariableTime(t *testing.T, packets []*testHeader, validate validateFunc, sampleRate float64, packetRate float64) *rtpGauge {
	gauge := &rtpGauge{
		timeHop:        time.Second / time.Duration(packetRate),
		sampleHop:      int32(sampleRate / packetRate),
		resetThreshold: 5 * time.Second,
	}

	synctest.Test(t, func(t *testing.T) {
		for i, packet := range packets {
			time.Sleep(packet.delta)
			if packet.skip {
				continue
			}
			header := packet.Get()
			result := gauge.Process(header)
			if validate != nil {
				err := validate(i, gauge, result)
				if err != nil {
					err = fmt.Errorf("packet %d validation failed: %w", i, err)
					t.Error(err)
				}
			}
		}
	})

	return gauge
}

func TestRtpGauge_LargeScale(t *testing.T) {
	const (
		sampleRate = 48000.0
		packetRate = 50.0                            // 20ms packets
		tsHop      = uint32(sampleRate / packetRate) // 960 samples = 20ms at 48kHz
		timeHop    = time.Second / packetRate        // 20ms packets
	)
	writer := NewTestWriter(tsHop, timeHop, WithStartSeq(math.MaxUint16-2), WithStartTs(math.MaxUint32-1500))
	packets := writer.GenerateArray(10000)

	// Introduce some variation: skip some packets, add jitter
	skipped := 0
	for i, packet := range packets {
		if (i % 7) == 0 {
			skipped++
			packet.skip = true
		}
		if i%13 == 12 {
			packets[i-3].delta = timeHop * 3
			packets[i-2].delta = 300 * time.Microsecond
			packets[i-1].delta = 300 * time.Microsecond
			packets[i-0].delta = (timeHop * 4) - packets[i-3].delta - packets[i-2].delta - packets[i-1].delta
		} else if i%5 == 0 { // jitter
			packet.delta += time.Duration((i%3)-1) * time.Millisecond
		}
	}

	valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
		if i%5 == 0 {
			fmt.Printf("DEBUG: index: %d, delta: %v\n", i, packets[i].delta)
		}
		if result != nil {
			return fmt.Errorf("no resets expected, got %v", result)
		}
		return nil
	}
	gauge := testVariableTime(t, packets, valFunc, sampleRate, packetRate)

	stats := gauge.GetData()
	expectedMaxDelta := (timeHop * 3) + (3 * time.Millisecond) // +3ms jitter
	if stats.maxDelta != expectedMaxDelta {
		t.Errorf("Max delay should be %v, got %v", expectedMaxDelta, stats.maxDelta)
	}
	expectedMinDelta := timeHop
	if stats.minDelta != expectedMinDelta {
		t.Errorf("Min delay should be %v, got %v", expectedMinDelta, stats.minDelta)
	}
	expectedCount := len(packets) - skipped - 1 // -1 because we're not counting the first packet
	if int(stats.numDelta) > expectedCount {
		t.Errorf("Should have not surpassed %d packets, got %d", expectedCount, int(stats.numDelta))
	}
}

func TestRtpGauge_BasicProjection(t *testing.T) {
	const (
		sampleRate = 48000.0
		packetRate = 50.0                            // 20ms packets
		tsHop      = uint32(sampleRate / packetRate) // 960 samples = 20ms at 48kHz
		timeHop    = time.Second / packetRate        // 20ms packets
	)
	writer := NewTestWriter(tsHop, timeHop)
	packets := writer.GenerateArray(3)
	packets[0].delta = 0
	packets[1].delta = 20 * time.Millisecond
	packets[2].delta = 50 * time.Millisecond

	valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
		if result != nil {
			return fmt.Errorf("no resets expected, encountered reset at packet %d", i)
		}
		data := gauge.GetData()
		if data.numDelta != i {
			return fmt.Errorf("count should be %d, got %d", i, data.numDelta)
		}
		if i > 0 && data.maxDelta != packets[i].delta {
			return fmt.Errorf("max timestamp delay should be %v, got %v", packets[i].delta, data.maxDelta)
		}
		if int64(gauge.projectedTs) != int64(packets[i].timestamp) {
			return fmt.Errorf("timestamp should be %d, got %d", packets[i].timestamp, gauge.projectedTs)
		}
		if int64(gauge.projectedSeq) != int64(packets[i].sequenceNumber) {
			return fmt.Errorf("sequence number should be %d, got %d", packets[i].sequenceNumber, gauge.projectedSeq)
		}
		return nil
	}

	testVariableTime(t, packets, valFunc, sampleRate, packetRate)
}

func TestRtpGauge_ResetDetection(t *testing.T) {
	const (
		sampleRate = 48000.0
		packetRate = 50.0
		tsHop      = uint32(sampleRate / packetRate)
		timeHop    = time.Second / packetRate
	)
	writer := NewTestWriter(tsHop, timeHop)

	t.Run("timestamp_positive", func(t *testing.T) {
		specialSeq := 5
		packets := writer.GenerateArray(6)
		packets[specialSeq].timestamp += uint32(sampleRate*5) + tsHop

		valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
			if i != specialSeq {
				if result != nil {
					return fmt.Errorf("not expecting a reset, got %v", result)
				}
			} else { // i == specialSeq
				if result == nil {
					return fmt.Errorf("expected a reset, got nil")
				}
			}
			return nil
		}

		testVariableTime(t, packets, valFunc, sampleRate, packetRate)
	})

	t.Run("timestamp_negative", func(t *testing.T) {
		specialSeq := 5
		packets := writer.GenerateArray(6)
		packets[specialSeq].timestamp -= uint32(sampleRate*5) + tsHop

		valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
			if i != specialSeq {
				if result != nil {
					return fmt.Errorf("not expecting a reset, got %v", result)
				}
			} else { // i == specialSeq
				if result == nil {
					return fmt.Errorf("expected a reset, got nil")
				}
			}
			return nil
		}

		testVariableTime(t, packets, valFunc, sampleRate, packetRate)
	})

	t.Run("seq_positive", func(t *testing.T) {
		specialSeq := 5
		packets := writer.GenerateArray(6)
		packets[specialSeq].sequenceNumber += uint16(packetRate*5) + 1

		valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
			if i != specialSeq {
				if result != nil {
					return fmt.Errorf("not expecting a reset, got %v", result)
				}
			} else { // i == specialSeq
				if result == nil {
					return fmt.Errorf("expected a reset, got nil")
				}
			}
			return nil
		}

		testVariableTime(t, packets, valFunc, sampleRate, packetRate)
	})

	t.Run("timestamp_negative", func(t *testing.T) {
		specialSeq := 5
		packets := writer.GenerateArray(6)
		packets[specialSeq].sequenceNumber -= uint16(packetRate*5) + 1

		valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
			if i != specialSeq {
				if result != nil {
					return fmt.Errorf("not expecting a reset, got %v", result)
				}
			} else { // i == specialSeq
				if result == nil {
					return fmt.Errorf("expected a reset, got nil")
				}
			}
			return nil
		}

		testVariableTime(t, packets, valFunc, sampleRate, packetRate)
	})
}

func TestRtpGauge_BaselineUpdate(t *testing.T) {
	const (
		sampleRate = 48000.0
		packetRate = 50.0
		tsHop      = uint32(sampleRate / packetRate)
		timeHop    = time.Second / packetRate
	)
	writer := NewTestWriter(tsHop, timeHop)
	packets := writer.GenerateArray(4)

	packets[0].delta = 0
	packets[1].delta = 50 * time.Millisecond // p0 + 50ms
	packets[2].delta = 1 * time.Millisecond  // p0 + 51ms (expected at p0 + 40ms)
	packets[3].delta = 1 * time.Millisecond  // p0 + 52ms (expected at p0 + 60ms, so should trigger an update)

	var projectedTime1 time.Time
	valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
		if i == 0 {
			projectedTime1 = gauge.projectedTime
			return nil
		}
		if i <= 2 {
			// Late packet should record delay
			data := gauge.GetData()
			if data.numDelta != i {
				return fmt.Errorf("should have recorded delay of %d packet, got %d", i, int(data.numDelta))
			}
			return nil
		}
		if i == 3 {
			// Early packet should update baseline (reset without returning data)
			if result != nil {
				return fmt.Errorf("baseline update should not return data")
			}
			// Projected time should be updated
			if gauge.projectedTime.Equal(projectedTime1) {
				return fmt.Errorf("projected time should be updated after early packet")
			}
			// Count should be reset after baseline update
			data := gauge.GetData()
			if data.numDelta != 0 {
				return fmt.Errorf("after baseline update, count should be 0, got %d", int(data.numDelta))
			}
		}
		return nil
	}

	testVariableTime(t, packets, valFunc, sampleRate, packetRate)
}

func TestRtpGauge_UpdatingProjection(t *testing.T) {
	const (
		sampleRate = 48000.0
		packetRate = 50.0
		tsHop      = uint32(sampleRate / packetRate)
		timeHop    = time.Second / packetRate
	)
	writer := NewTestWriter(tsHop, timeHop)
	packets := writer.GenerateArray(10)

	packets[3].delta -= 5 * time.Millisecond  // Arrive 5ms early, triggers an update
	packets[4].delta += 15 * time.Millisecond // Arrive 10ms compared to p0, 35ms from p3
	sumDelta := (timeHop * 10) - (5 * time.Millisecond) + (15 * time.Millisecond)

	gauge := testVariableTime(t, packets, nil, sampleRate, packetRate)

	// Sanity
	if int(gauge.GetData().numDelta) != 6 {
		t.Errorf("Should have processed 6 packets, got %d", int(gauge.GetData().numDelta))
	}
	if gauge.GetData().maxDelta != 35*time.Millisecond { // p3 -> p4 is 20 + 15 ms
		t.Errorf("Max delay should be %v, got %v", 35*time.Millisecond, gauge.GetData().maxDelta)
	}
	if int(gauge.GetData().minDelta/time.Millisecond) != 35 { // p4+ arrives 20ms + 15ms after baseline
		t.Errorf("Min delay should be %v, got %v", 35, gauge.GetData().minDelta)
	}

	// Make sure we're projecting the earliest possible time:
	if gauge.projectedTs != packets[9].timestamp {
		t.Errorf("Projected timestamp should be %d, got %d", packets[9].timestamp, gauge.projectedTs)
	}
	if gauge.projectedSeq != packets[9].sequenceNumber {
		t.Errorf("Projected sequence number should be %d, got %d", packets[9].sequenceNumber, gauge.projectedSeq)
	}
	gaugeData := gauge.GetData()
	if gaugeData.sumDelta != sumDelta {
		t.Errorf("Sum delay should be %v, got %v", sumDelta, gaugeData.sumDelta)
	}
}

func TestRtpGauge_WrapAround(t *testing.T) {
	const (
		sampleRate = 48000.0
		packetRate = 50.0
		tsHop      = uint32(sampleRate / packetRate)
		timeHop    = time.Second / packetRate
	)

	t.Run("timestamp", func(t *testing.T) {
		writer := NewTestWriter(tsHop, timeHop, WithStartTs(math.MaxUint32-uint32((0.75*float64(tsHop)))))
		packets := writer.GenerateArray(2)

		valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
			if result != nil {
				return fmt.Errorf("wrap-around should not trigger reset if within threshold")
			}
			if i == 1 {
				data := gauge.GetData()
				if data.numDelta == 0 {
					return fmt.Errorf("should handle timestamp wrap-around correctly")
				}
			}
			return nil
		}

		testVariableTime(t, packets, valFunc, sampleRate, packetRate)
	})

	t.Run("sequence", func(t *testing.T) {
		writer := NewTestWriter(tsHop, timeHop, WithStartSeq(math.MaxUint16-2))
		packets := writer.GenerateArray(4)

		valFunc := func(i int, gauge *rtpGauge, result *rtpGaugeData) error {
			if result != nil {
				return fmt.Errorf("wrap-around should not trigger reset if within threshold")
			}
			if i != 0 {
				data := gauge.GetData()
				if data.numDelta == 0 {
					return fmt.Errorf("should handle sequence wrap-around correctly")
				}
			}
			return nil
		}

		testVariableTime(t, packets, valFunc, sampleRate, packetRate)
	})
}

func TestRtpGauge_ResetMethod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			sampleRate = 48000.0
			packetRate = 50.0
			tsHop      = uint32(sampleRate / packetRate)
			timeHop    = time.Second / packetRate
		)
		gauge := &rtpGauge{
			timeHop:        time.Second / time.Duration(packetRate),
			sampleHop:      int32(sampleRate / packetRate),
			resetThreshold: 5 * time.Second,
		}

		// Process some packets
		writer := NewTestWriter(tsHop, timeHop)
		packets := writer.GenerateArray(2)

		header1 := packets[0].Get()
		gauge.Process(header1)

		time.Sleep(timeHop)
		header2 := packets[1].Get()
		gauge.Process(header2)

		initialData := gauge.GetData()
		if initialData.numDelta == 0 {
			t.Error("Should have some data")
		}

		// Manual reset
		resetHeader := &rtp.Header{
			SequenceNumber: 200,
			Timestamp:      5000,
		}
		returnedData := gauge.Reset(resetHeader)
		if returnedData == nil {
			t.Error("Reset should return previous data")
		} else if returnedData.numDelta != initialData.numDelta {
			t.Errorf("Returned data should match initial data, got count %d vs %d", int(returnedData.numDelta), int(initialData.numDelta))
		}

		// After reset, gauge should be reinitialized
		data := gauge.GetData()
		if data.numDelta != 0 {
			t.Errorf("After reset, count should be 0, got %d", int(data.numDelta))
		}
		if gauge.projectedTs != 5000 {
			t.Errorf("Projected timestamp should be reset to 5000, got %d", gauge.projectedTs)
		}
		if gauge.projectedSeq != 200 {
			t.Errorf("Projected sequence should be reset to 200, got %d", gauge.projectedSeq)
		}
	})
}
