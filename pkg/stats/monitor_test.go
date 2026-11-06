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

package stats

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/config"
)

func TestCallsActiveProviderChange(t *testing.T) {
	m, err := NewMonitor(&config.Config{})
	require.NoError(t, err)
	require.NoError(t, m.Start(&config.Config{}))
	t.Cleanup(m.Stop)

	active := func(provider string) float64 {
		families, err := prometheus.DefaultGatherer.Gather()
		require.NoError(t, err)
		for _, f := range families {
			if f.GetName() != "livekit_sip_calls_active" {
				continue
			}
			for _, m := range f.GetMetric() {
				got := make(map[string]string, len(m.GetLabel()))
				for _, l := range m.GetLabel() {
					got[l.GetName()] = l.GetValue()
				}
				if got["dir"] == "in" && got["to"] == "to" && got["provider"] == provider {
					return m.GetGauge().GetValue()
				}
			}
		}
		return 0
	}
	unknownBefore := active(ProviderUnknown)
	carrierBefore := active("internal/xyz_carrier")

	c := m.NewCall(Inbound, "from", "to")
	c.CallStart()
	c.SetProvider("internal/xyz_carrier")
	c.CallEnd()

	require.Equal(t, unknownBefore, active(ProviderUnknown))
	require.Equal(t, carrierBefore, active("internal/xyz_carrier"))
}
