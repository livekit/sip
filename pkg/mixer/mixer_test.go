package mixer

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMixer(t *testing.T) {
	sample := []byte{}
	m := createMixer(func(s []byte) {
		sample = s
	}, 8000)

	require.Equal(t, 160, m.mixSize)

	m.mixSize = 5

	t.Run("No Input", func(t *testing.T) {
		m.doMix()
		if !bytes.Equal(sample, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
			t.Fatal()
		}
	})

	t.Run("One Input", func(t *testing.T) {
		input := m.AddInput()
		input.bufferSize = 0
		input.Push([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.doMix()
		require.Equal(t, []byte{10, 0, 11, 0, 12, 0, 13, 0, 14, 0}, sample)
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
		require.Equal(t, []byte{24, 0, 24, 0, 24, 0, 24, 0, 24, 0}, sample)

		firstInput.Push([]int16{0x7FFF, 0x1, -0x7FFF, -0x1, 0x0})
		secondInput.Push([]int16{0x1, 0x7FFF, -0x1, -0x7FFF, 0x0})

		m.doMix()
		require.Equal(t, []byte{0xFF, 0x7F, 0xFF, 0x7F, 0x1, 0x80, 0x1, 0x80, 0x0, 0x0}, sample)

		m.RemoveInput(firstInput)
		m.RemoveInput(secondInput)
	})

	t.Run("Does Buffer", func(t *testing.T) {
		input := m.AddInput()

		for i := 0; i < 5; i++ {
			input.Push([]int16{0, 1, 2, 3, 4})
			m.doMix()

			expected := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			if i == 4 {
				expected = []byte{0, 0, 1, 0, 2, 0, 3, 0, 4, 0}
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

			expected := []byte{0, 0, 1, 0, 2, 0, 3, 0, 4, 0}
			if i == 7 {
				expected = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
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
