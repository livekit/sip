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
	"fmt"
	"net/http"
	"net/url"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

type signallingBaseParams struct {
	Logger logger.Logger
}

type signallingBase struct {
	signallingUnimplemented

	params signallingBaseParams
}

func newSignallingBase(params signallingBaseParams) *signallingBase {
	return &signallingBase{
		params: params,
	}
}

func (s *signallingBase) SetLogger(l logger.Logger) {
	s.params.Logger = l
}

func (s *signallingBase) Path() string {
	return "/rtc"
}

func (s *signallingBase) ValidatePath() string {
	return "/rtc/validate"
}

func (s *signallingBase) HTTPRequestForValidate(
	ctx context.Context,
	urlPrefix string,
	token string,
	queryParams string,
) (*http.Request, error) {
	if urlPrefix == "" {
		return nil, ErrURLNotProvided
	}

	u, err := url.Parse(ToHttpURL(urlPrefix) + s.ValidatePath() + fmt.Sprintf("?%s", queryParams))
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		s.params.Logger.Errorw("error creating validate request", err)
		return nil, err
	}
	req.Header = NewHTTPHeaderWithToken(token)
	return req, nil
}

func (s *signallingBase) DecodeErrorResponse(errorDetails []byte) string {
	return string(errorDetails)
}

func (s *signallingBase) SignalLeaveRequest(leave *livekit.LeaveRequest) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Leave{
			Leave: leave,
		},
	}
}

func (s *signallingBase) SignalICECandidate(trickle *livekit.TrickleRequest) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Trickle{
			Trickle: trickle,
		},
	}
}

func (s *signallingBase) SignalSdpOffer(offer *livekit.SessionDescription) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: offer,
		},
	}
}

func (s *signallingBase) SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Answer{
			Answer: answer,
		},
	}
}

func (s *signallingBase) SignalSimulateScenario(simulate *livekit.SimulateScenario) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Simulate{
			Simulate: simulate,
		},
	}
}

func (s *signallingBase) SignalMuteTrack(mute *livekit.MuteTrackRequest) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Mute{
			Mute: mute,
		},
	}
}

func (s *signallingBase) SignalUpdateSubscription(updateSubscription *livekit.UpdateSubscription) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Subscription{
			Subscription: updateSubscription,
		},
	}
}

func (s *signallingBase) SignalSyncState(syncState *livekit.SyncState) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_SyncState{
			SyncState: syncState,
		},
	}
}

func (s *signallingBase) SignalAddTrack(addTrack *livekit.AddTrackRequest) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_AddTrack{
			AddTrack: addTrack,
		},
	}
}

func (s *signallingBase) SignalSubscriptionPermission(subscriptionPermission *livekit.SubscriptionPermission) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_SubscriptionPermission{
			SubscriptionPermission: subscriptionPermission,
		},
	}
}

func (s *signallingBase) SignalUpdateTrackSettings(settings *livekit.UpdateTrackSettings) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_TrackSetting{
			TrackSetting: settings,
		},
	}
}

func (s *signallingBase) SignalUpdateParticipantMetadata(metadata *livekit.UpdateParticipantMetadata) proto.Message {
	return &livekit.SignalRequest{
		Message: &livekit.SignalRequest_UpdateMetadata{
			UpdateMetadata: metadata,
		},
	}
}
