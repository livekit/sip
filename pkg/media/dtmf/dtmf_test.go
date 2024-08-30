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

package dtmf

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media/rtp"
)

func TestDTMF(t *testing.T) {
	cases := []struct {
		name string
		data string
		exp  Event
	}{
		{
			name: "star end",
			data: `0a8a0820`,
			exp: Event{
				Code:   10,
				Digit:  '*',
				Volume: 10,
				End:    true,
				Dur:    2080,
			},
		},
		{
			name: "four",
			data: `040a0140`,
			exp: Event{
				Code:   4,
				Digit:  '4',
				Volume: 10,
				End:    false,
				Dur:    320,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			data, err := hex.DecodeString(c.data)
			require.NoError(t, err)

			got, err := Decode(data)
			require.NoError(t, err)
			require.Equal(t, c.exp, got)

			var buf [4]byte
			n, err := Encode(buf[:], got)
			require.NoError(t, err)
			require.Equal(t, len(data), n)
			require.Equal(t, c.data, hex.EncodeToString(buf[:n]))
		})
	}
}

func TestDTMFDelay(t *testing.T) {
	var buf rtp.Buffer
	w := rtp.NewSeqWriter(&buf).NewStream(101, SampleRate)
	err := Write(context.Background(), nil, w, startTime, "1w23")
	require.NoError(t, err)

	type packet struct {
		SequenceNumber uint16
		Timestamp      uint32
		Marker         bool
		Event
	}
	var (
		exp []packet
		seq uint16
		ts  uint32
	)
	const (
		packetDur = uint32(SampleRate / int(time.Second/rtp.DefFrameDur))
		startTime = 1242
	)

	ts = startTime

	expectDigit := func(code byte, digit byte) {
		start := ts
		const n = 13
		for i := 0; i < n-1; i++ {
			exp = append(exp, packet{
				SequenceNumber: seq,
				Timestamp:      start, // should be the same for all events
				Marker:         i == 0,
				Event: Event{
					Code:   code,
					Digit:  digit,
					Volume: eventVolume,
					Dur:    uint16(i+1) * uint16(packetDur),
					End:    false,
				},
			})
			ts += packetDur
			seq++
		}
		// end event must be sent 3 times with the same duration
		for i := 0; i < 3; i++ {
			exp = append(exp, packet{
				SequenceNumber: seq,
				Timestamp:      start, // should be the same for all events
				Marker:         false,
				Event: Event{
					Code:   code,
					Digit:  digit,
					Volume: eventVolume,
					Dur:    uint16(n) * uint16(packetDur),
					End:    true,
				},
			})
			seq++
		}
		ts += packetDur
		// delay between digits
		ts += uint32(eventDur / (time.Second / SampleRate))
		// rounding error (12.5 events in a sec)
		ts -= packetDur / 2
	}
	expectDigit(1, '1')
	ts += SampleRate / 2 // 500ms delay
	expectDigit(2, '2')
	expectDigit(3, '3')
	var got []packet
	for _, p := range buf {
		e, err := Decode(p.Payload)
		require.NoError(t, err)
		got = append(got, packet{
			SequenceNumber: p.SequenceNumber,
			Timestamp:      p.Timestamp,
			Marker:         p.Marker,
			Event:          e,
		})
	}
	require.Equal(t, exp, got)
}
