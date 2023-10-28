package mixer

import (
	"bytes"
	"testing"
)

func TestMixer(t *testing.T) {
	sample := []byte{}
	m := &Mixer{
		mixSize: 5,
		onSample: func(s []byte) {
			sample = s
		},
	}

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
		if !bytes.Equal(sample, []byte{10, 0, 11, 0, 12, 0, 13, 0, 14, 0}) {
			t.Fatal()
		}
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
		if !bytes.Equal(sample, []byte{12, 0, 11, 0, 12, 0, 11, 0, 12, 0}) {
			t.Fatal()
		}

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

			if !bytes.Equal(sample, expected) {
				t.Fatal()
			}
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

			if !bytes.Equal(sample, expected) {
				t.Fatal()
			}
		}

		m.RemoveInput(input)
	})

	t.Run("Buffer Overflow", func(t *testing.T) {
		input := m.AddInput()

		for i := 0; i < 500; i++ {
			input.Push([]int16{0, 1, 2, 3, 4})
		}

		m.doMix()
		if len(input.samples) != 400 {
			t.Fatal()
		}
		m.RemoveInput(input)
	})

}
