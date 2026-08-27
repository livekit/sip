// Copyright 2024 LiveKit, Inc.
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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/livekit/protocol/livekit"
)

// regionSettingsServer is a fake LiveKit Cloud `/settings/regions` endpoint. It
// records how many times it was hit and serves a configurable region list.
type regionSettingsServer struct {
	server   *httptest.Server
	hostname string
	hits     atomic.Int32

	mu      sync.Mutex
	regions *livekit.RegionSettings
	status  int
}

func newRegionSettingsServer(t *testing.T, regions *livekit.RegionSettings) *regionSettingsServer {
	t.Helper()
	rs := &regionSettingsServer{
		regions: regions,
		status:  http.StatusOK,
	}
	rs.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/settings/regions", r.URL.Path)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		rs.hits.Inc()

		rs.mu.Lock()
		status, regions := rs.status, rs.regions
		rs.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		b, err := protojson.Marshal(regions)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(rs.server.Close)
	// strip scheme so it matches what parseCloudURL produces (host[:port])
	rs.hostname = strings.TrimPrefix(rs.server.URL, "https://")
	return rs
}

func (rs *regionSettingsServer) setRegions(regions *livekit.RegionSettings) {
	rs.mu.Lock()
	rs.regions = regions
	rs.mu.Unlock()
}

func newTestProvider(rs *regionSettingsServer) *regionURLProvider {
	p := newRegionURLProvider()
	// trust the httptest TLS certificate
	p.cache.client = rs.server.Client()
	return p
}

func TestRegionURLProvider_RegionSettingsOrder(t *testing.T) {
	rs := newRegionSettingsServer(t, &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{
			{Region: "us-east", Url: "wss://us-east.example.com", Distance: 10},
			{Region: "us-west", Url: "wss://us-west.example.com", Distance: 50},
			{Region: "eu", Url: "wss://eu.example.com", Distance: 100},
		},
	})
	p := newTestProvider(rs)

	want := []string{
		"wss://us-east.example.com",
		"wss://us-west.example.com",
		"wss://eu.example.com",
	}
	// RegionSettings returns the list in server order and does not mutate it, so
	// repeated calls return the same list and only fetch once.
	for n := 0; n < 2; n++ {
		settings, err := p.RegionSettings(rs.hostname, "test-token")
		require.NoError(t, err)
		var got []string
		for _, region := range settings.GetRegions() {
			got = append(got, region.Url)
		}
		require.Equal(t, want, got)
	}
	require.EqualValues(t, 1, rs.hits.Load(), "should hit /settings/regions once")
}

func TestRegionURLProvider_CacheAvoidsRefetch(t *testing.T) {
	rs := newRegionSettingsServer(t, &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a.example.com"}},
	})
	p := newTestProvider(rs)

	require.NoError(t, p.RefreshRegionSettings(rs.hostname, "test-token"))
	require.NoError(t, p.RefreshRegionSettings(rs.hostname, "test-token"))
	require.EqualValues(t, 1, rs.hits.Load(), "second refresh within cache TTL must not refetch")
}

// TestRegionURLProvider_RefreshRepopulates verifies that after exhausting all
// regions, a refresh past the cache TTL fetches /settings/regions again and the
// fallback routine can continue with a fresh list. This backs the requirement
// that retries "continue the same routine with /settings/regions".
func TestRegionURLProvider_RefreshRepopulates(t *testing.T) {
	rs := newRegionSettingsServer(t, &livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "a", Url: "wss://a.example.com"}},
	})
	p := newTestProvider(rs)

	settings, err := p.RegionSettings(rs.hostname, "test-token")
	require.NoError(t, err)
	require.Equal(t, "wss://a.example.com", settings.GetRegions()[0].Url)

	// expire the cache and serve a different region list
	p.cache.mu.Lock()
	p.cache.cache[strings.ToLower(rs.hostname)].fetchedAt = time.Now().Add(-2 * regionHostnameProviderSettingsCacheTime)
	p.cache.mu.Unlock()
	rs.setRegions(&livekit.RegionSettings{
		Regions: []*livekit.RegionInfo{{Region: "b", Url: "wss://b.example.com"}},
	})

	settings, err = p.RegionSettings(rs.hostname, "test-token")
	require.NoError(t, err)
	require.EqualValues(t, 2, rs.hits.Load(), "refresh past TTL must refetch")
	require.Equal(t, "wss://b.example.com", settings.GetRegions()[0].Url)
}

func TestRegionURLProvider_FetchError(t *testing.T) {
	rs := newRegionSettingsServer(t, &livekit.RegionSettings{})
	rs.mu.Lock()
	rs.status = http.StatusServiceUnavailable
	rs.mu.Unlock()
	p := newTestProvider(rs)

	_, err := p.RegionSettings(rs.hostname, "test-token")
	require.Error(t, err, "no regions cached after a failed fetch")
}

func TestParseRegionSettingsMaxAge(t *testing.T) {
	cases := map[string]time.Duration{
		"max-age=60":         60 * time.Second,
		"public, max-age=30": 30 * time.Second,
		"public, Max-Age=60": 60 * time.Second, // directive names are case-insensitive
		"MAX-AGE=90":         90 * time.Second,
		"no-cache":           0,
		"":                   0,
		"max-age=abc":        0,
		"max-age=0":          0,
	}
	for in, want := range cases {
		require.Equal(t, want, parseRegionSettingsMaxAge(in), "input %q", in)
	}
}
