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
	"fmt"
	"strings"

	"github.com/pion/webrtc/v4"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	ICETCPFallback = "fallback"
	ICETCPForce    = "force"
)

func (c *Config) normalizeICETCP() error {
	v := strings.ToLower(strings.TrimSpace(c.ICETCP))
	switch v {
	case "", "off", "udp":
		c.ICETCP = ""
		return nil
	case ICETCPFallback, ICETCPForce:
		c.ICETCP = v
		return nil
	default:
		return fmt.Errorf("ice_tcp must be empty, fallback, or force (got %q)", c.ICETCP)
	}
}

func (c *Config) iceNetworkTypes() []webrtc.NetworkType {
	switch c.ICETCP {
	case ICETCPFallback:
		return []webrtc.NetworkType{
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		}
	case ICETCPForce:
		return []webrtc.NetworkType{
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		}
	default:
		return nil
	}
}

// ICEConnectOptions enables ICE-TCP to the SFU when ice_tcp is set.
func (c *Config) ICEConnectOptions() []lksdk.ConnectOption {
	types := c.iceNetworkTypes()
	if len(types) == 0 {
		return nil
	}
	return []lksdk.ConnectOption{lksdk.WithICENetworkTypes(types...)}
}
