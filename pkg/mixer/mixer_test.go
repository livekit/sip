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
		m.AddInput()
		m.Push([]int16{0xA, 0xB, 0xC, 0xD, 0xE})
		if !bytes.Equal(sample, []byte{10, 0, 11, 0, 12, 0, 13, 0, 14, 0}) {
			t.Fatal()
		}
	})

	t.Run("Two Input", func(t *testing.T) {
		m.AddInput()
		m.Push([]int16{0xE, 0xD, 0xC, 0xB, 0xA})
		m.Push([]int16{0xA, 0xB, 0xC, 0xD, 0xE})
		if !bytes.Equal(sample, []byte{12, 0, 11, 0, 12, 0, 11, 0, 12, 0}) {
			t.Fatal()
		}
	})
}
