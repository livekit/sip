package oggreader

import (
	"errors"
	"time"
)

const (
	maxFrameDuration = 120 * time.Millisecond
)

var (
	ErrInvalidPacket = errors.New("invalid opus packet")
)

// Parse the duration of a an OpusPacket
// https://www.rfc-editor.org/rfc/rfc6716#section-3.1
func ParsePacketDuration(data []byte) (time.Duration, error) {
	durations := [32]time.Duration{
		10e6, 20e6, 40e6, 60e6, // Silk-Only
		10e6, 20e6, 40e6, 60e6, // Silk-Only
		10e6, 20e6, 40e6, 60e6, // Silk-Only
		10e6, 20e6, // Hybrid
		10e6, 20e6, // Hybrid
		2.5e6, 5e6, 10e6, 20e6, // Celt-Only
		2.5e6, 5e6, 10e6, 20e6, // Celt-Only
		2.5e6, 5e6, 10e6, 20e6, // Celt-Only
		2.5e6, 5e6, 10e6, 20e6, // Celt-Only
	}

	if len(data) < 1 {
		return 0, ErrInvalidPacket
	}

	toc := data[0]
	var nFrames int
	switch toc & 3 {
	case 0:
		nFrames = 1
	case 1:
		nFrames = 2
	case 2:
		nFrames = 2
	case 3:
		if len(data) < 2 {
			return 0, ErrInvalidPacket
		}
		nFrames = int(data[1] & 63)
	}

	duration := time.Duration(nFrames) * durations[toc>>3]
	if duration > maxFrameDuration {
		return 0, ErrInvalidPacket
	}
	return duration, nil
}
