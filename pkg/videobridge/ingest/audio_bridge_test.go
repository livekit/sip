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

package ingest

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

type mockOpusWriter struct {
	frames [][]int16
}

func (m *mockOpusWriter) WriteOpusPCM(samples []int16) error {
	cp := make([]int16, len(samples))
	copy(cp, samples)
	m.frames = append(m.frames, cp)
	return nil
}

func TestAudioBridge_PCMU_Decode(t *testing.T) {
	log := logger.GetLogger()
	bridge := NewAudioBridge(log, G711PCMU)
	mock := &mockOpusWriter{}
	bridge.SetOutput(mock)

	// 160 bytes of G.711 μ-law = 20ms at 8kHz
	// After upsample to 48kHz = 960 samples = 1 Opus frame
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = 0xFF // μ-law silence (close to 0)
	}

	pkt := &rtp.Packet{
		Header:  rtp.Header{PayloadType: 0, Timestamp: 0, SequenceNumber: 1},
		Payload: payload,
	}

	err := bridge.HandleRTP(pkt)
	require.NoError(t, err)

	// 160 samples at 8kHz → 960 at 48kHz → exactly 1 Opus frame
	require.Len(t, mock.frames, 1)
	assert.Len(t, mock.frames[0], 960)
}

func TestAudioBridge_PCMA_Decode(t *testing.T) {
	log := logger.GetLogger()
	bridge := NewAudioBridge(log, G711PCMA)
	mock := &mockOpusWriter{}
	bridge.SetOutput(mock)

	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = 0xD5 // A-law silence
	}

	pkt := &rtp.Packet{
		Header:  rtp.Header{PayloadType: 8, Timestamp: 0, SequenceNumber: 1},
		Payload: payload,
	}

	err := bridge.HandleRTP(pkt)
	require.NoError(t, err)
	require.Len(t, mock.frames, 1)
	assert.Len(t, mock.frames[0], 960)
}

func TestAudioBridge_SmallPackets_Accumulate(t *testing.T) {
	log := logger.GetLogger()
	bridge := NewAudioBridge(log, G711PCMU)
	mock := &mockOpusWriter{}
	bridge.SetOutput(mock)

	// Send 80-byte packets (10ms each). Need 2 to fill one 20ms Opus frame.
	payload := make([]byte, 80)
	for i := range payload {
		payload[i] = 0xFF
	}

	pkt := &rtp.Packet{
		Header:  rtp.Header{PayloadType: 0},
		Payload: payload,
	}

	// First 10ms — not enough for an Opus frame
	err := bridge.HandleRTP(pkt)
	require.NoError(t, err)
	assert.Len(t, mock.frames, 0)

	// Second 10ms — now we have 20ms = 960 samples at 48kHz
	pkt.Header.SequenceNumber = 2
	err = bridge.HandleRTP(pkt)
	require.NoError(t, err)
	assert.Len(t, mock.frames, 1)
	assert.Len(t, mock.frames[0], 960)
}

func TestAudioBridge_NoOutput(t *testing.T) {
	log := logger.GetLogger()
	bridge := NewAudioBridge(log, G711PCMU)
	// No output set

	payload := make([]byte, 160)
	pkt := &rtp.Packet{
		Header:  rtp.Header{PayloadType: 0},
		Payload: payload,
	}

	// Should not panic or error
	err := bridge.HandleRTP(pkt)
	assert.NoError(t, err)
}

func TestAudioBridge_EmptyPayload(t *testing.T) {
	log := logger.GetLogger()
	bridge := NewAudioBridge(log, G711PCMU)
	mock := &mockOpusWriter{}
	bridge.SetOutput(mock)

	pkt := &rtp.Packet{
		Header:  rtp.Header{PayloadType: 0},
		Payload: nil,
	}

	err := bridge.HandleRTP(pkt)
	assert.NoError(t, err)
	assert.Len(t, mock.frames, 0)
}

func TestUlawDecode_Symmetry(t *testing.T) {
	// μ-law 0xFF is near-zero (silence)
	v := ulawDecode(0xFF)
	assert.True(t, v >= -10 && v <= 10, "μ-law 0xFF should decode near zero, got %d", v)

	// μ-law 0x00 is max negative
	vMax := ulawDecode(0x00)
	assert.True(t, vMax < -8000, "μ-law 0x00 should be large negative, got %d", vMax)
}

func TestAlawDecode_Symmetry(t *testing.T) {
	// A-law 0xD5 is near-zero (silence)
	v := alawDecode(0xD5)
	assert.True(t, v >= -20 && v <= 20, "A-law 0xD5 should decode near zero, got %d", v)
}
