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
	"fmt"
	"os"
	"sync/atomic"
)

var (
	resampleID         atomic.Uint32
	resampleDumpToFile = os.Getenv("LK_DUMP_RESAMPLE") == "true"
)

// Resample the source sample into the destination sample rate.
// It appends resulting samples to dst and returns the result.
func Resample(dst PCM16Sample, dstSampleRate int, src PCM16Sample, srcSampleRate int) PCM16Sample {
	if dstSampleRate == srcSampleRate {
		return append(dst, src...)
	}
	return resampleBuffer(dst, dstSampleRate, src, srcSampleRate)
}

// ResampleWriter returns a new writer that expects samples of a given sample rate
// and resamples then for the destination writer.
func ResampleWriter(w PCM16Writer, sampleRate int) (w2 PCM16Writer) {
	srcRate := sampleRate
	dstRate := w.SampleRate()
	if dstRate == srcRate {
		return w
	}

	if resampleDumpToFile {
		id := resampleID.Add(1)
		pref := fmt.Sprintf("sip_resample_%d", id)
		w = DumpWriterPCM16(pref+"_out", w)
		defer func() {
			w2 = DumpWriterPCM16(pref+"_in", w2)
		}()
	}
	return newResampleWriter(w, sampleRate)
}
