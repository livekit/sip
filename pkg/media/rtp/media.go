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

package rtp

import "time"

const (
	// DefSampleRate is a default number of audio samples per second.
	DefSampleRate = 8000
	// DefFrameDur is a default duration of an audio frame.
	DefFrameDur = 20 * time.Millisecond
	// DefFrameRate is a default number of audio frames per second.
	DefFrameRate = int(time.Second / DefFrameDur)
	// DefPacketDur is a default duration of frame packet in DefFrameDur units (aka samples per frame).
	// It is used as an increment for RTP timestamps.
	DefPacketDur = uint32(DefSampleRate / DefFrameRate)
)
