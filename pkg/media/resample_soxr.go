//go:build cgo

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

package media

import (
	"bytes"
	"encoding/binary"

	"github.com/zaf/resample"
)

const quality = resample.MediumQ

func resampleBuffer(dst PCM16Sample, dstSampleRate int, src PCM16Sample, srcSampleRate int) PCM16Sample {
	inbuf := make([]byte, len(src)*2)
	outbuf := bytes.NewBuffer(nil)
	r, err := resample.New(outbuf, float64(srcSampleRate), float64(dstSampleRate), 1, resample.I16, quality)
	if err != nil {
		panic(err)
	}
	for i, v := range src {
		binary.LittleEndian.PutUint16(inbuf[i*2:], uint16(v))
	}
	_, err = r.Write(inbuf)
	_ = r.Close()
	if err != nil {
		panic(err)
	}
	buf := outbuf.Bytes()
	for i := range len(buf) / 2 {
		v := int16(binary.LittleEndian.Uint16(buf[i*2:]))
		dst = append(dst, v)
	}
	return dst
}

func newResampleWriter(w WriteCloser[PCM16Sample], sampleRate int) WriteCloser[PCM16Sample] {
	srcRate := sampleRate
	dstRate := w.SampleRate()
	r := &resampleWriter{
		w:       w,
		srcRate: srcRate,
		dstRate: dstRate,
	}
	var err error
	r.r, err = resample.New(r, float64(srcRate), float64(dstRate), 1, resample.I16, quality)
	if err != nil {
		panic(err)
	}
	return r
}

type resampleWriter struct {
	w       WriteCloser[PCM16Sample]
	r       *resample.Resampler
	inbuf   []byte
	srcRate int
	dstRate int
	buf     PCM16Sample
}

func (w *resampleWriter) SampleRate() int {
	return w.srcRate
}

func (w *resampleWriter) Close() error {
	_ = w.r.Close()
	return w.w.Close()
}

func (w *resampleWriter) WriteSample(data PCM16Sample) error {
	if sz := len(data) * 2; cap(w.inbuf) < sz {
		w.inbuf = make([]byte, sz)
	} else {
		w.inbuf = w.inbuf[:sz]
	}
	for i, v := range data {
		binary.LittleEndian.PutUint16(w.inbuf[i*2:], uint16(v))
	}
	w.buf = w.buf[:0]
	left := w.inbuf
	// Write converted input to the resampler's buffer.
	// It will call our own Write method which collects data into a frame buffer.
	for len(left) > 0 {
		n, err := w.r.Write(left)
		if err != nil {
			return err
		}
		left = left[n:]
	}
	// Flush the resampler. Otherwise it returns short buffers.
	if err := w.r.Reset(w); err != nil {
		return err
	}
	// Now we have the full frame buffer that we can write back.
	return w.w.WriteSample(w.buf)
}

func (w *resampleWriter) Write(data []byte) (int, error) {
	for i := range len(data) / 2 {
		v := int16(binary.LittleEndian.Uint16(data[i*2:]))
		w.buf = append(w.buf, v)
	}
	return len(data), nil
}
