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

package webm

import (
	"fmt"
	"io"
	"math/rand"
	"slices"
	"time"

	"github.com/at-wat/ebml-go/webm"

	"github.com/livekit/sip/pkg/media"
)

func NewPCM16Writer(w io.WriteCloser, sampleRate int, channels int, sampleDur time.Duration) media.PCM16Writer {
	return NewWriter[media.PCM16Sample](w, "A_PCM/INT/LIT", channels, sampleRate, sampleDur)
}

func NewWriter[T media.Frame](w io.WriteCloser, codec string, channels, sampleRate int, sampleDur time.Duration) media.WriteCloser[T] {
	ws, err := webm.NewSimpleBlockWriter(w, []webm.TrackEntry{
		{
			Name:            "Audio",
			TrackNumber:     1,
			TrackUID:        rand.Uint64(),
			CodecID:         codec,
			TrackType:       2,
			DefaultDuration: uint64(sampleDur.Nanoseconds()),
			Audio: &webm.Audio{
				SamplingFrequency: float64(sampleRate),
				Channels:          uint64(channels),
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return &writer[T]{codec: codec, ws: ws[0], channels: channels, sampleRate: sampleRate, dur: sampleDur}
}

type writer[T media.Frame] struct {
	codec      string
	ws         webm.BlockWriteCloser
	channels   int
	sampleRate int
	dur        time.Duration
	ts         int64
	buf        []byte
}

func (w *writer[T]) String() string {
	return fmt.Sprintf("WEBM(%s,%d,%d)", w.codec, w.channels, w.sampleRate)
}

func (w *writer[T]) SampleRate() int {
	return w.sampleRate
}

func (w *writer[T]) WriteSample(sample T) error {
	if sz := sample.Size(); cap(w.buf) < sz {
		w.buf = make([]byte, sz)
	} else {
		w.buf = w.buf[:sz]
	}
	n, err := sample.CopyTo(w.buf)
	if err != nil {
		return err
	}
	_, err = w.ws.Write(true, w.ts, slices.Clone(w.buf[:n]))
	w.ts += w.dur.Milliseconds()
	return err
}

func (w *writer[T]) Close() error {
	return w.ws.Close()
}
