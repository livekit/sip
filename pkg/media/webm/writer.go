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
	"encoding/binary"
	"io"
	"math/rand"
	"time"

	"github.com/at-wat/ebml-go/webm"

	"github.com/livekit/sip/pkg/media"
)

func NewPCM16Writer(w io.WriteCloser, sampleRate float64, sampleDur time.Duration) media.PCM16WriteCloser {
	ws, err := webm.NewSimpleBlockWriter(w, []webm.TrackEntry{
		{
			Name:            "Audio",
			TrackNumber:     1,
			TrackUID:        rand.Uint64(),
			CodecID:         "A_PCM/INT/LIT",
			TrackType:       2,
			DefaultDuration: uint64(sampleDur.Nanoseconds()),
			Audio: &webm.Audio{
				SamplingFrequency: sampleRate,
				Channels:          1,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return &writerPCM16{ws: ws[0], dur: sampleDur}
}

type writerPCM16 struct {
	ws  webm.BlockWriteCloser
	dur time.Duration
	ts  int64
	buf []byte
}

func (w *writerPCM16) WriteSample(sample media.PCM16Sample) error {
	if sz := len(sample) * 2; cap(w.buf) < sz {
		w.buf = make([]byte, sz)
	} else {
		w.buf = w.buf[:sz]
	}
	for i, v := range sample {
		binary.LittleEndian.PutUint16(w.buf[2*i:], uint16(v))
	}
	_, err := w.ws.Write(true, w.ts, w.buf)
	w.ts += w.dur.Milliseconds()
	return err
}

func (w *writerPCM16) Close() error {
	return w.ws.Close()
}
