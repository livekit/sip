package sip

import (
	"testing"

	"github.com/livekit/sip/res"
)

func TestDecodeRes(t *testing.T) {
	res := res.EnterPinMkv
	frames := audioFileToFrames(res)
	for i, f := range frames {
		t.Log(i, len(f))
	}
	t.Log(len(frames))
}
