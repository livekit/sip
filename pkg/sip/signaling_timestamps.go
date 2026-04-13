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
	"time"

	"github.com/livekit/protocol/logger"
)

// SignalingTimestamps records wall-clock times for key SIP signaling events
// so that setup latency can be logged at the end of a session.
type SignalingTimestamps struct {
	// Inbound: INVITE received; Outbound: INVITE sent.
	InviteTime time.Time

	// Inbound: 100 Trying sent; Outbound: 100 Trying received.
	// Zero if 100 Trying was never sent/received.
	TryingTime time.Time

	// Inbound: first 180/183 sent; Outbound: first 180/183 received.
	RingingTime time.Time

	// Inbound: 200 OK sent; Outbound: 200 OK received.
	AcceptTime time.Time

	// Outbound only: time the API request was received (call creation).
	APITime time.Time

	// Outbound only: ACK sent after receiving 200 OK.
	AckTime time.Time
}

func (ts *SignalingTimestamps) Log(log logger.Logger) {
	if ts.InviteTime.IsZero() {
		return
	}

	var fields []interface{}

	// Outbound: API -> INVITE sent
	if !ts.APITime.IsZero() {
		fields = append(fields, "apiToInviteMs", ts.InviteTime.Sub(ts.APITime).Milliseconds())
	}

	// INVITE -> 100 Trying
	if !ts.TryingTime.IsZero() {
		fields = append(fields, "inviteToTryingMs", ts.TryingTime.Sub(ts.InviteTime).Milliseconds())
	}

	// INVITE -> first 180/183 Ringing
	if !ts.RingingTime.IsZero() {
		fields = append(fields, "inviteToRingingMs", ts.RingingTime.Sub(ts.InviteTime).Milliseconds())
	}

	// INVITE -> 200 OK
	if !ts.AcceptTime.IsZero() {
		fields = append(fields, "inviteToAcceptMs", ts.AcceptTime.Sub(ts.InviteTime).Milliseconds())
	}

	// 200 OK -> ACK (outbound)
	if !ts.AckTime.IsZero() && !ts.AcceptTime.IsZero() {
		fields = append(fields, "acceptToAckMs", ts.AckTime.Sub(ts.AcceptTime).Milliseconds())
	}

	// Total outbound: API -> ACK
	if !ts.APITime.IsZero() && !ts.AckTime.IsZero() {
		fields = append(fields, "apiToAckMs", ts.AckTime.Sub(ts.APITime).Milliseconds())
	}

	log.Infow("signaling timestamps", fields...)
}
