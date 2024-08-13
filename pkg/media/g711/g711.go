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

package g711

// from g711.c by SUN microsystems (unrestricted use)

const (
	signBit   = 0x80 // Sign bit for a A-law byte.
	quantMask = 0xf  // Quantization field mask.
	segShift  = 4    // Left shift for segment number.
	segMask   = 0x70 // Segment field mask.

	bias = 0x84 // Bias for linear code.
)

var (
	ulaw2lin [256]int16
	alaw2lin [256]int16
	lin2ulaw [16384]byte
	lin2alaw [16384]byte
)

func init() {
	buildLinTable(ulaw2lin[:], ulaw2linear)
	buildLinTable(alaw2lin[:], alaw2linear)
	buildLawTable(lin2ulaw[:], ulaw2linear, 0xff)
	buildLawTable(lin2alaw[:], alaw2linear, 0xd5)
}

func alaw2linear(v byte) int {
	v ^= 0x55

	t := int(v & quantMask)
	seg := int((uint(v) & segMask) >> segShift)
	if seg != 0 {
		t = (t + t + 1 + 32) << (seg + 2)
	} else {
		t = (t + t + 1) << 3
	}

	if (v & signBit) != 0 {
		return t
	}
	return -t
}

func ulaw2linear(v byte) int {
	v = ^v

	t := (int(v&quantMask) << 3) + bias
	t <<= (uint(v) & segMask) >> segShift

	if (v & signBit) != 0 {
		return bias - t
	}
	return t - bias
}

func buildLinTable(table []int16, log2lin func(byte) int) {
	for i := range table {
		table[i] = int16(log2lin(byte(i)))
	}
}

func buildLawTable(table []byte, log2lin func(byte) int, mask int) {
	j := 1
	table[8192] = byte(mask)
	for i := 0; i < 127; i++ {
		v1 := log2lin(byte(i ^ mask))
		v2 := log2lin(byte((i + 1) ^ mask))
		v := (v1 + v2 + 4) >> 3
		for ; j < v; j += 1 {
			table[8192-j] = byte(i ^ (mask ^ 0x80))
			table[8192+j] = byte(i ^ mask)
		}
	}
	for ; j < 8192; j++ {
		table[8192-j] = byte(127 ^ (mask ^ 0x80))
		table[8192+j] = byte(127 ^ mask)
	}
	table[0] = table[1]
}

func EncodeALawTo(out []byte, buf []int16) {
	for i, v := range buf {
		out[i] = lin2alaw[(int(v)+32768)>>2]
	}
}

func DecodeALawTo(out []int16, buf []byte) {
	for i, v := range buf {
		out[i] = alaw2lin[v]
	}
}

func EncodeULawTo(out []byte, buf []int16) {
	for i, v := range buf {
		out[i] = lin2ulaw[(int(v)+32768)>>2]
	}
}

func DecodeULawTo(out []int16, buf []byte) {
	for i, v := range buf {
		out[i] = ulaw2lin[v]
	}
}
