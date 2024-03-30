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

import (
	"context"
	"encoding/binary"
	"io"
	"time"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

const SDPName = "telephone-event/8000"

func init() {
	media.RegisterCodec(media.NewCodec(media.CodecInfo{
		SDPName:     SDPName,
		RTPIsStatic: false,
		Priority:    100, // let it be first in SDP
	}))
}

const (
	defVolume = 10
	defRTPDur = rtp.DefPacketDur

	// dtmfUserDelay is a delay per each 'w' character in DTMF.
	dtmfUserDelay = time.Second / 2

	dtmfDelayRate = int(time.Second / dtmfUserDelay)
	// dtmfDelayDur is a duration of dtmfUserDelay in RTP time units.
	dtmfDelayDur = uint32(rtp.DefSampleRate / dtmfDelayRate)
)

var eventToChar = [256]byte{
	0: '0', 1: '1', 2: '2', 3: '3', 4: '4',
	5: '5', 6: '6', 7: '7', 8: '8', 9: '9',
	10: '*', 11: '#',
	12: 'a', 13: 'b', 14: 'c', 15: 'd',
}

var charToEvent = map[byte]byte{
	'0': 0, '1': 1, '2': 2, '3': 3, '4': 4,
	'5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
	'*': 10, '#': 11,
	'a': 12, 'b': 13, 'c': 14, 'd': 15,
}

type Event struct {
	Code   byte
	Digit  byte
	Volume byte   // in dBm0 (without sign)
	Dur    uint16 // in timestamp units
	End    bool
}

func Decode(data []byte) (Event, error) {
	if len(data) < 4 {
		return Event{}, io.ErrUnexpectedEOF
	}
	ev := data[0] // RFC2833
	b := eventToChar[ev]
	return Event{
		Code:   ev,
		Digit:  b,
		End:    data[1]>>7 != 0,
		Volume: data[1] & 0x3F,
		Dur:    binary.BigEndian.Uint16(data[2:4]),
	}, nil
}

func DecodeRTP(p *rtp.Packet) (Event, bool) {
	if !p.Marker {
		return Event{}, false
	}
	ev, err := Decode(p.Payload)
	if err != nil {
		return Event{}, false
	}
	return ev, true
}

func Encode(out []byte, ev Event) (int, error) {
	if len(out) < 4 {
		return 0, io.ErrShortBuffer
	}
	if ev.Digit != 0 {
		ev.Code = charToEvent[ev.Digit]
	}
	out[0] = ev.Code
	out[1] = ev.Volume & 0x3f
	if ev.End {
		out[1] |= 1 << 7
	}
	binary.BigEndian.PutUint16(out[2:4], ev.Dur)
	return 4, nil
}

// WriteRTP writes DTMF tones to RTP stream.
//
// Digits may contain a special character 'w' which adds a 0.5 sec delay.
func WriteRTP(ctx context.Context, out *rtp.Stream, digits string) error {
	var buf [4]byte
	for _, digit := range digits {
		if digit == 'w' {
			out.Delay(dtmfDelayDur)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(dtmfUserDelay):
			}
			continue
		}
		n, err := Encode(buf[:], Event{
			Digit:  byte(digit),
			Volume: defVolume,
			Dur:    uint16(defRTPDur),
			End:    true,
		})
		if err != nil {
			return err
		}
		// TODO: Generate non-mark packets as well?
		//       Technically, we shouldn't need them as long as End bit is set.
		err = out.WritePayload(buf[:n], true)
		if err != nil {
			return err
		}
	}
	return nil
}
