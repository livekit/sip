// Copyright 2023 LiveKit, Inc.
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

package errors

import (
	"errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

var (
	ErrNoConfig    = psrpc.NewErrorf(psrpc.InvalidArgument, "missing config")
	ErrUnavailable = psrpc.NewErrorf(psrpc.Unavailable, "cpu exhausted")
)

func ErrCouldNotParseConfig(err error) psrpc.Error {
	return psrpc.NewErrorf(psrpc.InvalidArgument, "could not parse config: %v", err)
}

// If the provided error wraps a livekit.SIPStatus, this will apply the error code mapping to the resulting error
func ApplySIPStatus(err error) error {
	var sipStatus *livekit.SIPStatus
	if !errors.As(err, &sipStatus) {
		return err
	}
	code := sipStatus.GRPCStatus().Code()
	err = psrpc.NewError(psrpc.ErrorCodeFromGRPC(code), err, sipStatus)
	return err
}
