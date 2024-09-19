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
	"fmt"
	"sync"

	"github.com/zaf/resample"
)

func resampleSize(dstSampleRate, srcSampleRate int, srcSize int) int {
	if dstSampleRate < srcSampleRate {
		div := srcSampleRate / dstSampleRate
		sz := srcSize / div
		return sz
	}
	mul := dstSampleRate / srcSampleRate
	sz := srcSize * mul
	return sz
}

func resampleBuffer(dst PCM16Sample, dstSampleRate int, src PCM16Sample, srcSampleRate int) PCM16Sample {
	inbuf := make([]byte, len(src)*2)
	outbuf := bytes.NewBuffer(nil)
	outbuf.Grow(resampleSize(dstSampleRate, srcSampleRate, len(src)))
	r, err := resample.New(outbuf, float64(srcSampleRate), float64(dstSampleRate), 1, resample.I16, resample.Quick)
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
		buffer:  0, // set larger buffer for better resampler quality
	}
	quality := resample.Quick
	if srcRate > dstRate {
		if float64(srcRate)/float64(dstRate) > 3 {
			quality = resample.LowQ
		}
	}
	var err error
	r.r, err = resample.New(r, float64(srcRate), float64(dstRate), 1, resample.I16, quality)
	if err != nil {
		panic(err)
	}
	return r
}

type resampleWriter struct {
	mu       sync.Mutex
	w        WriteCloser[PCM16Sample]
	r        *resample.Resampler
	inbuf    []byte
	srcRate  int
	dstRate  int
	dstFrame int

	// The resampler could actually consume multiple full frames and emit just one.
	// This variable controls how many full frames we intentionally keep. Useful for higher resampler quality.
	buffer int
	buf    PCM16Sample
}

func (w *resampleWriter) String() string {
	return fmt.Sprintf("Resample(%d->%d) -> %s", w.srcRate, w.dstRate, w.w.String())
}

func (w *resampleWriter) SampleRate() int {
	return w.srcRate
}

func (w *resampleWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.r.Close() // flush resampler's internal buffer
	_ = w.flush(0)  // flush our own PCM frame buffer
	return w.w.Close()
}

func (w *resampleWriter) flush(minSize int) error {
	if len(w.buf) == 0 {
		return nil
	}
	frame := w.dstFrame
	if frame == 0 {
		frame = len(w.buf)
	}
	var last error
	for len(w.buf) > 0 && len(w.buf) >= minSize {
		sz := frame
		if sz > len(w.buf) {
			sz = len(w.buf)
		}
		if err := w.w.WriteSample(w.buf[:sz]); err != nil {
			last = err
		}
		n := copy(w.buf, w.buf[sz:])
		w.buf = w.buf[:n]
	}
	return last
}

func (w *resampleWriter) WriteSample(data PCM16Sample) error {
	if len(data) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if sz := len(data) * 2; cap(w.inbuf) < sz {
		w.inbuf = make([]byte, sz)
	} else {
		w.inbuf = w.inbuf[:sz]
	}
	for i, v := range data {
		binary.LittleEndian.PutUint16(w.inbuf[i*2:], uint16(v))
	}
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
	// Resampler will likely return a short buffer in the first run. In that case, we emit no samples on the first call.
	// This will cause a one frame delay for each resampler. Flushing the sampler, however will lead to frame
	// discontinuity, and thus - distortions on the frame boundaries.

	dstFrame := resampleSize(w.dstRate, w.srcRate, len(data))
	w.dstFrame = max(w.dstFrame, dstFrame)
	return w.flush(w.dstFrame * (1 + w.buffer))
}

func (w *resampleWriter) Write(data []byte) (int, error) {
	for i := range len(data) / 2 {
		v := int16(binary.LittleEndian.Uint16(data[i*2:]))
		w.buf = append(w.buf, v)
	}
	return len(data), nil
}
