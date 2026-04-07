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

package opus

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"gopkg.in/hraban/opus.v2"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/webm"
	"github.com/livekit/protocol/logger"
)

type Sample []byte

func (s Sample) Size() int {
	return len(s)
}

func (s Sample) CopyTo(dst []byte) (int, error) {
	if len(dst) < len(s) {
		return 0, io.ErrShortBuffer
	}
	n := copy(dst, s)
	return n, nil
}

type Writer = msdk.WriteCloser[Sample]

type params struct {
	SampleRate int
	Channels   int
}

func Decode(w msdk.PCM16Writer, channels int, log logger.Logger) (Writer, error) {
	rate := w.SampleRate()
	p := params{SampleRate: rate, Channels: channels}
	log = log.WithValues("params", p)
	dec, err := opus.NewDecoder(rate, channels)
	if err != nil {
		log.Errorw("cannot initialize opus decoder", err)
		return nil, err
	}
	return &decoder{
		w:   w,
		p:   p,
		dec: dec,
		buf: make([]int16, w.SampleRate()/rtp.DefFramesPerSec),
		log: log,
	}, nil
}

func Encode(w Writer, channels int, log logger.Logger) (msdk.PCM16Writer, error) {
	rate := w.SampleRate()
	p := params{SampleRate: rate, Channels: channels}
	log = log.WithValues("params", p)
	enc, err := opus.NewEncoder(rate, channels, opus.AppVoIP)
	if err != nil {
		log.Errorw("cannot initialize opus encoder", err)
		return nil, err
	}
	samples := w.SampleRate() / rtp.DefFramesPerSec
	return &encoder{
		w:       w,
		p:       p,
		enc:     enc,
		samples: samples,
		inbuf:   make(msdk.PCM16Sample, 0, samples),
		buf:     make([]byte, 4*channels*samples),
		log:     log,
	}, nil
}

type decoder struct {
	log logger.Logger
	p   params

	mu  sync.Mutex
	w   msdk.PCM16Writer
	dec *opus.Decoder
	buf msdk.PCM16Sample

	successiveErrorCount int
}

func (d *decoder) String() string {
	return fmt.Sprintf("OPUS(decode) -> %s", d.w)
}

func (d *decoder) SampleRate() int {
	return d.w.SampleRate()
}

func (d *decoder) WriteSample(in Sample) error {
	if len(in) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n, err := d.dec.Decode(in, d.buf)
	if err != nil {
		// Some workflows (concatenating opus files) can cause a suprious decoding error, so ignore small amount of corruption errors
		if !errors.Is(err, opus.ErrInvalidPacket) || d.successiveErrorCount >= 5 {
			d.log.Warnw("error decoding opus sample", err)
			return err
		}
		d.log.Debugw("opus decoder failed decoding a sample", "error", err)
		d.successiveErrorCount++
		return nil
	}
	d.successiveErrorCount = 0
	return d.w.WriteSample(d.buf[:n])
}

func (d *decoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w.Close()
}

type encoder struct {
	log     logger.Logger
	p       params
	samples int

	mu    sync.Mutex
	w     Writer
	enc   *opus.Encoder
	inbuf msdk.PCM16Sample
	buf   Sample

	successiveErrorCount int
}

func (e *encoder) String() string {
	return fmt.Sprintf("OPUS(encode) -> %s", e.w)
}

func (e *encoder) SampleRate() int {
	return e.w.SampleRate()
}

func (e *encoder) WriteSample(in msdk.PCM16Sample) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inbuf = append(e.inbuf, in...)
	for len(e.inbuf) >= e.samples {
		i := e.samples

		n, err := e.enc.Encode(e.inbuf[:i], e.buf)

		sz := copy(e.inbuf, e.inbuf[i:])
		e.inbuf = e.inbuf[:sz]
		if err != nil {
			if e.successiveErrorCount < 5 {
				e.log.Errorw("error encoding opus sample", err, "len", len(in), "buf", len(e.buf), "n", n)
				e.successiveErrorCount++
			}
			return err
		}
		e.successiveErrorCount = 0
		if err = e.w.WriteSample(e.buf[:n]); err != nil {
			return err
		}
	}
	return nil
}

func (e *encoder) flush() error {
	if len(e.inbuf) == 0 {
		return nil
	}
	n, err := e.enc.Encode(e.inbuf, e.buf)
	if err != nil {
		return err
	}
	return e.w.WriteSample(e.buf[:n])
}

func (e *encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err1 := e.flush()
	err2 := e.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func NewWebmWriter(w io.WriteCloser, sampleRate int, sampleDur time.Duration) msdk.WriteCloser[Sample] {
	return webm.NewWriter[Sample](w, "A_OPUS", 2, sampleRate, sampleDur)
}
