package sip

import (
	"bytes"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/ulaw"
)

func readMkvAudioFile(data []byte) []media.PCM16Sample {
	var ret struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(bytes.NewReader(data), &ret); err != nil {
		panic(err)
	}

	var frames []media.PCM16Sample
	for _, cluster := range ret.Segment.Cluster {
		for _, block := range cluster.SimpleBlock {
			for _, data := range block.Data {
				lpcm := ulaw.Sample(data).Decode()
				pcm := lpcm.Decode()
				frames = append(frames, pcm)
			}
		}
	}
	return frames
}
