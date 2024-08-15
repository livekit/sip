//go:build !cgo

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

const quality = 3

func resampleBuffer(dst PCM16Sample, dstSampleRate int, src PCM16Sample, srcSampleRate int) PCM16Sample {
	r := beepResample(quality, srcSampleRate, dstSampleRate, NewPCM16BufferReader(src))
	sz := 0
	if dstSampleRate < srcSampleRate {
		div := srcSampleRate / dstSampleRate
		sz = len(src) / div
	} else {
		mul := dstSampleRate / srcSampleRate
		sz = len(src) * mul
	}
	out := make(PCM16Sample, sz)
	n, _ := r.Stream(out)
	return append(dst, out[:n]...)
}

func newResampleWriter(w WriteCloser[PCM16Sample], sampleRate int) WriteCloser[PCM16Sample] {
	srcRate := sampleRate
	dstRate := w.SampleRate()
	r := &resampleWriter{
		w:       w,
		srcRate: srcRate,
		dstRate: dstRate,
	}
	r.r = beepResample(quality, srcRate, dstRate, r)
	return r
}

type resampleWriter struct {
	w       WriteCloser[PCM16Sample]
	r       *beepResampler
	inbuf   PCM16Sample
	srcRate int
	dstRate int
	buf     PCM16Sample
}

func (w *resampleWriter) SampleRate() int {
	return w.srcRate
}

func (w *resampleWriter) Close() error {
	return w.w.Close()
}

func (w *resampleWriter) ReadSample(data PCM16Sample) (int, error) {
	n := copy(data, w.inbuf)
	w.inbuf = w.inbuf[n:]
	return n, nil
}

func (w *resampleWriter) WriteSample(data PCM16Sample) error {
	w.inbuf = append(w.inbuf, data...)
	var sz int
	if w.srcRate > w.dstRate {
		sz = len(data) / (w.srcRate / w.dstRate)
	} else {
		sz = len(data) * (w.dstRate / w.srcRate)
	}
	if cap(w.buf) < sz {
		w.buf = make(PCM16Sample, sz)
	} else {
		w.buf = w.buf[:sz]
	}
	n, _ := w.r.Stream(w.buf)
	return w.w.WriteSample(w.buf[:n])
}
