// Copyright 2023 LiveKit, Inc.
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

package signalling

import (
	"context"
	"net/http"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

var _ Signalling = (*signallingUnimplemented)(nil)

type signallingUnimplemented struct {
}

func (s *signallingUnimplemented) SetLogger(l logger.Logger) {}

func (s *signallingUnimplemented) Path() string {
	return ""
}

func (s *signallingUnimplemented) ValidatePath() string {
	return ""
}

func (s *signallingUnimplemented) PublishInJoin() bool {
	return false
}

func (s *signallingUnimplemented) ConnectQueryParams(
	version string,
	protocol int,
	connectParams *ConnectParams,
	addTrackRequests []*livekit.AddTrackRequest,
	publisherOffer webrtc.SessionDescription,
	participantSID string,
) (string, error) {
	return "", ErrUnimplemented
}

func (s *signallingUnimplemented) HTTPRequestForValidate(
	ctx context.Context,
	version string,
	protocol int,
	urlPrefix string,
	token string,
	connectParams *ConnectParams,
	participantSID string,
) (*http.Request, error) {
	return nil, ErrUnimplemented
}

func (s *signallingUnimplemented) DecodeErrorResponse(errorDetails []byte) string {
	return ""
}

func (s *signallingUnimplemented) SignalLeaveRequest(leave *livekit.LeaveRequest) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalICECandidate(trickle *livekit.TrickleRequest) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalSdpOffer(offer *livekit.SessionDescription) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalSimulateScenario(simulate *livekit.SimulateScenario) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalMuteTrack(mute *livekit.MuteTrackRequest) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalUpdateSubscription(updateSubscription *livekit.UpdateSubscription) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalSyncState(syncState *livekit.SyncState) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalAddTrack(addTrack *livekit.AddTrackRequest) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalSubscriptionPermission(subscriptionPermission *livekit.SubscriptionPermission) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalUpdateTrackSettings(settings *livekit.UpdateTrackSettings) proto.Message {
	return nil
}

func (s *signallingUnimplemented) SignalUpdateParticipantMetadata(metadata *livekit.UpdateParticipantMetadata) proto.Message {
	return nil
}
