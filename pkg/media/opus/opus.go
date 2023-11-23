package opus

import (
	"gopkg.in/hraban/opus.v2"

	"github.com/livekit/sip/pkg/media"
)

type Sample []byte

func Decode(w media.Writer[media.PCM16Sample], sampleRate int, channels int) (media.Writer[Sample], error) {
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return nil, err
	}
	buf := make([]int16, 1000)
	return media.WriterFunc[Sample](func(in Sample) error {
		n, err := dec.Decode(in, buf)
		if err != nil {
			return err
		}
		return w.WriteSample(buf[:n])
	}), nil
}

func Encode(w media.Writer[Sample], sampleRate int, channels int) (media.Writer[media.PCM16Sample], error) {
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1024)
	return media.WriterFunc[media.PCM16Sample](func(in media.PCM16Sample) error {
		n, err := enc.Encode(in, buf)
		if err != nil {
			return err
		}
		return w.WriteSample(buf[:n])
	}), nil
}
