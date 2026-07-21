// Copyright 2023 LiveKit, Inc.
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

package main

import (
	"net"
	"testing"

	"github.com/livekit/sip/pkg/config"
)

func TestFirstIPv4(t *testing.T) {
	cases := []struct {
		name  string
		addrs []net.Addr
		want  string
		ok    bool
	}{
		{
			name:  "empty",
			addrs: nil,
			want:  "",
			ok:    false,
		},
		{
			name: "ipv6 only",
			addrs: []net.Addr{
				&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			},
			want: "",
			ok:   false,
		},
		{
			name: "picks first ipv4",
			addrs: []net.Addr{
				&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
				&net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("10.0.0.9"), Mask: net.CIDRMask(24, 32)},
			},
			want: "10.0.0.5",
			ok:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := firstIPv4(c.addrs)
			if got != c.want || ok != c.ok {
				t.Fatalf("firstIPv4() = (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestApplyNodeIPIface(t *testing.T) {
	t.Run("env unset is a no-op", func(t *testing.T) {
		t.Setenv("LIVEKIT_NODE_IP_IFACE", "")
		conf := &config.Config{ListenIP: ""}
		if err := applyNodeIPIface(conf); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conf.ListenIP != "" {
			t.Fatalf("ListenIP = %q, want empty", conf.ListenIP)
		}
	})

	t.Run("explicit listen_ip is preserved", func(t *testing.T) {
		t.Setenv("LIVEKIT_NODE_IP_IFACE", "eth0")
		conf := &config.Config{ListenIP: "10.1.2.3"}
		if err := applyNodeIPIface(conf); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conf.ListenIP != "10.1.2.3" {
			t.Fatalf("ListenIP = %q, want 10.1.2.3", conf.ListenIP)
		}
	})

	t.Run("unknown interface errors", func(t *testing.T) {
		t.Setenv("LIVEKIT_NODE_IP_IFACE", "definitely-not-an-iface-xyz")
		conf := &config.Config{ListenIP: ""}
		if err := applyNodeIPIface(conf); err == nil {
			t.Fatal("expected error for unknown interface, got nil")
		}
	})
}
