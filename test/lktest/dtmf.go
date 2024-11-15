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

package lktest

import (
	"context"
	"errors"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

type DTMFParticipant interface {
	SendDTMF(ctx context.Context, digits string) error
}

func CheckDTMFForParticipants(t TB, ctx context.Context, p1, p2 DTMFParticipant, data1, data2 <-chan lksdk.DataPacket) {
	checkDTMFForParticipants(t, ctx, p1, p2, data1, data2, "12345678")
	checkDTMFForParticipants(t, ctx, p2, p1, data2, data1, "abc*#")
}

func waitDTMF(ctx context.Context, data <-chan lksdk.DataPacket, digits string) error {
	var got string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data, ok := <-data:
			if !ok {
				return errors.New("data channel closed")
			}
			dtmf, ok := data.(*livekit.SipDTMF)
			if !ok {
				continue
			}
			got += dtmf.Digit
			if got == digits {
				return nil
			}
		}
	}
}

func checkDTMFForParticipants(t TB, ctx context.Context, p1, p2 DTMFParticipant, data1, data2 <-chan lksdk.DataPacket, digits string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- waitDTMF(ctx, data2, digits)
	}()
	err := p1.SendDTMF(ctx, digits)
	require.NoError(t, err)
	select {
	case <-ctx.Done():
		require.FailNow(t, "DTMF timed out")
	case err := <-errc:
		require.NoError(t, err)
	}
}
