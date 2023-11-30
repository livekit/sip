package ulaw

import (
	"github.com/zaf/g711"

	"github.com/livekit/sip/pkg/media"
)

type Sample []byte

func (s Sample) Decode() media.LPCM16Sample {
	return g711.DecodeUlaw(s)
}

func (s *Sample) Encode(data media.LPCM16Sample) {
	*s = g711.EncodeUlaw(data)
}

func Encode(w media.Writer[media.LPCM16Sample]) media.Writer[Sample] {
	return media.WriterFunc[Sample](func(in Sample) error {
		out := in.Decode()
		return w.WriteSample(out)
	})
}

func Decode(w media.Writer[Sample]) media.Writer[media.LPCM16Sample] {
	return media.WriterFunc[media.LPCM16Sample](func(in media.LPCM16Sample) error {
		var s Sample
		s.Encode(in)
		return w.WriteSample(s)
	})
}
