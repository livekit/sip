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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
)

var _ Signalling = (*signalling)(nil)

type SignallingParams struct {
	Logger logger.Logger
}

type signalling struct {
	*signallingBase
}

func NewSignalling(params SignallingParams) Signalling {
	return &signalling{
		signallingBase: newSignallingBase(signallingBaseParams(params)),
	}
}

func (s *signalling) PublishInJoin() bool {
	return false
}

func (s *signalling) ConnectQueryParams(
	version string,
	protocol int,
	connectParams *ConnectParams,
	addTrackRequests []*livekit.AddTrackRequest,
	publisherOffer webrtc.SessionDescription,
	participantSID string,
) (string, error) {
	queryParams := fmt.Sprintf("version=%s&protocol=%d&", version, protocol)

	if connectParams.AutoSubscribe {
		queryParams += "&auto_subscribe=1"
	} else {
		queryParams += "&auto_subscribe=0"
	}
	if connectParams.Reconnect {
		queryParams += "&reconnect=1"
		if participantSID != "" {
			queryParams += fmt.Sprintf("&sid=%s", participantSID)
		}
	}
	if len(connectParams.Attributes) != 0 {
		data, err := json.Marshal(connectParams.Attributes)
		if err != nil {
			return "", ErrInvalidParameter
		}
		str := base64.URLEncoding.EncodeToString(data)
		queryParams += "&attributes=" + str
	}
	queryParams += "&sdk=go&os=" + runtime.GOOS

	return queryParams, nil
}

func (s *signalling) HTTPRequestForValidate(
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
