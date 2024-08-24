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
	"math"
	"time"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/tones"
)

const SDPName = "telephone-event/8000"
const SampleRate = 8000

func init() {
	media.RegisterCodec(media.NewCodec(media.CodecInfo{
		SDPName:     SDPName,
		SampleRate:  SampleRate,
		RTPIsStatic: false,
		Priority:    -100, // let it be last in SDP
	}))
}

const (
	eventVolume = 10
	toneVolume  = math.MaxInt16 / 2

	// eventDur is a duration of a DTMF tone. Tone is followed by a delay with the same duration.
	eventDur = 250 * time.Millisecond

	// delayDur is a delay per each 'w' character in DTMF.
	delayDur = time.Second / 2
)

const (
	code0 = byte(iota)
	code1
	code2
	code3
	code4
	code5
	code6
	code7
	code8
	code9
	codeStar
	codeHash
	codeA
	codeB
	codeC
	codeD
)

const (
	dtmfLow1 = 697
	dtmfLow2 = 770
	dtmfLow3 = 852
	dtmfLow4 = 941

	dtmfHigh1 = 1209
	dtmfHigh2 = 1336
	dtmfHigh3 = 1477
	dtmfHigh4 = 1633
)

// eventFreq maps DTMF codes to frequencies.
//
// This table follows a keypad layout:
//
//			HI1		HI2		HI3		HI4
//	LO1		1		2		3		A
//	LO2		4		5		6		B
//	LO3		7		8		9		C
//	LO4		*		0		#		D
var eventFreq = [16][2]tones.Hz{
	code1: {dtmfLow1, dtmfHigh1},
	code2: {dtmfLow1, dtmfHigh2},
	code3: {dtmfLow1, dtmfHigh3},
	codeA: {dtmfLow1, dtmfHigh4},

	code4: {dtmfLow2, dtmfHigh1},
	code5: {dtmfLow2, dtmfHigh2},
	code6: {dtmfLow2, dtmfHigh3},
	codeB: {dtmfLow2, dtmfHigh4},

	code7: {dtmfLow3, dtmfHigh1},
	code8: {dtmfLow3, dtmfHigh2},
	code9: {dtmfLow3, dtmfHigh3},
	codeC: {dtmfLow3, dtmfHigh4},

	codeStar: {dtmfLow4, dtmfHigh1},
	code0:    {dtmfLow4, dtmfHigh2},
	codeHash: {dtmfLow4, dtmfHigh3},
	codeD:    {dtmfLow4, dtmfHigh4},
}

// Tone returns DTMF tone frequencies for a given digit.
func Tone(digit byte) (code byte, freq []tones.Hz) {
	code, ok := charToEvent[digit]
	if !ok || int(code) > len(eventFreq) {
		return 0, nil
	}
	f := eventFreq[code]
	return code, f[:]
}

var eventToChar = [16]byte{
	code0: '0', code1: '1', code2: '2', code3: '3', code4: '4',
	code5: '5', code6: '6', code7: '7', code8: '8', code9: '9',
	codeStar: '*', codeHash: '#',
	codeA: 'a', codeB: 'b', codeC: 'c', codeD: 'd',
}

var charToEvent = map[byte]byte{
	'0': code0, '1': code1, '2': code2, '3': code3, '4': code4,
	'5': code5, '6': code6, '7': code7, '8': code8, '9': code9,
	'*': codeStar, '#': codeHash,
	'a': codeA, 'b': codeB, 'c': codeC, 'd': codeD,
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
	digit := byte(0)
	if int(ev) < len(eventToChar) {
		digit = eventToChar[ev]
	}
	return Event{
		Code:   ev,
		Digit:  digit,
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

// Write in-band (analog) and off-band (digital) DTMF tones to audio and RTP streams respectively.
//
// Digits may contain a special character 'w' which adds a 0.5 sec delay.
func Write(ctx context.Context, audio media.Writer[media.PCM16Sample], events *rtp.Stream, digits string) error {
	const framesPerSec = int(time.Second / rtp.DefFrameDur)
	var (
		buf    [4]byte
		pcmBuf media.PCM16Sample
	)
	if audio != nil {
		pcmBuf = make(media.PCM16Sample, audio.SampleRate()/framesPerSec)
	}

	const step = rtp.DefFrameDur
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	var (
		ts        time.Duration
		code      = byte(0xff)
		freq      []tones.Hz
		nextDelay time.Duration
		totalDur  time.Duration
		remaining time.Duration
	)
	setDelay := func(dt time.Duration) {
		code, freq = 0xff, nil
		remaining = dt
		totalDur = dt
		nextDelay = 0
		if events != nil {
			events.Delay(uint32(dt / (time.Second / SampleRate)))
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		// current tone/delay ended
		if remaining <= 0 {
			if nextDelay != 0 {
				// play delay after a digit
				setDelay(nextDelay)
			} else {
				// pick the next digit
				if len(digits) == 0 {
					return nil
				}
				b := digits[0]
				digits = digits[1:]
				if b == 'w' {
					setDelay(delayDur)
				} else {
					code, freq = Tone(b)
					remaining = eventDur
					nextDelay = eventDur
					totalDur = remaining
				}
			}
		}
		// generate audio tones or silence
		if audio != nil {
			if len(freq) == 0 {
				pcmBuf.Clear() // silence
			} else {
				tones.Generate(pcmBuf, ts, step, toneVolume, freq)
			}
			if err := audio.WriteSample(pcmBuf); err != nil {
				return err
			}
		}
		// generate telephony events
		if events != nil && len(freq) != 0 {
			dur := step + totalDur - remaining
			first := totalDur == remaining
			end := remaining-step <= 0

			n, err := Encode(buf[:], Event{
				Code:   code,
				Volume: eventVolume,
				Dur:    uint16(dur / (time.Second / SampleRate)),
				End:    end,
			})
			if err != nil {
				return err
			}
			// all packets for a digit must be sent with the same timestamp
			err = events.WritePayloadAtCurrent(buf[:n], first)
			if err != nil {
				return err
			}
			if end {
				// must repeat edn event 3 times, as per RFC
				if err = events.WritePayloadAtCurrent(buf[:n], first); err != nil {
					return err
				}
				if err = events.WritePayloadAtCurrent(buf[:n], first); err != nil {
					return err
				}
				// advance the timestamp now
				events.Delay(uint32(totalDur / (time.Second / SampleRate)))
			}
		}
		remaining -= step
		ts += step
	}
}

type Writer interface {
	WriteDTMF(ctx context.Context, ev string) error
}

type Handler func(ev Event)
