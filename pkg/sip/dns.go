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
	"net/netip"
	"strconv"
	"strings"

	"github.com/livekit/sipgo/sip"
)

// DNSResolver is the subset of *net.Resolver used to locate a SIP next hop.
// It is an interface so that tests can stub DNS out.
type DNSResolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

var _ DNSResolver = (*net.Resolver)(nil)

// srvLabels returns the RFC 3263 service and protocol labels for a SIP
// transport, i.e. the "sip" and "udp" of an _sip._udp.example.com SRV lookup.
// ok is false for transports that have no SRV mapping.
func srvLabels(transport string) (service, proto string, ok bool) {
	switch strings.ToLower(transport) {
	case "udp":
		return "sip", "udp", true
	case "tcp":
		return "sip", "tcp", true
	case "tls":
		return "sips", "tcp", true
	case "ws": // RFC 7118
		return "sip", "ws", true
	case "wss": // RFC 7118
		return "sips", "wss", true
	}
	return "", "", false
}

// resolveNextHop returns the "ip:port" transport destination for a SIP next
// hop, following the DNS procedures of RFC 3263 section 4.
//
// SRV records are only consulted when the URI carries no explicit port
// (RFC 3263 section 4.2): a numeric port, like an IP literal host, means the
// hop has already been selected and DNS must not override it. When the target
// publishes no usable SRV record, resolution falls back to a plain address
// lookup of host at the transport's default port, which is what the transport
// layer would have done on its own.
//
// NAPTR (RFC 3263 section 4.1) is deliberately not implemented: the transport
// is always already known here, either from the trunk configuration or from
// the URI's transport parameter, so there is nothing left for NAPTR to select.
func resolveNextHop(ctx context.Context, r DNSResolver, host string, port int, transport string) (string, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if port == 0 {
			port = sip.DefaultPort(transport)
		}
		return netip.AddrPortFrom(ip, uint16(port)).String(), nil
	}
	if port != 0 {
		return resolveHost(ctx, r, host, port)
	}
	if service, proto, ok := srvLabels(transport); ok {
		if _, srvs, err := r.LookupSRV(ctx, service, proto, host); err == nil {
			// LookupSRV already orders records by priority and shuffles them by
			// weight, so the first target that resolves is the one to use.
			for _, srv := range srvs {
				target := strings.TrimSuffix(srv.Target, ".")
				if target == "" {
					// A lone "." target means "no service here" (RFC 2782). Treat
					// it as an unusable record and fall back to an address lookup,
					// rather than failing the call outright.
					break
				}
				dest, err := resolveHost(ctx, r, target, int(srv.Port))
				if err != nil {
					continue // try the next target
				}
				return dest, nil
			}
		}
	}
	return resolveHost(ctx, r, host, sip.DefaultPort(transport))
}

// resolveHost resolves host to a single "ip:port" destination. It picks the
// first address returned, which is the one the resolver considers preferable
// under RFC 6724, matching what net.ResolveIPAddr would have selected.
func resolveHost(ctx context.Context, r DNSResolver, host string, port int) (string, error) {
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return net.JoinHostPort(addrs[0].IP.String(), strconv.Itoa(port)), nil
}
