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

package mixer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media"
)

type testMixer struct {
	t      testing.TB
	sample media.PCM16Sample
	*Mixer
}

func newTestMixer(t testing.TB) *testMixer {
	m := &testMixer{t: t}
	m.Mixer = newMixer(media.WriterFunc[media.PCM16Sample](func(s media.PCM16Sample) error {
		m.sample = s
		return nil
	}), 5)
	return m
}

func (m *testMixer) Expect(exp media.PCM16Sample, msgAndArgs ...any) {
	m.t.Helper()
	m.mixOnce()
	require.Equal(m.t, exp, m.sample, msgAndArgs...)
}

func WriteSampleN(inp *Input, i int) {
	v := int16(i) * 5
	inp.WriteSample(media.PCM16Sample{v + 0, v + 1, v + 2, v + 3, v + 4})
}

func (m *testMixer) ExpectSampleN(i int, msgAndArgs ...any) {
	m.t.Helper()
	m.mixOnce()
	m.CheckSampleN(i, msgAndArgs...)
}

func (m *testMixer) CheckSampleN(i int, msgAndArgs ...any) {
	m.t.Helper()
	v := int16(i) * 5
	require.Equal(m.t, media.PCM16Sample{v + 0, v + 1, v + 2, v + 3, v + 4}, m.sample, msgAndArgs...)
}

func TestMixer(t *testing.T) {
	t.Run("no input produces silence", func(t *testing.T) {
		m := newTestMixer(t)
		m.mixOnce()
		m.Expect(media.PCM16Sample{0, 0, 0, 0, 0})
	})

	t.Run("one input mixing correctly", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer m.RemoveInput(inp)
		inp.buffering = false

		WriteSampleN(inp, 1)
		m.ExpectSampleN(1)
	})

	t.Run("two inputs mixing correctly", func(t *testing.T) {
		m := newTestMixer(t)
		one := m.NewInput()
		defer m.RemoveInput(one)
		one.buffering = false
		one.WriteSample([]int16{0xE, 0xD, 0xC, 0xB, 0xA})

		two := m.NewInput()
		defer m.RemoveInput(two)
		two.buffering = false
		two.WriteSample([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.Expect(media.PCM16Sample{24, 24, 24, 24, 24})

		one.WriteSample([]int16{0x7FFF, 0x1, -0x7FFF, -0x1, 0x0})
		two.WriteSample([]int16{0x1, 0x7FFF, -0x1, -0x7FFF, 0x0})

		m.Expect(media.PCM16Sample{0x7FFF, 0x7FFF, -0x7FFF, -0x7FFF, 0x0})
	})

	t.Run("draining produces silence afterwards", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer m.RemoveInput(inp)

		for i := 0; i < inputBufferFrames; i++ {
			inp.WriteSample([]int16{0, 1, 2, 3, 4})
		}

		for i := 0; i < inputBufferFrames+3; i++ {
			expected := media.PCM16Sample{0, 1, 2, 3, 4}
			if i >= inputBufferFrames {
				expected = media.PCM16Sample{0, 0, 0, 0, 0}
			}
			m.Expect(expected, "i=%d", i)
		}
	})

	t.Run("drops frames on overflow", func(t *testing.T) {
		m := newTestMixer(t)
		input := m.NewInput()
		defer m.RemoveInput(input)

		for i := 0; i < inputBufferFrames+3; i++ {
			input.WriteSample([]int16{0, 1, 2, 3, 4})
		}

		m.mixOnce()
		require.Equal(t, (inputBufferFrames-1)*5, input.buf.Len())
	})

	t.Run("buffered initially and after starving", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer m.RemoveInput(inp)

		inp.WriteSample([]int16{10, 11, 12, 13, 14})

		for i := 0; i < inputBufferMin-1; i++ {
			// Mixing produces nothing, because we are buffering the input initially.
			m.Expect(media.PCM16Sample{0, 0, 0, 0, 0})
			WriteSampleN(inp, i)
		}

		// Now we should finally receive all our samples, even if no new data is available.
		m.Expect(media.PCM16Sample{10, 11, 12, 13, 14})
		for i := 0; i < inputBufferMin-1; i++ {
			m.ExpectSampleN(i)
		}

		// Input is starving, should produce silence again until we buffer enough samples.
		m.Expect(media.PCM16Sample{0, 0, 0, 0, 0})
		for i := 0; i < inputBufferMin; i++ {
			m.Expect(media.PCM16Sample{0, 0, 0, 0, 0})
			WriteSampleN(inp, i)
		}
		// Data is flowing again after we buffered enough.
		for i := 0; i < inputBufferMin; i++ {
			m.ExpectSampleN(i)
			// Keep writing to see if we can get this data later without gaps.
			WriteSampleN(inp, i*10)
		}
		// Check data that we were writing above.
		for i := 0; i < inputBufferMin; i++ {
			m.ExpectSampleN(i * 10)
		}
	})

	t.Run("catches up after not running for long", func(t *testing.T) {
		step := 20 * time.Millisecond
		m := newTestMixer(t)
		m.tickerDur = step

		inp := m.NewInput()
		defer m.RemoveInput(inp)

		for i := 0; i < inputBufferFrames; i++ {
			WriteSampleN(inp, i)
		}

		m.mixUpdate()
		require.EqualValues(t, 1, m.mixCnt)
		m.CheckSampleN(0)

		const steps = inputBufferFrames/2 + 1
		time.Sleep(step*steps + step/2)
		m.mixUpdate()
		require.EqualValues(t, 1+steps, m.mixCnt)
		m.CheckSampleN(steps)
	})
}
