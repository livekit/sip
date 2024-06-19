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

// Resample the source sample into the destination sample rate.
// It appends resulting samples to dst and returns the result.
func Resample(dst PCM16Sample, dstSampleRate int, src PCM16Sample, srcSampleRate int) PCM16Sample {
	if dstSampleRate == srcSampleRate {
		return append(dst, src...)
	}
	r := beepResample(3, srcSampleRate, dstSampleRate, NewPCM16BufferReader(src))
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

// ResampleWriter returns a new writer that expects samples of a given sample rate
// and resamples then for the destination writer.
func ResampleWriter(w Writer[PCM16Sample], sampleRate int) Writer[PCM16Sample] {
	srcRate := sampleRate
	dstRate := w.SampleRate()
	if dstRate == srcRate {
		return w
	}
	r := &resampleWriter{
		w:       w,
		srcRate: srcRate,
		dstRate: dstRate,
	}
	r.r = beepResample(3, srcRate, dstRate, r)
	return r
}

type resampleWriter struct {
	w       Writer[PCM16Sample]
	r       *beepResampler
	inbuf   PCM16Sample
	srcRate int
	dstRate int
	buf     PCM16Sample
}

func (w *resampleWriter) SampleRate() int {
	return w.srcRate
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
