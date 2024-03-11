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

package dtmf

import "github.com/pion/rtp"

const SDPName = "telephone-event/8000"

var eventToChar = [256]byte{
	0: '0', 1: '1', 2: '2', 3: '3', 4: '4',
	5: '5', 6: '6', 7: '7', 8: '8', 9: '9',
	10: '*', 11: '#',
	12: 'a', 13: 'b', 14: 'c', 15: 'd',
}

type Tone struct {
	Code  byte
	Digit byte
}

func Decode(data []byte) (Tone, bool) {
	if len(data) < 4 {
		return Tone{}, false
	}
	ev := data[0] // RFC2833
	b := eventToChar[ev]
	return Tone{
		Code:  ev,
		Digit: b,
	}, true
}

func DecodeRTP(p *rtp.Packet) (Tone, bool) {
	if !p.Marker {
		return Tone{}, false
	}
	return Decode(p.Payload)
}
