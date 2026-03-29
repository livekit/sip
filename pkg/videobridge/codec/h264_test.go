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

package codec

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDepacketize_SingleNAL(t *testing.T) {
	d := NewH264Depacketizer()

	// Single NAL unit: type 5 (IDR), with some payload
	payload := []byte{0x65, 0x88, 0x84, 0x00, 0xFF} // NAL header 0x65 = IDR (type 5)

	nals, err := d.Depacketize(payload)
	require.NoError(t, err)
	require.Len(t, nals, 1)
	assert.Equal(t, uint8(NALTypeIDR), nals[0].Type)
	assert.True(t, nals[0].IsKeyframe())
	assert.True(t, nals[0].IsStart)
	assert.True(t, nals[0].IsEnd)
	assert.Equal(t, payload, nals[0].Data)
}

func TestDepacketize_SingleNAL_NonIDR(t *testing.T) {
	d := NewH264Depacketizer()

	// Single NAL unit: type 1 (non-IDR slice)
	payload := []byte{0x41, 0x9A, 0x00, 0x10}

	nals, err := d.Depacketize(payload)
	require.NoError(t, err)
	require.Len(t, nals, 1)
	assert.Equal(t, uint8(NALTypeSlice), nals[0].Type)
	assert.False(t, nals[0].IsKeyframe())
}

func TestDepacketize_SPS_PPS(t *testing.T) {
	d := NewH264Depacketizer()

	// SPS (type 7)
	spsPayload := []byte{0x67, 0x42, 0xE0, 0x1F, 0xDA, 0x01}
	nals, err := d.Depacketize(spsPayload)
	require.NoError(t, err)
	require.Len(t, nals, 1)
	assert.Equal(t, uint8(NALTypeSPS), nals[0].Type)
	assert.True(t, nals[0].IsParameterSet())

	// PPS (type 8)
	ppsPayload := []byte{0x68, 0xCE, 0x38, 0x80}
	nals, err = d.Depacketize(ppsPayload)
	require.NoError(t, err)
	require.Len(t, nals, 1)
	assert.Equal(t, uint8(NALTypePPS), nals[0].Type)
	assert.True(t, nals[0].IsParameterSet())
}

func TestDepacketize_STAPA(t *testing.T) {
	d := NewH264Depacketizer()

	// Build STAP-A: header byte (24) + [size1][nal1] + [size2][nal2]
	sps := []byte{0x67, 0x42, 0xE0, 0x1F}
	pps := []byte{0x68, 0xCE, 0x38, 0x80}

	payload := []byte{NALTypeSTAPA} // STAP-A header

	// SPS
	sizeBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(sizeBuf, uint16(len(sps)))
	payload = append(payload, sizeBuf...)
	payload = append(payload, sps...)

	// PPS
	binary.BigEndian.PutUint16(sizeBuf, uint16(len(pps)))
	payload = append(payload, sizeBuf...)
	payload = append(payload, pps...)

	nals, err := d.Depacketize(payload)
	require.NoError(t, err)
	require.Len(t, nals, 2)

	assert.Equal(t, uint8(NALTypeSPS), nals[0].Type)
	assert.Equal(t, sps, nals[0].Data)

	assert.Equal(t, uint8(NALTypePPS), nals[1].Type)
	assert.Equal(t, pps, nals[1].Data)
}

func TestDepacketize_FUA(t *testing.T) {
	d := NewH264Depacketizer()

	// Fragment a large IDR NAL into 3 FU-A packets
	originalNAL := make([]byte, 300)
	originalNAL[0] = 0x65 // IDR header (forbidden=0, nri=3, type=5)
	for i := 1; i < len(originalNAL); i++ {
		originalNAL[i] = byte(i % 256)
	}

	nalRefIDC := originalNAL[0] & 0x60 // 0x60
	nalType := originalNAL[0] & 0x1F   // 5 (IDR)
	fuIndicator := nalRefIDC | NALTypeFUA

	data := originalNAL[1:] // skip NAL header

	// Packet 1: start
	pkt1 := []byte{fuIndicator, 0x80 | nalType} // start bit + type
	pkt1 = append(pkt1, data[:100]...)

	nals, err := d.Depacketize(pkt1)
	require.NoError(t, err)
	assert.Len(t, nals, 0) // incomplete

	// Packet 2: middle
	pkt2 := []byte{fuIndicator, nalType} // no start, no end
	pkt2 = append(pkt2, data[100:200]...)

	nals, err = d.Depacketize(pkt2)
	require.NoError(t, err)
	assert.Len(t, nals, 0) // still incomplete

	// Packet 3: end
	pkt3 := []byte{fuIndicator, 0x40 | nalType} // end bit + type
	pkt3 = append(pkt3, data[200:]...)

	nals, err = d.Depacketize(pkt3)
	require.NoError(t, err)
	require.Len(t, nals, 1)

	assert.Equal(t, uint8(NALTypeIDR), nals[0].Type)
	assert.True(t, nals[0].IsKeyframe())
	assert.Equal(t, originalNAL, nals[0].Data) // fully reassembled
}

func TestDepacketize_FUA_MissingStart(t *testing.T) {
	d := NewH264Depacketizer()

	// Send a middle fragment without a start fragment
	fuIndicator := byte(0x60 | NALTypeFUA)
	pkt := []byte{fuIndicator, NALTypeIDR, 0xAA, 0xBB}

	_, err := d.Depacketize(pkt)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "without start fragment")
}

func TestDepacketize_EmptyPayload(t *testing.T) {
	d := NewH264Depacketizer()

	_, err := d.Depacketize(nil)
	assert.Error(t, err)

	_, err = d.Depacketize([]byte{})
	assert.Error(t, err)
}

func TestDepacketize_Reset(t *testing.T) {
	d := NewH264Depacketizer()

	// Start a fragment
	fuIndicator := byte(0x60 | NALTypeFUA)
	pkt := []byte{fuIndicator, 0x80 | NALTypeIDR, 0xAA, 0xBB}
	_, _ = d.Depacketize(pkt)

	assert.True(t, d.hasFrag)

	d.Reset()
	assert.False(t, d.hasFrag)
	assert.Nil(t, d.fragBuf)
}

func TestExtractParameterSets(t *testing.T) {
	nals := []NALUnit{
		{Type: NALTypeSPS, Data: []byte{0x67, 0x42, 0xE0}},
		{Type: NALTypePPS, Data: []byte{0x68, 0xCE}},
		{Type: NALTypeIDR, Data: []byte{0x65, 0x88}},
	}

	sps, pps := ExtractParameterSets(nals)
	assert.Equal(t, []byte{0x67, 0x42, 0xE0}, sps)
	assert.Equal(t, []byte{0x68, 0xCE}, pps)
}

func TestRepacketizer_SmallNAL(t *testing.T) {
	r := NewH264Repacketizer(1200)

	nal := NALUnit{
		Type: NALTypeIDR,
		Data: make([]byte, 100),
	}

	payloads := r.Repacketize(nal)
	require.Len(t, payloads, 1)
	assert.Len(t, payloads[0], 100) // single packet, no fragmentation
}

func TestRepacketizer_LargeNAL_FUA(t *testing.T) {
	r := NewH264Repacketizer(100) // small max to force fragmentation

	nalData := make([]byte, 500)
	nalData[0] = 0x65 // IDR

	nal := NALUnit{
		Type: NALTypeIDR,
		Data: nalData,
	}

	payloads := r.Repacketize(nal)
	require.True(t, len(payloads) > 1, "should fragment into multiple packets")

	// Verify first packet has start bit
	assert.Equal(t, byte(NALTypeFUA), payloads[0][0]&0x1F)
	assert.True(t, payloads[0][1]&0x80 != 0, "first fragment should have start bit")

	// Verify last packet has end bit
	last := payloads[len(payloads)-1]
	assert.True(t, last[1]&0x40 != 0, "last fragment should have end bit")

	// Verify middle packets have neither start nor end
	for i := 1; i < len(payloads)-1; i++ {
		assert.True(t, payloads[i][1]&0x80 == 0, "middle fragment should not have start bit")
		assert.True(t, payloads[i][1]&0x40 == 0, "middle fragment should not have end bit")
	}

	// Verify NAL type is preserved in FU headers
	for _, p := range payloads {
		assert.Equal(t, byte(NALTypeIDR), p[1]&0x1F)
	}
}

func TestNALUnit_IsKeyframe(t *testing.T) {
	assert.True(t, (&NALUnit{Type: NALTypeIDR}).IsKeyframe())
	assert.False(t, (&NALUnit{Type: NALTypeSlice}).IsKeyframe())
	assert.False(t, (&NALUnit{Type: NALTypeSPS}).IsKeyframe())
}

func TestNALUnit_IsParameterSet(t *testing.T) {
	assert.True(t, (&NALUnit{Type: NALTypeSPS}).IsParameterSet())
	assert.True(t, (&NALUnit{Type: NALTypePPS}).IsParameterSet())
	assert.False(t, (&NALUnit{Type: NALTypeIDR}).IsParameterSet())
	assert.False(t, (&NALUnit{Type: NALTypeSlice}).IsParameterSet())
}
