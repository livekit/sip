// Copyright 2026 LiveKit, Inc.
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

package sip

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const challengeTimerWait = 50 * time.Millisecond

func TestChallengeTimerFires(t *testing.T) {
	var c inboundCallInfo
	fired := make(chan struct{})
	c.startChallengeTimer(time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("timer did not fire")
	}
	c.Lock()
	require.Nil(t, c.challengeTimer)
	c.Unlock()
}

func TestChallengeTimerStop(t *testing.T) {
	var c inboundCallInfo
	var fired atomic.Int32
	c.startChallengeTimer(10*time.Millisecond, func() { fired.Add(1) })
	c.stopChallengeTimer()
	time.Sleep(challengeTimerWait)
	require.Zero(t, fired.Load())

	// Stopping with nothing pending is a no-op.
	c.stopChallengeTimer()
}

func TestChallengeTimerReplace(t *testing.T) {
	var c inboundCallInfo
	var first, second atomic.Int32
	c.startChallengeTimer(10*time.Millisecond, func() { first.Add(1) })
	c.startChallengeTimer(10*time.Millisecond, func() { second.Add(1) })
	time.Sleep(challengeTimerWait)
	require.Zero(t, first.Load())
	require.Equal(t, int32(1), second.Load())
}

// A timer that fires while it is being stopped must not call fire.
func TestChallengeTimerStoppedWhileFiring(t *testing.T) {
	var c inboundCallInfo
	var fired atomic.Int32
	c.startChallengeTimer(time.Millisecond, func() { fired.Add(1) })

	// Hold the lock across expiry, so the timer's callback is blocked on it, as when a
	// new INVITE stops the timer at the moment it fires.
	c.Lock()
	time.Sleep(challengeTimerWait)
	c.stopChallengeTimerLocked()
	c.Unlock()

	time.Sleep(challengeTimerWait)
	require.Zero(t, fired.Load())
}
