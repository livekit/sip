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

package signaling

import (
	"strings"

	"github.com/livekit/protocol/logger"
)

// ReInviteAction represents the type of re-INVITE.
type ReInviteAction int

const (
	ReInviteUnknown     ReInviteAction = iota
	ReInviteHold                       // call placed on hold (sendonly/inactive)
	ReInviteResume                     // call resumed from hold
	ReInviteCodecChange                // codec or media parameters changed
	ReInviteUpdate                     // generic media update
)

func (a ReInviteAction) String() string {
	switch a {
	case ReInviteHold:
		return "hold"
	case ReInviteResume:
		return "resume"
	case ReInviteCodecChange:
		return "codec_change"
	case ReInviteUpdate:
		return "update"
	default:
		return "unknown"
	}
}

// ReInviteHandler processes SIP re-INVITE requests for an active call.
type ReInviteHandler struct {
	log logger.Logger
}

// NewReInviteHandler creates a new re-INVITE handler.
func NewReInviteHandler(log logger.Logger) *ReInviteHandler {
	return &ReInviteHandler{log: log}
}

// ReInviteResult holds the analysis of a re-INVITE SDP.
type ReInviteResult struct {
	Action   ReInviteAction
	NewMedia *NegotiatedMedia
	OnHold   bool
}

// Analyze examines a re-INVITE SDP and determines what action is needed.
func (h *ReInviteHandler) Analyze(sdpBody string, currentMedia *NegotiatedMedia) (*ReInviteResult, error) {
	parsed, err := ParseSDP(sdpBody)
	if err != nil {
		return nil, err
	}

	result := &ReInviteResult{
		Action: ReInviteUpdate,
	}

	// Check for hold (sendonly or inactive direction)
	if isHoldSDP(sdpBody) {
		result.Action = ReInviteHold
		result.OnHold = true
		h.log.Infow("re-INVITE: call placed on hold")
		return result, nil
	}

	// Check if resuming from hold
	if isSendRecvSDP(sdpBody) && currentMedia != nil {
		result.Action = ReInviteResume
		result.OnHold = false
		h.log.Infow("re-INVITE: call resumed from hold")
	}

	// Re-negotiate media
	newMedia, err := parsed.Negotiate()
	if err != nil {
		return nil, err
	}
	result.NewMedia = newMedia

	// Check for codec change
	if currentMedia != nil && newMedia.VideoCodec != currentMedia.VideoCodec {
		result.Action = ReInviteCodecChange
		h.log.Infow("re-INVITE: codec change detected",
			"oldCodec", currentMedia.VideoCodec,
			"newCodec", newMedia.VideoCodec,
		)
	}

	return result, nil
}

// isHoldSDP checks if the SDP indicates a hold (sendonly or inactive).
func isHoldSDP(sdp string) bool {
	lines := strings.Split(sdp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "a=sendonly" || line == "a=inactive" {
			return true
		}
	}
	// Also check for c=0.0.0.0 (RFC 2543 hold)
	for _, line := range lines {
		if strings.HasPrefix(line, "c=IN IP4 0.0.0.0") {
			return true
		}
	}
	return false
}

// isSendRecvSDP checks if the SDP has sendrecv direction (active media).
func isSendRecvSDP(sdp string) bool {
	lines := strings.Split(sdp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "a=sendrecv" {
			return true
		}
	}
	return false
}
