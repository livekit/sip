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
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

// terminationFromRoomDisconnect classifies a call termination triggered by
// the LiveKit room closing. The reason comes from lksdk's
// OnDisconnectedWithReason callback. Clean leaves (customer ended the call
// or removed the participant) count as success; connection failures and
// unknown causes count as server_error.
func terminationFromRoomDisconnect(reason lksdk.DisconnectionReason) stats.Termination {
	switch reason {
	case lksdk.LeaveRequested, lksdk.RoomClosed, lksdk.ParticipantRemoved:
		return stats.Success("removed")
	case lksdk.Failed:
		return stats.ServerError("room-failed")
	default:
		// UserUnavailable, RejectedByUser, DuplicateIdentity, OtherReason,
		// or empty (no reason reported by SDK). Conservative default —
		// surface as server_error so the SLI doesn't silently absorb LK-side
		// issues.
		return stats.ServerError("room-disconnected")
	}
}

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
	"X-Twilio-AccountSid":            livekit.AttrSIPPrefix + "twilio.accountSid",
	"X-Twilio-CallSid":               livekit.AttrSIPPrefix + "twilio.callSid",
	"X-call_leg_id":                  livekit.AttrSIPPrefix + "telnyx.callLegID",
	"X-call_session_id":              livekit.AttrSIPPrefix + "telnyx.callSessionID",
	"X-Amzn-ConnectContactId":        livekit.AttrSIPPrefix + "amazon.contactId",
	"X-Amzn-ConnectInitialContactId": livekit.AttrSIPPrefix + "amazon.initialContactId",
	"X-Amzn-SourceAccount":           livekit.AttrSIPPrefix + "amazon.sourceAccount",
	"X-Amzn-SourceArn":               livekit.AttrSIPPrefix + "amazon.sourceArn",
	"X-Amzn-TargetArn":               livekit.AttrSIPPrefix + "amazon.targetArn",
	"X-Lk-Test-Id":                   "lktest.id",
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
