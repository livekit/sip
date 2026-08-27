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

package lksdk

import (
	"errors"

	"github.com/livekit/server-sdk-go/v2/signalling"
	"github.com/twitchtv/twirp"
)

// ServerError is returned when a server API call fails. It exposes the error
// code, message, and any metadata the server attached. Use errors.As to extract
// it from a returned error.
type ServerError = twirp.Error

var (
	ErrConnectionTimeout        = errors.New("could not connect after timeout")
	ErrTrackPublishTimeout      = errors.New("timed out publishing track")
	ErrCannotDetermineMime      = errors.New("cannot determine mimetype from file extension")
	ErrUnsupportedFileType      = errors.New("ReaderSampleProvider does not support this mime type")
	ErrUnsupportedSimulcastKind = errors.New("simulcast is only supported for video")
	ErrInvalidSimulcastTrack    = errors.New("simulcast track was not initiated correctly")
	ErrCannotFindTrack          = errors.New("could not find the track")
	ErrInvalidMessageType       = signalling.ErrInvalidMessageType
	ErrInvalidParameter         = signalling.ErrInvalidParameter
	ErrCannotConnectSignal      = errors.New("could not establish signal connection")
	ErrNoPeerConnection         = errors.New("peer connection not established")
	ErrAborted                  = errors.New("operation was aborted")
	ErrMissingPrimaryCodec      = errors.New("primary track must be TrackLocalWithCodec when backup codec is present")
	ErrPublishRequiresSinglePC  = errors.New("WithTrack requires WithSinglePeerConnection() to be enabled")
)
