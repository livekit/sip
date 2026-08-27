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

package config

import (
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestNormalizeICETCP(t *testing.T) {
	c := &Config{ICETCP: " Fallback "}
	require.NoError(t, c.normalizeICETCP())
	require.Equal(t, ICETCPFallback, c.ICETCP)

	c.ICETCP = "FORCE"
	require.NoError(t, c.normalizeICETCP())
	require.Equal(t, ICETCPForce, c.ICETCP)

	c.ICETCP = "off"
	require.NoError(t, c.normalizeICETCP())
	require.Equal(t, "", c.ICETCP)

	c.ICETCP = "relay"
	require.Error(t, c.normalizeICETCP())
}

func TestICENetworkTypes(t *testing.T) {
	require.Nil(t, (&Config{}).iceNetworkTypes())

	fb := (&Config{ICETCP: ICETCPFallback}).iceNetworkTypes()
	require.Equal(t, []webrtc.NetworkType{
		webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
		webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
	}, fb)

	force := (&Config{ICETCP: ICETCPForce}).iceNetworkTypes()
	require.Equal(t, []webrtc.NetworkType{
		webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
	}, force)

	require.Nil(t, (&Config{}).ICEConnectOptions())
	require.Len(t, (&Config{ICETCP: ICETCPFallback}).ICEConnectOptions(), 1)
}

func TestICETCPYAML(t *testing.T) {
	var c Config
	require.NoError(t, yaml.Unmarshal([]byte("ice_tcp: force\n"), &c))
	require.Equal(t, "force", c.ICETCP)
	require.NoError(t, c.normalizeICETCP())
	require.Equal(t, ICETCPForce, c.ICETCP)
}
