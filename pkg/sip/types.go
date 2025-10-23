// Copyright 2024 LiveKit, Inc.
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
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo/sip"
)

type Headers []sip.Header

func (h Headers) GetHeader(name string) sip.Header {
	name = sip.HeaderToLower(name)
	for _, kv := range h {
		if sip.HeaderToLower(kv.Name()) == name {
			return kv
		}
	}
	return nil
}

func TransportFrom(t livekit.SIPTransport) Transport {
	switch t {
	case livekit.SIPTransport_SIP_TRANSPORT_UDP:
		return TransportUDP
	case livekit.SIPTransport_SIP_TRANSPORT_TCP:
		return TransportTCP
	case livekit.SIPTransport_SIP_TRANSPORT_TLS:
		return TransportTLS
	}
	return ""
}

func SIPTransportFrom(t Transport) livekit.SIPTransport {
	switch t {
	case TransportUDP:
		return livekit.SIPTransport_SIP_TRANSPORT_UDP
	case TransportTCP:
		return livekit.SIPTransport_SIP_TRANSPORT_TCP
	case TransportTLS:
		return livekit.SIPTransport_SIP_TRANSPORT_TLS
	}

	return livekit.SIPTransport_SIP_TRANSPORT_AUTO
}

type Transport string

const (
	TransportUDP = Transport("udp")
	TransportTCP = Transport("tcp")
	TransportTLS = Transport("tls")
)

type URI struct {
	User      string
	Host      string
	Addr      netip.AddrPort
	Transport Transport
}

func ConvertURI(u *sip.Uri) URI {
	uri := URI{
		User:      u.User,
		Host:      u.Host,
		Addr:      netip.AddrPortFrom(netip.Addr{}, uint16(u.Port)),
		Transport: TransportUDP,
	}
	if tr := transportFromURI(u); tr != "" {
		uri.Transport = tr
	}
	uri = uri.Normalize()
	return uri
}

func CreateURIFromUserAndAddress(user string, address string, transport Transport) URI {
	uri := URI{
		User:      user,
		Host:      address,
		Transport: transport,
	}

	uri = uri.Normalize()

	return uri
}

func (u URI) Normalize() URI {
	if addr, sport, err := net.SplitHostPort(u.Host); err == nil {
		if port, err := strconv.Atoi(sport); err == nil {
			u.Host = addr
			u.Addr = netip.AddrPortFrom(u.Addr.Addr(), uint16(port))
		}
	}
	return u
}

func (u URI) GetHost() string {
	host := u.Host
	if host == "" {
		host = u.Addr.Addr().String()
	}
	return host
}

func (u URI) GetPort() int {
	port := int(u.Addr.Port())
	if port == 0 {
		if u.Transport == TransportTLS {
			port = 5061
		} else {
			port = 5060
		}
	}
	return port
}

func (u URI) GetPortOrNone() int {
	port := u.GetPort()
	if port == 5060 {
		port = 0
	}
	return port
}

func (u URI) GetHostPort() string {
	return u.GetHost() + ":" + strconv.Itoa(u.GetPort())
}

func (u URI) GetDest() string {
	host := u.Host
	if u.Addr.Addr().IsValid() {
		host = u.Addr.Addr().String()
	}
	host += ":" + strconv.Itoa(u.GetPort())
	return host
}

func (u URI) GetURI() *sip.Uri {
	su := &sip.Uri{
		User: u.User,
		Host: u.GetHost(),
	}
	if port := u.Addr.Port(); port != 0 {
		su.Port = int(port)
	}
	if u.Transport != "" {
		if su.UriParams == nil {
			su.UriParams = make(sip.HeaderParams)
		}
		su.UriParams.Add("transport", string(u.Transport))
	}
	return su
}

func (u URI) GetContactURI() *sip.Uri {
	su := u.GetURI()
	switch u.Transport {
	case TransportUDP, TransportTCP:
		// Use IP instead of a hostname for TCP and UDP.
		if addr := u.Addr.Addr(); addr.IsValid() {
			su.Host = addr.String()
		}
	}
	return su
}

func (u URI) ToSIPUri() *livekit.SIPUri {
	url := &livekit.SIPUri{
		User:      u.User,
		Host:      u.GetHost(),
		Port:      uint32(u.GetPort()),
		Transport: SIPTransportFrom(u.Transport),
	}

	if u.Addr.Addr().IsValid() {
		url.Ip = u.Addr.Addr().String()
	}
	return url
}

type LocalTag string
type RemoteTag string

func getFromTag(r *sip.Request) (RemoteTag, error) {
	from := r.From()
	if from == nil {
		return "", errors.New("no From on Request")
	}
	tag, ok := getTagFrom(from.Params)
	if !ok {
		return "", errors.New("no tag in From on Request")
	}
	return tag, nil
}

func getToTag(r *sip.Response) (RemoteTag, error) {
	to := r.To()
	if to == nil {
		return "", errors.New("no To on Response")
	}
	tag, ok := getTagFrom(to.Params)
	if !ok {
		return "", errors.New("no tag in To on Response")
	}
	return tag, nil
}

func getTagFrom(params sip.HeaderParams) (RemoteTag, bool) {
	tag, ok := params["tag"]
	if !ok {
		return "", false
	}
	return RemoteTag(tag), true
}

func LoggerWithParams(log logger.Logger, c Signaling) logger.Logger {
	if a := c.From(); a.Host != "" {
		log = log.WithValues("fromHost", a.Host, "fromUser", a.User)
	}
	if a := c.To(); a.Host != "" {
		log = log.WithValues("toHost", a.Host, "toUser", a.User)
	}
	if tag := c.Tag(); tag != "" {
		log = log.WithValues("sipTag", tag)
	}
	if cid := c.SIPCallID(); cid != "" {
		log = log.WithValues("sipCallID", cid)
	}
	return log
}

func LoggerWithHeaders(log logger.Logger, c Signaling) logger.Logger {
	headers := c.RemoteHeaders()
	for hdr, name := range headerToLog {
		if h := headers.GetHeader(hdr); h != nil {
			log = log.WithValues(name, h.Value())
		}
	}
	return log
}

func HeadersToAttrs(attrs, hdrToAttr map[string]string, opts livekit.SIPHeaderOptions, c Signaling, headers Headers) map[string]string {
	if attrs == nil {
		attrs = make(map[string]string)
	}
	if c != nil {
		headers = c.RemoteHeaders()
	}
	// Map all headers, if requested
	if opts != livekit.SIPHeaderOptions_SIP_NO_HEADERS {
		for _, h := range headers {
			if h == nil {
				continue
			}
			name := strings.ToLower(h.Name())
			if name == "" {
				continue
			}
			switch opts {
			case livekit.SIPHeaderOptions_SIP_X_HEADERS:
				if !strings.HasPrefix(name, "x-") {
					continue
				}
			}
			attrs[livekit.AttrSIPHeaderPrefix+name] = h.Value()
		}
	}
	// Global header mapping
	for hdr, name := range headerToAttr {
		if h := headers.GetHeader(hdr); h != nil {
			attrs[name] = h.Value()
		}
	}
	// Request mapping
	for hdr, name := range hdrToAttr {
		if h := headers.GetHeader(hdr); h != nil {
			attrs[name] = h.Value()
		}
	}
	if c != nil {
		// Other metadata
		if tag := c.Tag(); tag != "" {
			attrs[AttrSIPCallTag] = string(tag)
		}
		if cid := c.SIPCallID(); cid != "" {
			attrs[AttrSIPCallIDFull] = cid
		}
	}
	return attrs
}

func AttrsToHeaders(attrs, attrToHdr, headers map[string]string) map[string]string {
	if len(attrToHdr) == 0 {
		return headers
	}
	if headers == nil {
		headers = make(map[string]string)
	}
	for attr, hdr := range attrToHdr {
		val, ok := attrs[attr]
		if !ok {
			continue
		}
		headers[hdr] = val
	}
	return headers
}

func sdpEncryption(e livekit.SIPMediaEncryption) (sdp.Encryption, error) {
	switch e {
	case livekit.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_DISABLE:
		return sdp.EncryptionNone, nil
	case livekit.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_ALLOW:
		return sdp.EncryptionAllow, nil
	case livekit.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_REQUIRE:
		return sdp.EncryptionRequire, nil
	}
	return sdp.EncryptionAllow, errors.New("invalid SIP media encryption type")
}

func SelectValue[T any](then, els T, probElse float64) T {
	if probElse <= 0 {
		return then
	}
	if rand.Float64() < probElse {
		return then
	}
	return els
}

func SelectValueBool(then bool, probElse float64) bool {
	return SelectValue(then, !then, probElse)
}
