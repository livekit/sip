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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"runtime"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	protosignalling "github.com/livekit/protocol/signalling"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

var _ Signalling = (*signallingJoinRequest)(nil)

type SignallingJoinRequestParams struct {
	Logger logger.Logger
}

type signallingJoinRequest struct {
	*signallingBase
}

func NewSignallingJoinRequest(params SignallingJoinRequestParams) Signalling {
	return &signallingJoinRequest{
		signallingBase: newSignallingBase(signallingBaseParams(params)),
	}
}

func (s *signallingJoinRequest) PublishInJoin() bool {
	return true
}

func (s *signallingJoinRequest) ConnectQueryParams(
	version string,
	protocol int,
	connectParams *ConnectParams,
	addTrackRequests []*livekit.AddTrackRequest,
	publisherOffer webrtc.SessionDescription,
	participantSID string,
) (string, error) {
	clientInfo := &livekit.ClientInfo{
		Version:  version,
		Protocol: int32(protocol),
		Os:       runtime.GOOS,
		Sdk:      livekit.ClientInfo_GO,
	}

	connectionSettings := &livekit.ConnectionSettings{
		AutoSubscribe: connectParams.AutoSubscribe,
	}

	joinRequest := &livekit.JoinRequest{
		ClientInfo:            clientInfo,
		ConnectionSettings:    connectionSettings,
		Metadata:              connectParams.Metadata,
		ParticipantAttributes: connectParams.Attributes,
		AddTrackRequests:      addTrackRequests,
		PublisherOffer:        protosignalling.ToProtoSessionDescription(publisherOffer, 0, nil),
	}
	if connectParams.Reconnect {
		joinRequest.Reconnect = true
		if participantSID != "" {
			joinRequest.ParticipantSid = participantSID
		}
	}

	marshalled, err := proto.Marshal(joinRequest)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	writer.Write(marshalled)
	writer.Close()

	wrapped := &livekit.WrappedJoinRequest{
		Compression: livekit.WrappedJoinRequest_GZIP,
		JoinRequest: buf.Bytes(),
	}

	wrappedMarshalled, err := proto.Marshal(wrapped)
	if err != nil {
		return "", err
	}

	base64Bytes := base64.URLEncoding.EncodeToString(wrappedMarshalled)
	s.params.Logger.Infow("JoinRequest sizes", "marshalled", len(marshalled), "gzipped", len(buf.Bytes()), "wrappedMarshalled", len(wrappedMarshalled), "base64Bytes", len(base64Bytes))

	return fmt.Sprintf("&join_request=%s", base64Bytes), nil
}

func (s *signallingJoinRequest) HTTPRequestForValidate(
	ctx context.Context,
	version string,
	protocol int,
	urlPrefix string,
	token string,
	connectParams *ConnectParams,
	participantSID string,
) (*http.Request, error) {
	queryParams, err := s.ConnectQueryParams(
		version,
		protocol,
		connectParams,
		nil,
		webrtc.SessionDescription{},
		participantSID,
	)
	if err != nil {
		return nil, err
	}

	return s.signallingBase.HTTPRequestForValidate(ctx, urlPrefix, token, queryParams)
}
