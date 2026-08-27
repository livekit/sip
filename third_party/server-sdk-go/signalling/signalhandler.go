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
	"fmt"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	protosignalling "github.com/livekit/protocol/signalling"
	"google.golang.org/protobuf/proto"
)

var _ SignalHandler = (*signalhandler)(nil)

type SignalHandlerParams struct {
	Logger    logger.Logger
	Processor SignalProcessor
}

type signalhandler struct {
	signalhandlerUnimplemented

	params SignalHandlerParams
}

func NewSignalHandler(params SignalHandlerParams) SignalHandler {
	return &signalhandler{
		params: params,
	}
}

func (s *signalhandler) SetLogger(l logger.Logger) {
	s.params.Logger = l
}

func (s *signalhandler) HandleMessage(msg proto.Message) error {
	rsp, ok := msg.(*livekit.SignalResponse)
	if !ok {
		s.params.Logger.Warnw(
			"unknown message type", nil,
			"messageType", fmt.Sprintf("%T", msg),
		)
		return ErrInvalidMessageType
	}

	switch payload := rsp.GetMessage().(type) {
	case *livekit.SignalResponse_Join:
		s.params.Processor.OnJoinResponse(payload.Join)

	case *livekit.SignalResponse_Reconnect:
		s.params.Processor.OnReconnectResponse(payload.Reconnect)

	case *livekit.SignalResponse_Answer:
		s.params.Processor.OnAnswer(protosignalling.FromProtoSessionDescription(payload.Answer))

	case *livekit.SignalResponse_Offer:
		s.params.Processor.OnOffer(protosignalling.FromProtoSessionDescription(payload.Offer))

	case *livekit.SignalResponse_Trickle:
		ci, err := protosignalling.FromProtoTrickle(payload.Trickle)
		if err != nil {
			s.params.Logger.Warnw("could not unmarshal ICE candidate", err)
			return err
		}

		s.params.Processor.OnTrickle(ci, payload.Trickle.Target)

	case *livekit.SignalResponse_Update:
		s.params.Processor.OnParticipantUpdate(payload.Update.Participants)

	case *livekit.SignalResponse_SpeakersChanged:
		s.params.Processor.OnSpeakersChanged(payload.SpeakersChanged.Speakers)

	case *livekit.SignalResponse_TrackPublished:
		s.params.Processor.OnLocalTrackPublished(payload.TrackPublished)

	case *livekit.SignalResponse_Mute:
		s.params.Processor.OnTrackRemoteMuted(payload.Mute)

	case *livekit.SignalResponse_ConnectionQuality:
		s.params.Processor.OnConnectionQuality(payload.ConnectionQuality.Updates)

	case *livekit.SignalResponse_RoomUpdate:
		s.params.Processor.OnRoomUpdate(payload.RoomUpdate.Room)

	case *livekit.SignalResponse_RoomMoved:
		s.params.Processor.OnTokenRefresh(payload.RoomMoved.Token)

		s.params.Processor.OnRoomMoved(payload.RoomMoved)

	case *livekit.SignalResponse_Leave:
		s.params.Processor.OnLeave(payload.Leave)

	case *livekit.SignalResponse_RefreshToken:
		s.params.Processor.OnTokenRefresh(payload.RefreshToken)

	case *livekit.SignalResponse_TrackUnpublished:
		s.params.Processor.OnLocalTrackUnpublished(payload.TrackUnpublished)

	case *livekit.SignalResponse_TrackSubscribed:
		s.params.Processor.OnLocalTrackSubscribed(payload.TrackSubscribed)

	case *livekit.SignalResponse_SubscribedQualityUpdate:
		s.params.Processor.OnSubscribedQualityUpdate(payload.SubscribedQualityUpdate)

	case *livekit.SignalResponse_SubscribedAudioCodecUpdate:
		s.params.Processor.OnSubscribedAudioCodecUpdate(payload.SubscribedAudioCodecUpdate)

	case *livekit.SignalResponse_MediaSectionsRequirement:
		s.params.Processor.OnMediaSectionsRequirement(payload.MediaSectionsRequirement)
	}

	return nil
}
