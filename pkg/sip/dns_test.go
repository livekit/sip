// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeDNS is a DNSResolver backed by static records. Lookups that miss return
// NXDOMAIN, so a zero fakeDNS resolves nothing and keeps tests off the network.
type fakeDNS struct {
	srv  map[string][]*net.SRV   // keyed by the full name, e.g. "_sip._udp.example.com"
	addr map[string][]net.IPAddr // keyed by host
}

func (f fakeDNS) LookupSRV(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	full := "_" + service + "._" + proto + "." + name
	recs, ok := f.srv[full]
	if !ok {
		return "", nil, &net.DNSError{Err: "no such host", Name: full, IsNotFound: true}
	}
	return full, recs, nil
}

func (f fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := f.addr[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

func ipAddrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out
}

func TestResolveNextHop(t *testing.T) {
	dns := fakeDNS{
		srv: map[string][]*net.SRV{
			"_sip._udp.example.com":  {{Target: "udp1.example.com.", Port: 5080}},
			"_sip._tcp.example.com":  {{Target: "tcp1.example.com.", Port: 5081}},
			"_sips._tcp.example.com": {{Target: "tls1.example.com.", Port: 5082}},
			// First target does not resolve, so the second one wins.
			"_sip._udp.failover.com": {
				{Target: "missing.example.com.", Port: 5090},
				{Target: "udp1.example.com.", Port: 5091},
			},
			// RFC 2782 "service decidedly not available at this domain".
			"_sip._udp.nosrv.com": {{Target: ".", Port: 0}},
		},
		addr: map[string][]net.IPAddr{
			"example.com":      ipAddrs("192.0.2.1"),
			"udp1.example.com": ipAddrs("192.0.2.10"),
			"tcp1.example.com": ipAddrs("192.0.2.11"),
			"tls1.example.com": ipAddrs("192.0.2.12"),
			"failover.com":     ipAddrs("192.0.2.2"),
			"nosrv.com":        ipAddrs("192.0.2.3"),
			"plain.com":        ipAddrs("192.0.2.4", "192.0.2.5"),
			"v6.example.com":   ipAddrs("2001:db8::1"),
		},
	}

	cases := []struct {
		name      string
		host      string
		port      int
		transport string
		exp       string
		expErr    bool
	}{
		{name: "ip literal", host: "192.0.2.1", transport: "UDP", exp: "192.0.2.1:5060"},
		{name: "ip literal with port", host: "192.0.2.1", port: 5080, transport: "UDP", exp: "192.0.2.1:5080"},
		{name: "ip literal tls default port", host: "192.0.2.1", transport: "TLS", exp: "192.0.2.1:5061"},
		{name: "ipv6 literal", host: "2001:db8::1", transport: "UDP", exp: "[2001:db8::1]:5060"},

		// An explicit port means the hop is already chosen: RFC 3263 sec 4.2 says
		// not to look at SRV, even though example.com publishes records.
		{name: "explicit port skips srv", host: "example.com", port: 5070, transport: "UDP", exp: "192.0.2.1:5070"},

		{name: "srv udp", host: "example.com", transport: "UDP", exp: "192.0.2.10:5080"},
		{name: "srv tcp", host: "example.com", transport: "TCP", exp: "192.0.2.11:5081"},
		{name: "srv tls uses _sips._tcp", host: "example.com", transport: "TLS", exp: "192.0.2.12:5082"},

		{name: "srv target that does not resolve is skipped", host: "failover.com", transport: "UDP", exp: "192.0.2.10:5091"},
		{name: "dot target falls back to address lookup", host: "nosrv.com", transport: "UDP", exp: "192.0.2.3:5060"},

		{name: "no srv falls back to address lookup", host: "plain.com", transport: "UDP", exp: "192.0.2.4:5060"},
		{name: "no srv tls fallback uses 5061", host: "plain.com", transport: "TLS", exp: "192.0.2.4:5061"},
		{name: "ipv6 address record", host: "v6.example.com", transport: "UDP", exp: "[2001:db8::1]:5060"},

		{name: "unresolvable", host: "nowhere.com", transport: "UDP", expErr: true},
		{name: "unresolvable with explicit port", host: "nowhere.com", port: 5060, transport: "UDP", expErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveNextHop(context.Background(), dns, c.host, c.port, c.transport)
			if c.expErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.exp, got)
		})
	}
}

func TestSRVLabels(t *testing.T) {
	cases := []struct {
		transport string
		service   string
		proto     string
		ok        bool
	}{
		{transport: "UDP", service: "sip", proto: "udp", ok: true},
		{transport: "udp", service: "sip", proto: "udp", ok: true},
		{transport: "TCP", service: "sip", proto: "tcp", ok: true},
		{transport: "TLS", service: "sips", proto: "tcp", ok: true},
		{transport: "WS", service: "sip", proto: "ws", ok: true},
		{transport: "WSS", service: "sips", proto: "wss", ok: true},
		{transport: "sctp", ok: false},
	}
	for _, c := range cases {
		t.Run(c.transport, func(t *testing.T) {
			service, proto, ok := srvLabels(c.transport)
			require.Equal(t, c.ok, ok)
			require.Equal(t, c.service, service)
			require.Equal(t, c.proto, proto)
		})
	}
}
