// Copyright 2026 LiveKit, Inc.
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

package sip

import (
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
)

// dialtoneTones returns the ringback for the requested dial tone, or nil if none should play.
func dialtoneTones(tone livekit.SIPDialtone) []tones.Tone {
	switch tone {
	case livekit.SIPDialtone_SIP_DIALTONE_DISABLED:
		return nil
	case livekit.SIPDialtone_SIP_DIALTONE_US:
		return tones.USRinging
	case livekit.SIPDialtone_SIP_DIALTONE_UK:
		return tones.UKRinging
	case livekit.SIPDialtone_SIP_DIALTONE_JP:
		return tones.JPRinging
	case livekit.SIPDialtone_SIP_DIALTONE_AU:
		return tones.AURinging
	}
	// TODO: SIP_DIALTONE_AUTO should pick by the called number's country (tones.RingingByRegion).
	return tones.ETSIRinging
}
