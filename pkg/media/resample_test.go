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

package media_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/webm"
	"github.com/livekit/sip/res"
	"github.com/livekit/sip/res/testdata"
)

type paceWriter struct {
	w       media.PCM16Writer
	mu      sync.Mutex
	cur     int
	frameT  []int
	frameSz []int
	samples int
}

func (p *paceWriter) Set(i int) {
	p.mu.Lock()
	p.cur = i
	p.mu.Unlock()
}

func (p *paceWriter) String() string {
	return fmt.Sprintf("paceWriter(%d) -> %s", p.cur, p.w.String())
}

func (p *paceWriter) SampleRate() int {
	return p.w.SampleRate()
}

func (p *paceWriter) WriteSample(sample media.PCM16Sample) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frameT = append(p.frameT, p.cur)
	p.frameSz = append(p.frameSz, len(sample))
	p.samples += len(sample)
	return p.w.WriteSample(sample)
}

func (p *paceWriter) Close() error {
	return nil
}

func TestResample(t *testing.T) {
	const (
		srcFile     = "resample.src.s16le"
		srcFileWebm = "resample.src.mka"
		srcRate     = res.SampleRate
	)
	srcFrames := res.ReadOggAudioFile(testdata.TestAudioOgg)
	gotSrc := writePCM16s(t, srcFile, srcFrames)
	writePCM16sWebm(t, srcFileWebm, srcRate, srcFrames)
	require.Equal(t, "c774af95", gotSrc)

	for _, c := range []struct {
		Rate   int
		Skip   int
		Buffer int
		Bumps  []int
	}{
		{
			Rate: 16000, Skip: 0},
		{
			Rate: 8000, Skip: 2,
			Buffer: 3,
			// TODO: figure out why resampler delays that specific frame; silence?
			Bumps: []int{211},
		},
	} {
		t.Run(strconv.Itoa(c.Rate), func(t *testing.T) {
			resPref := fmt.Sprintf("resample.out.%d", c.Rate)
			resFile := resPref + ".s16le"
			resFileWebm := resPref + ".mka"

			var dstFrames []media.PCM16Sample
			dst := media.NewPCM16FrameWriter(&dstFrames, c.Rate)
			pw := &paceWriter{w: dst}
			r := media.ResampleWriter(pw, srcRate)
			defer r.Close()
			totalSamples := 0
			for i, src := range srcFrames {
				pw.Set(i)
				err := r.WriteSample(src)
				require.NoError(t, err)
				totalSamples += len(src)
			}
			r.Close()
			gotDst := writePCM16s(t, resFile, dstFrames)
			// only change if you validated the quality!
			// require.Equal(t, "???", gotDst) // TODO: resampler is numerically unstable
			_ = gotDst

			writePCM16sWebm(t, resFileWebm, c.Rate, dstFrames)

			expSamples := totalSamples / (srcRate / c.Rate)
			require.Equal(t, expSamples, pw.samples)
			require.Equal(t, len(srcFrames), len(pw.frameT))
			skipped := pw.frameT[0]
			require.Equal(t, c.Skip, skipped)
			corr := 0
			for i, num := range pw.frameT {
				if i >= len(pw.frameT)-c.Buffer {
					break
				}
				exp := skipped + i + corr
				for _, b := range c.Bumps {
					if b == exp {
						corr++
						exp++
						break
					}
				}
				if exp != num {
					corr = num - (skipped + i)
					t.Errorf("skipped frame: exp %d, got %d", exp, num)
				}
			}
		})
	}

}

func writePCM16s(t testing.TB, path string, buf []media.PCM16Sample) string {
	var out []byte
	for _, frame := range buf {
		for _, v := range frame {
			var b [2]byte
			binary.LittleEndian.PutUint16(b[:], uint16(v))
			out = append(out, b[:]...)
		}
	}
	err := os.WriteFile(path, out, 0644)
	require.NoError(t, err)
	h := sha256.Sum256(out)
	return hex.EncodeToString(h[:])[:8]
}

func writePCM16sWebm(t testing.TB, path string, rate int, buf []media.PCM16Sample) {
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := webm.NewPCM16Writer(f, rate, media.DefFrameDur)
	defer w.Close()

	for _, frame := range buf {
		err = w.WriteSample(frame)
		require.NoError(t, err)
	}
}
