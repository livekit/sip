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

package sip

import (
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sipgo/sip"
)

const (
	// maxCallDuration sets a global max call duration.
	maxCallDuration = 24 * time.Hour
	// defaultRingingTimeout is a maximal duration which SIP participant will wait to connect.
	//
	// For inbound, the participant will wait this duration for other participant tracks.
	//
	// For outbound, this sets a timeout for the other end to pick up the call.
	defaultRingingTimeout = 3 * time.Minute
)

const (
	AttrSIPCallIDFull = livekit.AttrSIPPrefix + "callIDFull"
	AttrSIPCallTag    = livekit.AttrSIPPrefix + "callTag"
)

var headerToLog = map[string]string{
	"X-Twilio-AccountSid": "twilioAccSID",
	"X-Twilio-CallSid":    "twilioCallSID",
	"X-call_leg_id":       "telnyxCallLegID",
	"X-call_session_id":   "telnyxCallSessionID",
}

var headerToAttr = map[string]string{
	"X-Twilio-AccountSid": livekit.AttrSIPPrefix + "twilio.accountSid",
	"X-Twilio-CallSid":    livekit.AttrSIPPrefix + "twilio.callSid",
	"X-call_leg_id":       livekit.AttrSIPPrefix + "telnyx.callLegID",
	"X-call_session_id":   livekit.AttrSIPPrefix + "telnyx.callSessionID",
	"X-Lk-Test-Id":        "lktest.id",
}

type CallStatus int

func (v CallStatus) Attribute() string {
	switch v {
	default:
		return "" // no attribute for these statuses
	case CallDialing:
		return "dialing"
	case CallRinging:
		return "ringing"
	case CallAutomation:
		return "automation"
	case CallActive:
		return "active"
	case CallHangup, callHangupMedia:
		return "hangup"
	}
}

func (v CallStatus) DisconnectReason() livekit.DisconnectReason {
	switch v {
	default:
		return livekit.DisconnectReason_UNKNOWN_REASON
	case CallHangup, callHangupMedia:
		// It's the default that LK sets, but map it here explicitly to show the assumption.
		return livekit.DisconnectReason_CLIENT_INITIATED
	case callUnavailable:
		return livekit.DisconnectReason_USER_UNAVAILABLE
	case callRejected:
		return livekit.DisconnectReason_USER_REJECTED
	}
}

func (v CallStatus) SIPStatus() (sip.StatusCode, string) {
	switch v {
	default:
		return sip.StatusBusyHere, "Rejected"
	case callMediaFailed:
		return sip.StatusNotAcceptableHere, "MediaFailed"
	}
}

const (
	callDropped = CallStatus(iota)
	callFlood
	CallDialing
	CallRinging
	CallAutomation
	CallActive
	CallHangup
	callUnavailable
	callRejected
	callMediaFailed
	callAcceptFailed
	callNoACK
	callHangupMedia
)
