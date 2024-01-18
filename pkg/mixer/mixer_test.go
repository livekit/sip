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

	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media"
)

func TestMixer(t *testing.T) {
	var sample media.PCM16Sample
	m := newMixer(media.WriterFunc[media.PCM16Sample](func(s media.PCM16Sample) error {
		sample = s
		return nil
	}), 5)

	t.Run("No Input", func(t *testing.T) {
		m.doMix()
		require.Equal(t, media.PCM16Sample{0, 0, 0, 0, 0}, sample)
	})

	t.Run("One Input", func(t *testing.T) {
		input := m.NewInput()
		defer m.RemoveInput(input)
		input.WriteSample([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.doMix()
		require.Equal(t, media.PCM16Sample{10, 11, 12, 13, 14}, sample)
	})

	t.Run("Two Inputs", func(t *testing.T) {
		firstInput := m.NewInput()
		defer m.RemoveInput(firstInput)
		firstInput.WriteSample([]int16{0xE, 0xD, 0xC, 0xB, 0xA})

		secondInput := m.NewInput()
		defer m.RemoveInput(secondInput)
		secondInput.WriteSample([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.doMix()
		require.Equal(t, media.PCM16Sample{24, 24, 24, 24, 24}, sample)

		firstInput.WriteSample([]int16{0x7FFF, 0x1, -0x7FFF, -0x1, 0x0})
		secondInput.WriteSample([]int16{0x1, 0x7FFF, -0x1, -0x7FFF, 0x0})

		m.doMix()
		require.Equal(t, media.PCM16Sample{0x7FFF, 0x7FFF, -0x7FFF, -0x7FFF, 0x0}, sample)

		m.RemoveInput(firstInput)
		m.RemoveInput(secondInput)
	})

	t.Run("No Buffer", func(t *testing.T) {
		input := m.NewInput()
		defer m.RemoveInput(input)

		expected := media.PCM16Sample{0, 1, 2, 3, 4}
		for i := 0; i < 5; i++ {
			input.WriteSample([]int16{0, 1, 2, 3, 4})
			m.doMix()
			require.Equal(t, expected, sample)
		}
	})

	t.Run("Buffer Underflow", func(t *testing.T) {
		input := m.NewInput()
		defer m.RemoveInput(input)

		for i := 0; i < inputBufferFrames; i++ {
			input.WriteSample([]int16{0, 1, 2, 3, 4})
		}

		for i := 0; i < inputBufferFrames+3; i++ {

			m.doMix()

			expected := media.PCM16Sample{0, 1, 2, 3, 4}
			if i >= inputBufferFrames {
				expected = media.PCM16Sample{0, 0, 0, 0, 0}
			}

			require.Equal(t, expected, sample, "i=%d", i)
		}
	})

	t.Run("Buffer Overflow", func(t *testing.T) {
		input := m.NewInput()
		defer m.RemoveInput(input)

		for i := 0; i < inputBufferFrames+3; i++ {
			input.WriteSample([]int16{0, 1, 2, 3, 4})
		}

		m.doMix()
		require.Equal(t, (inputBufferFrames-1)*5, input.buf.Len())
	})

}
