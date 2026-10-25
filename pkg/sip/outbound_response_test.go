// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"fmt"
	"testing"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// Reliable transactions may finish immediately after queuing a final response.
// Both channels can therefore be ready when the consumer is scheduled.
func TestSIPResponseConsumesBufferedFinalAfterDone(t *testing.T) {
	for _, code := range []sip.StatusCode{200, 401, 403, 407} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			for range 100 {
				final := &sip.Response{StatusCode: code}
				tx := &testSIPClientTransaction{
					log:       logger.NewTestLogger(t),
					responses: make(chan *sip.Response, 2),
					done:      make(chan struct{}),
				}
				tx.responses <- &sip.Response{StatusCode: 100}
				tx.responses <- final
				close(tx.responses)
				close(tx.done)
				var statuses []sip.StatusCode
				response, err := sipResponse(context.Background(), tx, nil, func(status sip.StatusCode, _ Headers) {
					statuses = append(statuses, status)
				})
				require.NoError(t, err)
				require.Same(t, final, response)
				require.Equal(t, []sip.StatusCode{100, code}, statuses)
			}
		})
	}
}

func TestSIPResponseDoneWithoutFinal(t *testing.T) {
	for _, provisional := range []bool{false, true} {
		t.Run(fmt.Sprint(provisional), func(t *testing.T) {
			tx := &testSIPClientTransaction{log: logger.NewTestLogger(t), responses: make(chan *sip.Response, 1), done: make(chan struct{})}
			if provisional {
				tx.responses <- &sip.Response{StatusCode: 100}
			}
			close(tx.responses)
			close(tx.done)
			response, err := sipResponse(context.Background(), tx, nil, nil)
			require.Error(t, err)
			require.Nil(t, response)
			var incomplete transactionTimeoutError
			require.ErrorAs(t, err, &incomplete)
			if provisional {
				require.Equal(t, 1, incomplete.responses)
			} else {
				require.Zero(t, incomplete.responses)
			}
		})
	}
}
