// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build cgo

package sip

// Opus codec registration.
//
// The current pinned media-sdk does not register Opus (it is the codec used
// for file playback, not as a SIP media codec), so register it here with the
// canonical SDP name per RFC 7587 §6.1. Once the media-sdk ships its own Opus
// media codec registration, drop this file in favor of the upstream one.

import (
	media "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/opus"
	"github.com/livekit/protocol/logger"
)

func init() {
	media.RegisterCodec(media.NewAudioCodec(
		media.CodecInfo{
			SDPName:     opusSDPName,
			SampleRate:  48000,
			RTPIsStatic: false,
			Priority:    10,
			Disabled:    true, // registered disabled; enabled via the default codec set
			FileExt:     "opus",
		},
		func(w media.PCM16Writer) media.WriteCloser[opus.Sample] {
			dec, err := opus.Decode(w, 1, logger.GetLogger())
			if err != nil {
				return nil
			}
			return dec
		},
		func(w media.WriteCloser[opus.Sample]) media.PCM16Writer {
			enc, err := opus.Encode(w, 1, logger.GetLogger())
			if err != nil {
				return nil
			}
			return enc
		},
	))
}
