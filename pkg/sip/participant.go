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

import "github.com/livekit/protocol/livekit"

var headerToLog = map[string]string{
	"X-Twilio-AccountSid": "twilioAccSID",
	"X-Twilio-CallSid":    "twilioCallSID",
}

var headerToAttr = map[string]string{
	"X-Twilio-AccountSid": livekit.AttrSIPPrefix + "twilio.accountSid",
	"X-Twilio-CallSid":    livekit.AttrSIPPrefix + "twilio.callSid",
	"X-Lk-Test-Id":        "lktest.id",
}

const (
	AttrSIPCallStatus = livekit.AttrSIPPrefix + "callStatus"
)

type CallStatus int

func (v CallStatus) Attribute() string {
	switch v {
	default:
		return "" // no attribute for these statuses
	case CallDialing:
		return "dialing"
	case CallAutomation:
		return "automation"
	case CallActive:
		return "active"
	case CallHangup:
		return "hangup"
	}
}

func (v CallStatus) DisconnectReason() livekit.DisconnectReason {
	switch v {
	default:
		return livekit.DisconnectReason_UNKNOWN_REASON
	case CallHangup:
		// It's the default that LK sets, but map it here explicitly to show the assumption.
		return livekit.DisconnectReason_CLIENT_INITIATED
	case callUnavailable:
		return livekit.DisconnectReason_USER_UNAVAILABLE
	case callRejected:
		return livekit.DisconnectReason_USER_REJECTED
	}
}

const (
	callDropped = CallStatus(iota)
	CallDialing
	CallAutomation
	CallActive
	CallHangup
	callUnavailable
	callRejected
)
