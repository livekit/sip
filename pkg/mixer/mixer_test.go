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
	m := createMixer(media.WriterFunc[media.PCM16Sample](func(s media.PCM16Sample) error {
		sample = s
		return nil
	}), 8000)

	require.Equal(t, 160, m.mixSize)

	m.mixSize = 5

	t.Run("No Input", func(t *testing.T) {
		m.doMix()
		require.Equal(t, media.PCM16Sample{0, 0, 0, 0, 0}, sample)
	})

	t.Run("One Input", func(t *testing.T) {
		input := m.AddInput()
		input.bufferSize = 0
		input.Push([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.doMix()
		require.Equal(t, media.PCM16Sample{10, 11, 12, 13, 14}, sample)
		m.RemoveInput(input)
	})

	t.Run("Two Input", func(t *testing.T) {
		firstInput := m.AddInput()
		firstInput.bufferSize = 0
		firstInput.Push([]int16{0xE, 0xD, 0xC, 0xB, 0xA})

		secondInput := m.AddInput()
		secondInput.bufferSize = 0
		secondInput.Push([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.doMix()
		require.Equal(t, media.PCM16Sample{24, 24, 24, 24, 24}, sample)

		firstInput.Push([]int16{0x7FFF, 0x1, -0x7FFF, -0x1, 0x0})
		secondInput.Push([]int16{0x1, 0x7FFF, -0x1, -0x7FFF, 0x0})

		m.doMix()
		require.Equal(t, media.PCM16Sample{0x7FFF, 0x7FFF, -0x7FFF, -0x7FFF, 0x0}, sample)

		m.RemoveInput(firstInput)
		m.RemoveInput(secondInput)
	})

	t.Run("Does Buffer", func(t *testing.T) {
		input := m.AddInput()

		for i := 0; i < 5; i++ {
			input.Push([]int16{0, 1, 2, 3, 4})
			m.doMix()

			expected := media.PCM16Sample{0, 0, 0, 0, 0}
			if i == 4 {
				expected = media.PCM16Sample{0, 1, 2, 3, 4}
			}

			require.Equal(t, expected, sample)
		}

		m.RemoveInput(input)
	})

	t.Run("Buffer Underflow", func(t *testing.T) {
		input := m.AddInput()

		for i := 0; i < 5; i++ {
			input.Push([]int16{0, 1, 2, 3, 4})
		}

		for i := 0; i < 8; i++ {
			m.doMix()

			expected := media.PCM16Sample{0, 1, 2, 3, 4}
			if i == 7 {
				expected = media.PCM16Sample{0, 0, 0, 0, 0}
			}

			require.Equal(t, expected, sample)
		}

		m.RemoveInput(input)
	})

	t.Run("Buffer Overflow", func(t *testing.T) {
		input := m.AddInput()

		for i := 0; i < 500; i++ {
			input.Push([]int16{0, 1, 2, 3, 4})
		}

		m.doMix()
		require.Equal(t, 400, input.samples.Len())
		m.RemoveInput(input)
	})

}
