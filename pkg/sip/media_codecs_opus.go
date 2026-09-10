// Copyright 2024 LiveKit, Inc.
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

//go:build cgo

package sip

import (
	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/opus"
	"github.com/livekit/protocol/logger"
)

func init() {
	msdk.RegisterCodec(msdk.NewAudioCodec(msdk.CodecInfo{
		SDPName:      OpusSDPName,
		SampleRate:   48000,
		RTPClockRate: 48000,
		RTPIsStatic:  false,
		Priority:     10,
		Disabled:     true,
		FileExt:      "opus",
	}, opusDecode, opusEncode))
}

// SetOpusEnabled toggles Opus in both the per-call default codec set and the
// global media-sdk codec set. Call once during Service.Start.
func SetOpusEnabled(enabled bool) {
	defaultCodecs.SetEnabled(OpusSDPName, enabled)
	msdk.CodecSetEnabled(OpusSDPName, enabled)
}

func opusDecode(w msdk.PCM16Writer) msdk.WriteCloser[opus.Sample] {
	dec, err := opus.Decode(w, 1, logger.GetLogger())
	if err != nil {
		logger.GetLogger().Errorw("opus decode init failed", err)
		return nil
	}
	return dec
}

func opusEncode(w msdk.WriteCloser[opus.Sample]) msdk.PCM16Writer {
	enc, err := opus.Encode(w, 1, logger.GetLogger())
	if err != nil {
		logger.GetLogger().Errorw("opus encode init failed", err)
		return nil
	}
	return enc
}
