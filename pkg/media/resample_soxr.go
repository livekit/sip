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

/*
#cgo pkg-config: soxr
#include <stdlib.h>
#include <soxr.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
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
	w := newResampleWriter(NewPCM16BufferWriter(&dst, dstSampleRate), srcSampleRate)
	err := w.WriteSample(src)
	_ = w.Close()
	if err != nil {
		panic(err)
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
		buffer:  0, // set larger buffer for better resampler quality (see below)
	}
	quality := int(C.SOXR_QQ)
	if srcRate > dstRate {
		if float64(srcRate)/float64(dstRate) > 3 {
			quality = int(C.SOXR_LQ)
		}
	}
	var err error
	r.r, err = newSoxr(dstRate, srcRate, quality)
	if err != nil {
		panic(err)
	}
	return r
}

type resampleWriter struct {
	mu       sync.Mutex
	w        WriteCloser[PCM16Sample]
	r        *soxrResampler
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
	// Flush soxr buffer to our buffer.
	var err error
	w.buf, _, err = w.r.Resample(w.buf, nil)
	if err != nil {
		return err
	}
	// Close soxr resampler.
	_ = w.r.Close()
	// Flush our own PCM frame buffer to the underlying writer.
	_ = w.flush(0)
	err2 := w.w.Close()
	if err2 != nil {
		err = err2
	}
	return err
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
	var err error
	// Write input to the resampler buffer.
	w.buf, _, err = w.r.Resample(w.buf, data)
	if err != nil {
		return err
	}
	// Resampler will likely return a short buffer in the first run. In that case, we emit no samples on the first call.
	// This will cause a one frame delay for each resampler. Flushing the sampler, however will lead to frame
	// discontinuity, and thus - distortions on the frame boundaries.
	dstFrame := resampleSize(w.dstRate, w.srcRate, len(data))
	w.dstFrame = max(w.dstFrame, dstFrame)
	return w.flush(w.dstFrame * (1 + w.buffer))
}

type soxrResampler struct {
	ptr     C.soxr_t
	srcRate int
	dstRate int
	maxIn   int
	done    *atomic.Bool
}

func soxrErr(e C.soxr_error_t) error {
	if e == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(e))
	estr := C.GoString(e)
	switch estr {
	case "", "0":
		return nil
	}
	return errors.New(estr)
}

func newSoxr(dstRate, srcRate int, quality int) (*soxrResampler, error) {
	ic := C.soxr_io_spec(C.SOXR_INT16_I, C.SOXR_INT16_I)
	qc := C.soxr_quality_spec(C.ulong(quality), 0)
	rc := C.soxr_runtime_spec(1) // 1 thread
	var e C.soxr_error_t
	p := C.soxr_create(C.double(srcRate), C.double(dstRate), 1, &e, &ic, &qc, &rc)
	err := soxrErr(e)
	if err != nil {
		return nil, err
	}
	// This variable helps avoid double-free on the soxr resampler ptr. See soxrCleanup.
	done := new(atomic.Bool)
	r := &soxrResampler{
		ptr:     p,
		dstRate: dstRate,
		srcRate: srcRate,
		done:    done,
	}
	runtime.AddCleanup(r, func(p C.soxr_t) {
		soxrCleanup(done, p)
	}, p)
	return r, nil
}

func soxrCleanup(done *atomic.Bool, p C.soxr_t) {
	if done.CompareAndSwap(false, true) {
		C.soxr_delete(p)
	}
}

func (r *soxrResampler) Close() error {
	if r.ptr == nil {
		return nil
	}
	soxrCleanup(r.done, r.ptr)
	r.ptr = nil
	return nil
}

func (r *soxrResampler) Resample(out PCM16Sample, in PCM16Sample) (PCM16Sample, int, error) {
	if r.ptr == nil || r.done.Load() {
		return out, 0, errors.New("resampler is closed")
	}
	r.maxIn = max(r.maxIn, len(in))
	dstN := (len(in) * r.dstRate) / r.srcRate
	if dstN == 0 {
		dstN = max(
			(r.maxIn*r.dstRate)/r.srcRate,
			cap(out)-len(out),
			1024,
		)
	}
	// Make sure output has space for new samples. Length is still unchanged.
	out = slices.Grow(out, dstN)
	// Slice for the unused capacity, which we will write into.
	dst := out[len(out) : len(out)+dstN]
	total := 0
	// Always call at least once (for flush to work), thus not considering len(in) here.
	for len(dst) > 0 {
		var read, done C.size_t
		var e C.soxr_error_t
		if len(in) != 0 {
			e = C.soxr_process(r.ptr, C.soxr_in_t(unsafe.Pointer(&in[0])), C.size_t(len(in)), &read, C.soxr_out_t(unsafe.Pointer(&dst[0])), C.size_t(len(dst)), &done)
		} else {
			// Flush, no input.
			e = C.soxr_process(r.ptr, nil, 0, nil, C.soxr_out_t(unsafe.Pointer(&dst[0])), C.size_t(len(dst)), &done)
			read = 0
		}
		err := soxrErr(e)
		if err != nil {
			return out, 0, err
		}
		total += int(done)
		dst = dst[done:]
		in = in[read:]
		if len(in) == 0 {
			break
		}
	}
	// Finally adjust the length to cover written data.
	out = out[:len(out)+total]
	return out, total, nil
}
