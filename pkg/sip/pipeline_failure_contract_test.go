// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"io"
	"testing"
	"testing/synctest"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/media-sdk/sdp"
	"github.com/stretchr/testify/require"
)

func TestMediaPipelineDecoderFailurePreservesRoomSinkForRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPipelineHarness(t, 48000)
		out := &playoutCodecSink{rate: 48000}
		h.audioIn.Swap(out)
		codec := audioCodecByName(t, g722.SDPNameOnly)
		info, _, ok := codec.Supports(msdk.CodecConfig{SampleRate: 16000})
		require.True(t, ok)
		info.Channels = 2 // G722 only supports mono; the public pipeline must reject this configuration.
		h.audio = &sdp.CodecInfo{Type: testAudioPT(codec), Codec: codec, Info: info}
		pipe, err := NewMediaPortPipeline(h.conf, h.mediaConfig(), h.port, h.audioIn, h.dtmfIn, h.audioIn.SampleRate())
		require.Nil(t, pipe, "failed initialization must not return a partially usable pipeline")
		require.ErrorContains(t, err, "failed to setup pipeline input")
		require.ErrorContains(t, err, `no support for PCM codec "G722"`)
		require.Empty(t, out.snapshot(), "a failed decoder must not publish audio")
		require.Zero(t, out.closeCount, "initialization cleanup must not close the caller-owned room sink")
		_, err = h.port.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF, "the failed pipeline must close its RTP session")

		// A new negotiation can reuse both anchors after the failed attempt.
		h.port.Reopen()
		h.configureType(codec, 16000, testAudioPT(codec), 0, false)
		for i := uint16(0); i < 5; i++ {
			h.injectAudio(1, i+1, uint32(i+1)*160, h.codecFrame())
		}
		synctest.Wait()
		frames := out.snapshot()
		require.NotEmpty(t, frames, "valid G722 must decode through the same room sink and real resampler")
		var samples msdk.PCM16Sample
		for _, frame := range frames {
			samples = append(samples, frame.pcm...)
		}
		require.Greater(t, pcmEnergy(samples), int64(0))
		require.NoError(t, h.pipeline.Close())
		require.Zero(t, out.closeCount, "successful pipeline shutdown also leaves the borrowed room sink open")
	})
}
