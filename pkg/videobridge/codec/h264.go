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
	"fmt"
)

// H.264 NAL unit types (RFC 6184)
const (
	NALTypeSlice    = 1  // Coded slice of a non-IDR picture
	NALTypeIDR      = 5  // Coded slice of an IDR picture (keyframe)
	NALTypeSEI      = 6  // Supplemental enhancement information
	NALTypeSPS      = 7  // Sequence parameter set
	NALTypePPS      = 8  // Picture parameter set
	NALTypeAUD      = 9  // Access unit delimiter
	NALTypeSTAPA    = 24 // Single-time aggregation packet A
	NALTypeFUA      = 28 // Fragmentation unit A
	NALTypeFUB      = 29 // Fragmentation unit B
)

// NALUnit represents a single H.264 NAL unit.
type NALUnit struct {
	Type    uint8
	Data    []byte
	IsStart bool // first fragment of a fragmented NAL
	IsEnd   bool // last fragment of a fragmented NAL
}

// IsKeyframe returns true if this NAL is part of an IDR picture.
func (n *NALUnit) IsKeyframe() bool {
	return n.Type == NALTypeIDR
}

// IsParameterSet returns true if this is SPS or PPS.
func (n *NALUnit) IsParameterSet() bool {
	return n.Type == NALTypeSPS || n.Type == NALTypePPS
}

// H264Depacketizer reassembles H.264 NAL units from RTP packets per RFC 6184.
type H264Depacketizer struct {
	// Fragment reassembly buffer
	fragBuf  []byte
	fragType uint8
	hasFrag  bool
}

// NewH264Depacketizer creates a new H.264 RTP depacketizer.
func NewH264Depacketizer() *H264Depacketizer {
	return &H264Depacketizer{}
}

// Depacketize processes an RTP payload and returns zero or more complete NAL units.
// Handles single NAL, STAP-A, FU-A packet types.
func (d *H264Depacketizer) Depacketize(payload []byte) ([]NALUnit, error) {
	if len(payload) < 1 {
		return nil, fmt.Errorf("empty RTP payload")
	}

	// First byte: forbidden_zero_bit(1) | nal_ref_idc(2) | nal_unit_type(5)
	nalType := payload[0] & 0x1F

	switch {
	case nalType >= 1 && nalType <= 23:
		// Single NAL unit packet
		return []NALUnit{{
			Type:    nalType,
			Data:    payload,
			IsStart: true,
			IsEnd:   true,
		}}, nil

	case nalType == NALTypeSTAPA:
		return d.depacketizeSTAPA(payload)

	case nalType == NALTypeFUA:
		return d.depacketizeFUA(payload)

	case nalType == NALTypeFUB:
		return d.depacketizeFUA(payload) // FU-B is similar, rare in practice

	default:
		return nil, fmt.Errorf("unsupported NAL type: %d", nalType)
	}
}

// depacketizeSTAPA handles STAP-A packets containing multiple NALs.
func (d *H264Depacketizer) depacketizeSTAPA(payload []byte) ([]NALUnit, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("STAP-A payload too short")
	}

	var nals []NALUnit
	offset := 1 // skip STAP-A header byte

	for offset < len(payload) {
		if offset+2 > len(payload) {
			return nil, fmt.Errorf("STAP-A: incomplete NAL size at offset %d", offset)
		}
		nalSize := int(binary.BigEndian.Uint16(payload[offset:]))
		offset += 2

		if offset+nalSize > len(payload) {
			return nil, fmt.Errorf("STAP-A: NAL size %d exceeds remaining payload at offset %d", nalSize, offset)
		}

		nalData := payload[offset : offset+nalSize]
		if len(nalData) > 0 {
			nals = append(nals, NALUnit{
				Type:    nalData[0] & 0x1F,
				Data:    nalData,
				IsStart: true,
				IsEnd:   true,
			})
		}
		offset += nalSize
	}

	return nals, nil
}

// depacketizeFUA handles FU-A fragmented NAL units.
func (d *H264Depacketizer) depacketizeFUA(payload []byte) ([]NALUnit, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("FU-A payload too short")
	}

	fuIndicator := payload[0]
	fuHeader := payload[1]

	isStart := fuHeader&0x80 != 0
	isEnd := fuHeader&0x40 != 0
	nalType := fuHeader & 0x1F

	if isStart {
		// Start of a new fragmented NAL: reconstruct NAL header
		nalHeader := (fuIndicator & 0xE0) | nalType
		d.fragBuf = make([]byte, 0, len(payload)*4) // pre-allocate
		d.fragBuf = append(d.fragBuf, nalHeader)
		d.fragBuf = append(d.fragBuf, payload[2:]...)
		d.fragType = nalType
		d.hasFrag = true

		return nil, nil // incomplete, wait for more fragments
	}

	if !d.hasFrag {
		return nil, fmt.Errorf("FU-A continuation without start fragment")
	}

	if nalType != d.fragType {
		d.hasFrag = false
		d.fragBuf = nil
		return nil, fmt.Errorf("FU-A NAL type mismatch: expected %d, got %d", d.fragType, nalType)
	}

	// Append fragment data (skip FU indicator + FU header)
	d.fragBuf = append(d.fragBuf, payload[2:]...)

	if isEnd {
		// Complete NAL unit reassembled
		nal := NALUnit{
			Type:    nalType,
			Data:    d.fragBuf,
			IsStart: true,
			IsEnd:   true,
		}
		d.hasFrag = false
		d.fragBuf = nil
		return []NALUnit{nal}, nil
	}

	return nil, nil // still incomplete
}

// Reset clears any partial fragment state.
func (d *H264Depacketizer) Reset() {
	d.hasFrag = false
	d.fragBuf = nil
	d.fragType = 0
}

// H264Repacketizer converts NAL units back into WebRTC-compatible RTP payloads.
// This is used for H.264 passthrough mode.
type H264Repacketizer struct {
	maxPayloadSize int
}

// NewH264Repacketizer creates a repacketizer with the specified max RTP payload size.
func NewH264Repacketizer(maxPayloadSize int) *H264Repacketizer {
	if maxPayloadSize <= 0 {
		maxPayloadSize = 1200 // safe default below typical MTU
	}
	return &H264Repacketizer{maxPayloadSize: maxPayloadSize}
}

// Repacketize takes a NAL unit and produces one or more RTP payloads.
// Small NALs are sent as single NAL packets; large NALs are fragmented with FU-A.
func (r *H264Repacketizer) Repacketize(nal NALUnit) [][]byte {
	if len(nal.Data) <= r.maxPayloadSize {
		// Single NAL unit packet
		out := make([]byte, len(nal.Data))
		copy(out, nal.Data)
		return [][]byte{out}
	}

	// Fragment using FU-A
	return r.fragmentFUA(nal)
}

func (r *H264Repacketizer) fragmentFUA(nal NALUnit) [][]byte {
	if len(nal.Data) < 1 {
		return nil
	}

	nalHeader := nal.Data[0]
	nalRefIDC := nalHeader & 0x60
	nalType := nalHeader & 0x1F

	fuIndicator := nalRefIDC | NALTypeFUA

	// Fragment the NAL data (skip original NAL header byte)
	data := nal.Data[1:]
	maxFragSize := r.maxPayloadSize - 2 // 2 bytes for FU indicator + FU header

	var payloads [][]byte
	offset := 0
	first := true

	for offset < len(data) {
		end := offset + maxFragSize
		if end > len(data) {
			end = len(data)
		}

		fuHeader := nalType
		if first {
			fuHeader |= 0x80 // start bit
			first = false
		}
		if end == len(data) {
			fuHeader |= 0x40 // end bit
		}

		payload := make([]byte, 2+end-offset)
		payload[0] = fuIndicator
		payload[1] = fuHeader
		copy(payload[2:], data[offset:end])

		payloads = append(payloads, payload)
		offset = end
	}

	return payloads
}

// ExtractParameterSets extracts SPS and PPS from a sequence of NAL units.
func ExtractParameterSets(nals []NALUnit) (sps, pps []byte) {
	for _, nal := range nals {
		switch nal.Type {
		case NALTypeSPS:
			sps = make([]byte, len(nal.Data))
			copy(sps, nal.Data)
		case NALTypePPS:
			pps = make([]byte, len(nal.Data))
			copy(pps, nal.Data)
		}
	}
	return
}
