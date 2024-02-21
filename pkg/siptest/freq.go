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

package siptest

import (
	"math"
	"math/cmplx"
	"slices"

	"github.com/mjibson/go-dsp/fft"

	"github.com/livekit/sip/pkg/media"
)

type wave struct {
	Ind int
	Amp int
}

func genSignal(dst media.PCM16Sample, waves []wave) {
	for i := range dst {
		ifl := float64(i) / float64(len(dst))
		var v float64
		for _, w := range waves {
			v += float64(w.Amp) * math.Sin(ifl*2*math.Pi*(float64(int(1)<<w.Ind)))
		}
		dst[i] = int16(v)
	}
}

func findSignal(src media.PCM16Sample) []wave {
	cmp := make([]complex128, len(src))
	for i, v := range src {
		cmp[i] = complex(float64(v), 0)
	}
	out := fft.FFT(cmp)
	var waves []wave
	for i, v := range out[:len(out)/2] {
		if i == 0 {
			continue
		}
		a := 2 * cmplx.Abs(v) / float64(len(src))
		if a < 1 {
			continue
		}
		fi := int(math.Log2(float64(i)))
		waves = append(waves, wave{Ind: fi, Amp: int(math.Round(a + 0.5))})
	}
	slices.SortFunc(waves, func(a, b wave) int {
		return b.Amp - a.Amp
	})
	return waves
}
