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
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
)

type testCase struct {
	idleTimeout     time.Duration
	maxDuration     time.Duration
	sendDuration    time.Duration
	expectedRelease time.Duration
}

var testCases = []testCase{
	{
		idleTimeout:     0,
		maxDuration:     0,
		sendDuration:    0,
		expectedRelease: 0,
	},
	{
		idleTimeout:     0,
		maxDuration:     0,
		sendDuration:    10 * time.Millisecond,
		expectedRelease: 0,
	},
	{
		idleTimeout:     0,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    5 * time.Millisecond,
		expectedRelease: 25 * time.Millisecond,
	},
	{
		idleTimeout:     0,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    50 * time.Millisecond,
		expectedRelease: 25 * time.Millisecond,
	},
	{
		idleTimeout:     5 * time.Millisecond,
		maxDuration:     0,
		sendDuration:    0,
		expectedRelease: 5 * time.Millisecond,
	},
	{
		idleTimeout:     5 * time.Millisecond,
		maxDuration:     0,
		sendDuration:    10 * time.Millisecond,
		expectedRelease: 15 * time.Millisecond,
	},
	{
		idleTimeout:     5 * time.Millisecond,
		maxDuration:     0,
		sendDuration:    35 * time.Millisecond,
		expectedRelease: 40 * time.Millisecond,
	},
	{
		idleTimeout:     5 * time.Millisecond,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    0,
		expectedRelease: 5 * time.Millisecond,
	},
	{
		idleTimeout:     5 * time.Millisecond,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    10 * time.Millisecond,
		expectedRelease: 15 * time.Millisecond,
	},
	{
		idleTimeout:     5 * time.Millisecond,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    35 * time.Millisecond,
		expectedRelease: 25 * time.Millisecond,
	},
	{
		idleTimeout:     30 * time.Millisecond,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    0,
		expectedRelease: 25 * time.Millisecond,
	},
	{
		idleTimeout:     30 * time.Millisecond,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    15 * time.Millisecond,
		expectedRelease: 25 * time.Millisecond,
	},
	{
		idleTimeout:     30 * time.Millisecond,
		maxDuration:     25 * time.Millisecond,
		sendDuration:    35 * time.Millisecond,
		expectedRelease: 25 * time.Millisecond,
	},
}

func TestDrainPort(t *testing.T) {
	interval := time.Millisecond
	fuzz := 2500 * time.Microsecond
	for _, testCase := range testCases {
		testCase.idleTimeout *= 2
		testCase.maxDuration *= 2
		testCase.sendDuration *= 2
		testCase.expectedRelease *= 2

		t.Run(fmt.Sprintf("idleTimeout=%s/maxDuration=%s/sendDuration=%s", testCase.idleTimeout, testCase.maxDuration, testCase.sendDuration), func(t *testing.T) {
			conn, port := drainLoopback(t)
			start := time.Now()
			done := make(chan struct{})
			errors := make(chan error, 1)
			go DrainPort(logger.NewTestLogger(t), conn, testCase.idleTimeout, testCase.maxDuration, done)
			go generateTraffic(t, conn.LocalAddr().(*net.UDPAddr), interval, testCase.sendDuration, errors)
			if testCase.expectedRelease > 0 {
				require.False(t, portRebindable(t, port), "port was rebindable while still draining")
			}
			maxWait := testCase.idleTimeout + testCase.maxDuration + testCase.sendDuration + interval
			elapsed := awaitDrain(t, start, done, maxWait)
			require.NoError(t, <-errors, "failed to generate traffic")
			require.GreaterOrEqual(t, elapsed, testCase.expectedRelease-fuzz, "released too early")
			require.LessOrEqual(t, elapsed, testCase.expectedRelease+fuzz, "released too late")
			require.True(t, portRebindable(t, port), "port not released after drain")
		})
	}
}

func drainLoopback(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	return c, c.LocalAddr().(*net.UDPAddr).Port
}

// awaitDrain waits for done and returns how long it took, failing if it exceeds ceiling.
func awaitDrain(t *testing.T, start time.Time, done <-chan struct{}, ceiling time.Duration) time.Duration {
	t.Helper()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(ceiling):
		t.Fatalf("DrainPort did not release within %s", ceiling)
		return 0
	}
}

// portRebindable reports whether the given port can be bound again (i.e. the drained
// socket has actually been closed and the port released).
func portRebindable(t *testing.T, port int) bool {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func generateTraffic(t *testing.T, addr *net.UDPAddr, frequency time.Duration, duration time.Duration, errors chan<- error) {
	t.Helper()
	defer close(errors)
	peer, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		errors <- err
		return
	}
	defer peer.Close()
	tk := time.NewTicker(frequency)
	defer tk.Stop()

	deadline := time.Now().Add(duration)
	for {
		select {
		case <-time.After(time.Until(deadline)):
			return
		case <-tk.C:
			_, _ = peer.Write([]byte("x"))
		}
	}
}
