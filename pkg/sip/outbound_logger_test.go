// Copyright 2026 LiveKit, Inc.
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

package sip

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/sip"
)

func TestOutboundLoggerEnrichmentDuringStats(t *testing.T) {
	h := NewTestSIP(t, TestSIPConfig{})
	req := MinimalCreateSIPParticipantRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	created := make(chan error, 1)
	go func() {
		_, err := h.Client.CreateSIPParticipant(ctx, req)
		created <- err
	}()
	tr := h.WaitTransaction(t, time.Second, req.SipCallId, "")
	h.Client.cmu.Lock()
	var call *outboundCall
	for _, active := range h.Client.activeCalls {
		call = active
	}
	h.Client.cmu.Unlock()
	require.NotNil(t, call)

	// Keep the stats callback active while sipSignal enriches the logger from
	// the final response, rather than testing only the lock's accessors.
	stop := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				call.printStats()
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	<-started
	response := sip.NewSDPResponseFromRequest(tr.req, []byte(testMinimalSDP))
	response.AppendHeader(sip.NewHeader("X-call_leg_id", "test-leg"))
	require.NoError(t, tr.transaction.SendResponse(response))
	ack := h.WaitRequest(t, time.Second, req.SipCallId, "")
	require.Equal(t, sip.ACK, ack.req.Method)
	select {
	case err := <-created:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("outbound call did not finish setup")
	}
}
