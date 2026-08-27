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

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

var _ SignalTransport = (*signalTransportUnimplemented)(nil)

type signalTransportUnimplemented struct{}

func (s *signalTransportUnimplemented) SetLogger(l protoLogger.Logger) {}

func (s *signalTransportUnimplemented) Start() {}

func (s *signalTransportUnimplemented) IsStarted() bool {
	return false
}

func (s *signalTransportUnimplemented) Close() {}

func (s *signalTransportUnimplemented) Join(
	ctx context.Context,
	url string,
	token string,
	connectParams ConnectParams,
	addTrackRequests []*livekit.AddTrackRequest,
	publisherOffer webrtc.SessionDescription,
) error {
	return ErrUnimplemented
}

func (s *signalTransportUnimplemented) Reconnect(
	ctx context.Context,
	url string,
	token string,
	connectParams ConnectParams,
	participantSID string,
) error {
	return ErrUnimplemented
}

func (s *signalTransportUnimplemented) SendMessage(msg proto.Message) error {
	return nil
}
