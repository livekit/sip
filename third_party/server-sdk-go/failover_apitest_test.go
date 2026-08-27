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

// API failover tests that exercise the public server SDK against the shared
// mock LiveKit API server (livekit/livekit cmd/test-server). Point them at a
// running instance with LK_TEST_SERVER_URL (default http://127.0.0.1:9999);
// they skip when no server is reachable. In CI the server is booted as a Docker
// container.
//
// See cmd/test-server/README.md for the X-Lk-Mock JSON control protocol. Mock
// directives are passed to the SDK as a twirp request header (see mockControl),
// which the failover transport forwards to the discovery fetch and every retry.
// These tests are in-package so they can use the internal test-force hook (the
// public API only exposes WithFailover(ctx, bool), which is cloud-gated).
package lksdk

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/livekit"
)

func testServerURL(t *testing.T) string {
	url := os.Getenv("LK_TEST_SERVER_URL")
	if url == "" {
		url = "http://127.0.0.1:9999"
	}
	resp, err := http.Get(url + "/settings/regions")
	if err != nil {
		t.Skipf("mock test server not reachable at %s (set LK_TEST_SERVER_URL): %v", url, err)
	}
	_ = resp.Body.Close()
	return url
}

// failoverCtx returns a context that forces failover on (the mock is not a
// cloud host) with a tiny backoff, carrying the given mock directives as an
// X-Lk-Mock request header. force/backoff are internal, test-only knobs.
func failoverCtx(t *testing.T, m mockControl) context.Context {
	return withMock(withFailoverForce(context.Background(), time.Millisecond), t, m)
}

func TestAPI_Healthy(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	room, err := client.CreateRoom(failoverCtx(t, mockControl{}), &livekit.CreateRoomRequest{Name: "api-test"})
	require.NoError(t, err)
	require.Equal(t, "api-test", room.Name, "the mock echoes the request name")
	require.NotEmpty(t, room.Sid)
}

func TestAPI_PrimaryUnavailable(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0}})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.NoError(t, err, "should fail over to a healthy region")
}

func TestAPI_TwoRegionsUnavailable(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0, 1}})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.NoError(t, err, "should fail over to the third region")
}

func TestAPI_AllUnavailable(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0, 1, 2, 3}})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.Error(t, err)
}

func TestAPI_ClientErrorNotRetried(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0}, FailStatus: 400})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.Error(t, err)
	var terr twirp.Error
	require.ErrorAs(t, err, &terr)
	require.Equal(t, twirp.InvalidArgument, terr.Code(), "a 4xx must surface as a typed error, not fail over")
}

func TestAPI_TransportError(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0}, FailMode: "drop"})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.NoError(t, err, "a dropped connection should fail over to a healthy region")
}

func TestAPI_RegionDiscoveryUnreachable(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0}, RegionsStatus: 500})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.Error(t, err, "no fallback hosts means the original error is surfaced")
}

func TestAPI_FailoverNotCloudHost(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	// Enabled (the default) but not forced; 127.0.0.1 is not a cloud host, so
	// failover must not engage.
	ctx := withMock(context.Background(), t, mockControl{FailRegions: []int{0}})
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.Error(t, err)
}

func TestAPI_FailoverDisabled(t *testing.T) {
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	// Forced on, but explicitly disabled via WithFailover.
	ctx := WithFailover(failoverCtx(t, mockControl{FailRegions: []int{0}}), false)
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.Error(t, err)
}

// An unresponsive region (per-attempt timeout) should fail over to a healthy
// region, with the deadline reset so the retry has its full budget. "delay" fail
// mode stalls only the failing region, unlike DelayMs which delays every region.
func TestAPI_TimeoutFailsOver(t *testing.T) {
	setMinFailoverTimeout(t, 50*time.Millisecond) // let a short test timeout fail over
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0}, FailMode: "delay"})
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.NoError(t, err, "an unresponsive region should fail over to a healthy one")
}

// A request whose timeout is below the thundering-herd threshold must not fail
// over: a stalled region surfaces the timeout instead of retrying elsewhere.
func TestAPI_ShortTimeoutNotRetried(t *testing.T) {
	setMinFailoverTimeout(t, 5*time.Second)
	client := NewRoomServiceClient(testServerURL(t), "devkey", "secret")
	ctx := failoverCtx(t, mockControl{FailRegions: []int{0}, FailMode: "delay"})
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond) // < threshold
	defer cancel()
	_, err := client.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "api-test"})
	require.Error(t, err, "a short-timeout request must not fail over to another region")
}
