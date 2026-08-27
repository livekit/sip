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

package lksdk

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/server-sdk-go/v2/signalling"
)

// regionNames / regionURLs extract the ordered region identifiers from a plan so
// tests can assert on the failover order concisely.
func planRegionNames(plan []connectionAttemptParams) []string {
	names := make([]string, 0, len(plan))
	for _, p := range plan {
		names = append(names, p.region.Region)
	}
	return names
}

func planRegionURLs(plan []connectionAttemptParams) []string {
	urls := make([]string, 0, len(plan))
	for _, p := range plan {
		urls = append(urls, p.region.Url)
	}
	return urls
}

// newTestConnectionManager builds a connection manager with the incoming request
// params set. Region discovery is served from the provider cache (seeded per
// test), so no HTTP endpoint is required.
func newTestConnectionManager(url string, disableRegionDiscovery bool) *connectionManager {
	cm := newConnectionManager(newRegionURLProvider())
	cm.setIncomingRequestParams(
		context.Background(),
		url,
		"test-token",
		&signalling.ConnectParams{},
		disableRegionDiscovery,
	)
	return cm
}

func TestConnectionManager_InitialState(t *testing.T) {
	cm := newConnectionManager(newRegionURLProvider())
	require.Equal(t, connectionManagerStateInitial, cm.state)
	require.False(t, cm.isReconnectingState())
}

func TestConnectionManager_StateTransitions(t *testing.T) {
	cm := newConnectionManager(newRegionURLProvider())

	settings := &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "us-west", Url: "wss://us-west.example.com"}},
	}

	// a reconnect must not start from the initial state (it cannot race the
	// initial join), so setReconnecting is ignored and the state stays Initial
	cm.setReconnecting(settings)
	require.Equal(t, connectionManagerStateInitial, cm.state)
	require.Nil(t, cm.regionSettings)
	require.False(t, cm.isReconnectingState())

	region := &livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.example.com"}
	cm.setConnected(region)
	require.Equal(t, connectionManagerStateConnected, cm.state)
	require.False(t, cm.isReconnectingState())

	cm.setResuming(settings)
	require.Equal(t, connectionManagerStateResuming, cm.state)
	require.False(t, cm.isReconnectingState())

	cm.setReconnecting(settings)
	require.Equal(t, connectionManagerStateReconnecting, cm.state)
	require.True(t, cm.isReconnectingState())

	cm.setClosed()
	require.Equal(t, connectionManagerStateClosed, cm.state)
	require.Nil(t, cm.regionSettings)
	require.False(t, cm.isReconnectingState())
}

// TestConnectionManager_SetConnectedClonesAndResets verifies setConnected deep
// clones the region (so later mutation of the caller's copy is not observed) and
// clears any region settings carried over from a prior leave request.
func TestConnectionManager_SetConnectedClonesAndResets(t *testing.T) {
	cm := newConnectionManager(newRegionURLProvider())

	// leftover region settings from a previous session: reach Resuming (which
	// requires being Connected first) so regionSettings is populated
	cm.setConnected(&livekit.RegionInfo{Region: "prev", Url: "wss://prev.example.com"})
	cm.setResuming(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "stale", Url: "wss://stale.example.com"}},
	})
	require.NotNil(t, cm.regionSettings)

	region := &livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.example.com"}
	cm.setConnected(region)

	require.Nil(t, cm.regionSettings, "region settings must be reset on connect")
	require.NotNil(t, cm.connectedRegion)
	require.Equal(t, "us-east", cm.connectedRegion.Region)

	// mutating the caller's copy must not affect the stored (cloned) region
	region.Url = "wss://mutated.example.com"
	require.Equal(t, "wss://us-east.example.com", cm.connectedRegion.Url)
}

func TestConnectionManager_GetConnectParamsRoundTrip(t *testing.T) {
	cm := newConnectionManager(newRegionURLProvider())
	cp := &signalling.ConnectParams{AutoSubscribe: true}
	cm.setIncomingRequestParams(context.Background(), "wss://example.com", "tok", cp, false)

	require.Equal(t, true, cm.getConnectParams().AutoSubscribe)
	require.Equal(t, "tok", cm.token)

	cm.setToken("tok2")
	require.Equal(t, "tok2", cm.token)
}

func TestConnectionManager_GetConnectionPlan_Errors(t *testing.T) {
	// url not set
	cm := newConnectionManager(newRegionURLProvider())
	_, err := cm.getConnectionPlan()
	require.Error(t, err)
	require.Contains(t, err.Error(), "original url not set")

	// url set but state is not a plannable one (e.g. Connected)
	cm = newTestConnectionManager("wss://example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "us-east", Url: "wss://example.com"})
	_, err = cm.getConnectionPlan()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid state")
}

func TestConnectionManager_GetConnectionPlanInitial_NoDiscovery(t *testing.T) {
	cm := newTestConnectionManager("wss://example.com", true /* disableRegionDiscovery */)

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Len(t, plan, 1)
	require.Equal(t, cOriginalRegion, plan[0].region.Region)
	require.Equal(t, "wss://example.com", plan[0].region.Url)
	require.EqualValues(t, -1, plan[0].region.Distance)
	require.Equal(t, time.Duration(0), plan[0].backoffWait)
	require.Equal(t, "test-token", plan[0].token)
	require.Equal(t, cValidateTimeout, plan[0].validateTimeout)
}

// TestConnectionManager_GetConnectionPlanInitial_NonCloudURL: a non-cloud URL
// yields no region discovery even when discovery is enabled, so only the
// original URL is planned.
func TestConnectionManager_GetConnectionPlanInitial_NonCloudURL(t *testing.T) {
	cm := newTestConnectionManager("wss://example.com", false /* discovery enabled */)

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{cOriginalRegion}, planRegionNames(plan))
}

func TestConnectionManager_GetConnectionPlanInitial_WithDiscovery(t *testing.T) {
	cm := newTestConnectionManager("wss://test.livekit.cloud", false /* discovery enabled */)
	cm.regionProvider.cache.set("test.livekit.cloud", &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{
			{Region: "us-east", Url: "wss://us-east.livekit.cloud"},
			{Region: "us-west", Url: "wss://us-west.livekit.cloud"},
		},
	}, time.Minute)

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"us-east", "us-west", cOriginalRegion}, planRegionNames(plan))
	require.Equal(t, "wss://test.livekit.cloud", plan[2].region.Url)

	// backoff schedule: first attempt has no wait, subsequent ones grow exponentially
	require.Equal(t, time.Duration(0), plan[0].backoffWait)
	require.Equal(t, 100*time.Millisecond, plan[1].backoffWait)
	require.Equal(t, 200*time.Millisecond, plan[2].backoffWait)
}

func TestConnectionManager_GetConnectionPlanResuming_WithRegionSettings(t *testing.T) {
	cm := newTestConnectionManager("wss://test.livekit.cloud", true)
	cm.setConnected(&livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.livekit.cloud"})
	cm.setResuming(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{
			{Region: "us-east", Url: "wss://us-east.livekit.cloud"},
			{Region: "us-west", Url: "wss://us-west.livekit.cloud"},
		},
	})

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"us-east", "us-west", cOriginalRegion}, planRegionNames(plan))
}

// TestConnectionManager_GetConnectionPlanResuming_NoRegionSettings: without a
// server-sent region list, resume retries the connected region, then the
// original URL.
func TestConnectionManager_GetConnectionPlanResuming_NoRegionSettings(t *testing.T) {
	cm := newTestConnectionManager("wss://test.livekit.cloud", true)
	cm.setConnected(&livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.livekit.cloud"})
	cm.setResuming(nil) // no server list

	require.Nil(t, cm.regionSettings)

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"us-east", cOriginalRegion}, planRegionNames(plan))
	require.Equal(t, "wss://us-east.livekit.cloud", plan[0].region.Url)
}

func TestConnectionManager_GetConnectionPlanReconnecting_MergesSources(t *testing.T) {
	cm := newTestConnectionManager("wss://test.livekit.cloud", false /* discovery enabled */)
	cm.regionProvider.cache.set("test.livekit.cloud", &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{
			{Region: "us-west", Url: "wss://us-west.livekit.cloud"},
			{Region: "eu", Url: "wss://eu.livekit.cloud"},
		},
	}, time.Minute)

	cm.setConnected(&livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.livekit.cloud"})
	cm.setReconnecting(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{
			{Region: "us-east", Url: "wss://us-east.livekit.cloud"},
		},
	})

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	// server-sent list first, then discovery regions, then original; deduped by region name
	require.Equal(t, []string{"us-east", "us-west", "eu", cOriginalRegion}, planRegionNames(plan))
}

// TestConnectionManager_SetResumingIgnoredWhileReconnecting verifies a resume
// request (e.g. a RESUME leave or transport-close) arriving while a full
// reconnect is in progress does NOT downgrade the state to Resuming, and does
// not clobber the reconnect's region settings.
func TestConnectionManager_SetResumingIgnoredWhileReconnecting(t *testing.T) {
	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.example.com"})
	cm.setReconnecting(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "reconnect", Url: "wss://reconnect.example.com"}},
	})
	require.Equal(t, connectionManagerStateReconnecting, cm.state)

	// attempt to downgrade with a different region list
	cm.setResuming(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "resume", Url: "wss://resume.example.com"}},
	})

	require.Equal(t, connectionManagerStateReconnecting, cm.state, "resume must not downgrade an in-progress reconnect")
	require.Equal(t, "reconnect", cm.regionSettings.GetRegions()[0].Region, "reconnect region settings must be preserved")
}

// TestConnectionManager_ClosedIsTerminal verifies that once Closed, no set*
// transition can move the state away from it. This guards against an in-flight
// reconnect (setConnected/setResuming/setReconnecting/setResumed) resurrecting a
// connection after Close() has marked it closed.
func TestConnectionManager_ClosedIsTerminal(t *testing.T) {
	region := &livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.example.com"}
	settings := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}}}

	newClosed := func() *connectionManager {
		cm := newTestConnectionManager("wss://original.example.com", true)
		cm.setConnected(region)
		cm.setClosed()
		require.Equal(t, connectionManagerStateClosed, cm.state)
		return cm
	}

	cm := newClosed()
	cm.setConnected(region)
	require.Equal(t, connectionManagerStateClosed, cm.state)

	cm = newClosed()
	cm.setResuming(settings)
	require.Equal(t, connectionManagerStateClosed, cm.state)

	cm = newClosed()
	cm.setReconnecting(settings)
	require.Equal(t, connectionManagerStateClosed, cm.state)

	cm = newClosed()
	cm.setResumed(region)
	require.Equal(t, connectionManagerStateClosed, cm.state)
}

// TestConnectionManager_RequestRecoveryStartsSingleWorker verifies that only the
// first requestRecovery for an idle manager claims the worker slot; while a
// worker is running, further requests apply the state but do not start a second
// worker.
func TestConnectionManager_RequestRecoveryStartsSingleWorker(t *testing.T) {
	regions := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}}}

	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "init", Url: "wss://init"})

	require.True(t, cm.requestRecovery(true /* fullReconnect */, regions), "first request must start a worker")
	require.True(t, cm.isRecovering())
	require.Equal(t, connectionManagerStateReconnecting, cm.state)

	require.False(t, cm.requestRecovery(true, regions), "worker already running, must not start a second")
	require.True(t, cm.isRecovering())

	// a reconnect requested from the initial state cannot start a worker
	cmInit := newTestConnectionManager("wss://original.example.com", true)
	require.False(t, cmInit.requestRecovery(true, regions))
	require.False(t, cmInit.isRecovering())
	require.Equal(t, connectionManagerStateInitial, cmInit.state)
}

// TestConnectionManager_RecoveryWorkerNotOrphaned verifies that a recovery state
// transition is never left without a worker to act on it, regardless of whether a
// new disconnect lands before or after the worker re-checks on settling.
func TestConnectionManager_RecoveryWorkerNotOrphaned(t *testing.T) {
	regions := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}}}
	region := &livekit.RegionInfo{Region: "a", Url: "wss://a"}

	// Ordering A: a new disconnect arrives after the worker settled the state to
	// Connected but before it reconciles. requestRecovery finds a worker still
	// running (no second worker), and the worker's reconcile sees pending work and
	// keeps running.
	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "init", Url: "wss://init"})
	require.True(t, cm.requestRecovery(false /* resume */, regions))

	cm.setResumed(region) // worker resumed successfully -> Connected
	require.False(t, cm.requestRecovery(false, nil), "worker still running; no second worker")
	require.Equal(t, connectionManagerStateResuming, cm.state)
	require.True(t, cm.recoveryWorkerShouldContinue(), "pending work -> worker keeps running")
	require.True(t, cm.isRecovering())

	// Ordering B: the worker reconciles first (settles, releasing the slot), then
	// the new disconnect arrives and must start a fresh worker.
	cm = newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "init", Url: "wss://init"})
	require.True(t, cm.requestRecovery(false, regions))

	cm.setResumed(region) // -> Connected
	require.False(t, cm.recoveryWorkerShouldContinue(), "settled -> worker exits")
	require.False(t, cm.isRecovering())
	require.True(t, cm.requestRecovery(false, nil), "new disconnect must re-arm a worker")
	require.True(t, cm.isRecovering())
	require.Equal(t, connectionManagerStateResuming, cm.state)
}

// TestConnectionManager_SetResumingRequiresConnected verifies resume is a no-op
// unless the manager is currently Connected.
func TestConnectionManager_SetResumingRequiresConnected(t *testing.T) {
	settings := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}}}

	// from Initial
	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setResuming(settings)
	require.Equal(t, connectionManagerStateInitial, cm.state)
	require.Nil(t, cm.regionSettings)

	// from Closed
	cm = newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "us-east", Url: "wss://us-east.example.com"})
	cm.setClosed()
	cm.setResuming(settings)
	require.Equal(t, connectionManagerStateClosed, cm.state)
}

// TestConnectionManager_ConsecutiveResumesUseFreshRegions verifies that once a
// successful resume restores the Connected state (via setResumed), the next
// resume starts from Connected and adopts the fresh server-provided region list.
func TestConnectionManager_ConsecutiveResumesUseFreshRegions(t *testing.T) {
	regionsA := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}}}
	regionsB := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "b", Url: "wss://b"}}}

	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "init", Url: "wss://init"})

	// first resume with region list A
	cm.setResuming(regionsA)
	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"a", cOriginalRegion}, planRegionNames(plan))

	// a successful resume restores the Connected state via setResumed
	cm.setResumed(&livekit.RegionInfo{Region: "a", Url: "wss://a"})
	require.Equal(t, connectionManagerStateConnected, cm.state)
	require.Equal(t, "a", cm.connectedRegion.Region, "connectedRegion tracks the resumed region")
	require.Nil(t, cm.regionSettings, "region settings reset on connect")

	// second resume carries a fresh region list B, which must be applied
	cm.setResuming(regionsB)
	plan, err = cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"b", cOriginalRegion}, planRegionNames(plan))
}

// TestConnectionManager_SetResumedIgnoredWhenReconnectPending verifies that if a
// reconnect was requested while a resume was in flight (state moved to
// Reconnecting), a successful resume's setResumed does NOT clobber it back to
// Connected — the pending full reconnect must survive.
func TestConnectionManager_SetResumedIgnoredWhenReconnectPending(t *testing.T) {
	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "init", Url: "wss://init"})
	cm.setResuming(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}},
	})

	// a reconnect leave arrives mid-resume -> upgrade to Reconnecting
	reconnectRegions := &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "reconnect", Url: "wss://reconnect"}},
	}
	cm.setReconnecting(reconnectRegions)
	require.Equal(t, connectionManagerStateReconnecting, cm.state)

	// resume then completes "successfully" and tries to restore Connected
	cm.setResumed(&livekit.RegionInfo{Region: "a", Url: "wss://a"})

	require.Equal(t, connectionManagerStateReconnecting, cm.state, "pending reconnect must not be clobbered")
	require.Equal(t, "reconnect", cm.regionSettings.GetRegions()[0].Region, "reconnect region settings preserved")
}

// TestConnectionManager_SetResumingWhileResumingUpdatesRegions verifies that a
// resume request arriving while already Resuming keeps the state Resuming but
// adopts the newer server-provided region list, and that a nil list is ignored
// (some internal resumes carry no regions) so the list already in effect is not
// wiped.
func TestConnectionManager_SetResumingWhileResumingUpdatesRegions(t *testing.T) {
	regionsA := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a"}}}
	regionsB := &livekit.RegionSettings{Regions: []*livekit.RegionInfo{{Region: "b", Url: "wss://b"}}}

	cm := newTestConnectionManager("wss://original.example.com", true)
	cm.setConnected(&livekit.RegionInfo{Region: "init", Url: "wss://init"})

	// first resume transitions Connected -> Resuming and adopts list A
	cm.setResuming(regionsA)
	require.Equal(t, connectionManagerStateResuming, cm.state)

	// a second resume while still Resuming keeps the state Resuming but consumes
	// the newer non-nil region list
	cm.setResuming(regionsB)
	require.Equal(t, connectionManagerStateResuming, cm.state)

	plan, err := cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"b", cOriginalRegion}, planRegionNames(plan), "newer region list must be consumed while resuming")

	// a nil list while resuming is ignored, preserving the list already in effect
	cm.setResuming(nil)
	require.Equal(t, connectionManagerStateResuming, cm.state)

	plan, err = cm.getConnectionPlan()
	require.NoError(t, err)
	require.Equal(t, []string{"b", cOriginalRegion}, planRegionNames(plan), "nil list must not wipe the region list in effect")
}

// TestConnectionManager_BuildConnectionPlan_Dedup verifies regions are deduped
// by region name, keeping first occurrence order.
func TestConnectionManager_BuildConnectionPlan_Dedup(t *testing.T) {
	plan, err := buildConnectionPlan(context.Background(), []*livekit.RegionInfo{
		{Region: "a", Url: "wss://a1.example.com"},
		{Region: "b", Url: "wss://b.example.com"},
		{Region: "a", Url: "wss://a2.example.com"}, // duplicate name, dropped
		{Region: "c", Url: "wss://c.example.com"},
	}, "tok")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, planRegionNames(plan))
	require.Equal(t, []string{"wss://a1.example.com", "wss://b.example.com", "wss://c.example.com"}, planRegionURLs(plan))
	for _, p := range plan {
		require.Equal(t, "tok", p.token)
	}
}

// TestConnectionManager_BuildConnectionPlan_Backoff verifies the exponential
// backoff schedule and its 6.4s cap.
func TestConnectionManager_BuildConnectionPlan_Backoff(t *testing.T) {
	var regions []*livekit.RegionInfo
	for i := 0; i < 10; i++ {
		regions = append(regions, &livekit.RegionInfo{
			Region: string(rune('a' + i)),
			Url:    "wss://r.example.com",
		})
	}

	plan, err := buildConnectionPlan(context.Background(), regions, "tok")
	require.NoError(t, err)
	require.Len(t, plan, 10)

	want := []time.Duration{
		0,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		3200 * time.Millisecond,
		6400 * time.Millisecond,
		6400 * time.Millisecond, // capped
		6400 * time.Millisecond, // capped
	}
	for i, w := range want {
		require.Equal(t, w, plan[i].backoffWait, "idx %d", i)
	}
}
