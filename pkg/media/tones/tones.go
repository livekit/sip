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

package tones

import (
	"context"
	"math"
	"time"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
)

type Hz uint32

func Generate(buf media.PCM16Sample, ts, dur time.Duration, amp int16, freq []Hz) time.Duration {
	for i := range buf {
		phi := ts + (dur*time.Duration(i))/time.Duration(len(buf))
		if len(freq) == 0 {
			buf[i] = 0
		} else {
			var sum float64
			for _, hz := range freq {
				ph := (phi * time.Duration(hz) * 2).Seconds() * math.Pi
				sum += math.Sin(ph)
			}
			buf[i] = int16(float64(amp) * sum / float64(len(freq)))
		}
	}
	return ts + dur
}

type Tone struct {
	Freq    []Hz
	Dur     time.Duration
	Silence time.Duration
}

var (
	ETSIDial    = []Tone{{Freq: []Hz{425}}}
	ETSIRinging = []Tone{{Freq: []Hz{425}, Dur: time.Second, Silence: 4 * time.Second}}
	ETSIBusy    = []Tone{{Freq: []Hz{425}, Dur: time.Second / 2, Silence: time.Second / 2}}
)

// Play specified audio tones in a loop until the context is cancelled.
func Play(ctx context.Context, audio media.PCM16Writer, vol int16, tones []Tone) error {
	const (
		frameDur     = rtp.DefFrameDur
		framesPerSec = int(time.Second / frameDur)
	)
	pcmBuf := make(media.PCM16Sample, audio.SampleRate()/framesPerSec)

	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	var (
		ts        time.Duration
		freq      []Hz
		remaining time.Duration
		ind       = -1 // ind%2 is tone and silence, ind/2 is the index in tones
	)
	next := func() (t Tone, silence bool) {
		ind++
		ind %= len(tones) * 2
		return tones[ind/2], ind%2 != 0
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		// pick the next tone (or tone vs silence)
		if remaining <= 0 {
			t, silence := next()
			if silence && t.Silence == 0 {
				// no silence for this tone - continue to the next one
				t, silence = next()
			}
			if !silence {
				freq = t.Freq
				remaining = t.Dur
			} else {
				freq = nil
				remaining = t.Silence
			}
		}
		// generate audio tones or silence
		if len(freq) == 0 {
			pcmBuf.Clear() // silence
		} else {
			Generate(pcmBuf, ts, frameDur, vol, freq)
		}
		if err := audio.WriteSample(pcmBuf); err != nil {
			return err
		}
		remaining -= frameDur
		ts += frameDur
	}
}
