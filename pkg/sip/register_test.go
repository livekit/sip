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

package sip

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/icholy/digest"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
)

const (
	testRegUser  = "reguser"
	testRegPass  = "regpass"
	testRegRealm = "registrar.test"
)

// testRegistrar is a minimal registrar: it records the requests it receives and answers each
// one with the response the test queued for it.
type testRegistrar struct {
	t    *testing.T
	addr string

	mu       sync.Mutex
	requests []*sip.Request
	got      chan struct{}

	// respond returns the response to send for a request. Called with the lock held.
	respond func(req *sip.Request) *sip.Response
}

func newTestRegistrar(t *testing.T, respond func(req *sip.Request) *sip.Response) *testRegistrar {
	t.Helper()
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	lis, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(localIP.AsSlice()), Port: 0})
	require.NoError(t, err)

	r := &testRegistrar{
		t:       t,
		addr:    lis.LocalAddr().String(),
		got:     make(chan struct{}, 64),
		respond: respond,
	}

	log := slog.New(logger.ToSlogHandler(logger.LogRLogger(logr.Discard())))
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("test-registrar"), sipgo.WithUserAgentLogger(log))
	require.NoError(t, err)
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(log))
	require.NoError(t, err)

	handle := func(_ *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
		r.mu.Lock()
		r.requests = append(r.requests, req)
		resp := r.respond(req)
		r.mu.Unlock()
		if resp != nil {
			_ = tx.Respond(resp)
		}
		select {
		case r.got <- struct{}{}:
		default:
		}
	}
	srv.OnRegister(handle)
	srv.OnOptions(handle)

	go func() {
		_ = srv.ServeUDP(lis)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ua.Close()
		_ = lis.Close()
	})
	return r
}

// waitRequests waits until at least n requests have been received.
func (r *testRegistrar) waitRequests(n int) []*sip.Request {
	r.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if got := r.received(); len(got) >= n {
			return got
		}
		select {
		case <-r.got:
		case <-deadline:
			r.t.Fatalf("timed out waiting for %d requests, got %d", n, len(r.received()))
		}
	}
}

func (r *testRegistrar) received() []*sip.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*sip.Request(nil), r.requests...)
}

// challenge answers a request with 401 and a digest challenge.
func challenge(req *sip.Request, qop []string) *sip.Response {
	chal := digest.Challenge{
		Realm:     testRegRealm,
		Nonce:     strconv.Itoa(rand.Int()),
		Algorithm: "MD5",
		QOP:       qop,
	}
	resp := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
	resp.AppendHeader(sip.NewHeader("WWW-Authenticate", chal.String()))
	return resp
}

// checkCredentials verifies the digest in a request against the expected password.
func checkCredentials(t *testing.T, req *sip.Request, chal *digest.Challenge, user, pass string) {
	t.Helper()
	h := req.GetHeader("Authorization")
	require.NotNil(t, h, "request has no Authorization header")
	cred, err := digest.ParseCredentials(h.Value())
	require.NoError(t, err)
	require.Equal(t, user, cred.Username)
	// The digest URI must be the Request-URI, not the address-of-record in To.
	require.Equal(t, req.Recipient.String(), cred.URI)

	want, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: user,
		Password: pass,
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
	})
	require.NoError(t, err)
	require.Equal(t, want.Response, cred.Response)
}

// newTestRegistrant builds a registrant talking to addr over a real SIP client, listening on
// its own signaling socket the way the service does. It returns the registrant and the
// signaling port, which is the port requests must be sent from.
func newTestRegistrant(t *testing.T, addr string, rc config.SIPRegistrationConfig) (*registrant, int) {
	t.Helper()
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	log := slog.New(logger.ToSlogHandler(logger.LogRLogger(logr.Discard())))
	ua, err := sipgo.NewUA(sipgo.WithUserAgent(UserAgent), sipgo.WithUserAgentLogger(log))
	require.NoError(t, err)
	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname(localIP.String()), sipgo.WithClientLogger(log))
	require.NoError(t, err)

	// Serve the signaling socket, so sipgo sends our requests from it rather than binding a
	// fresh one, as it does in the service.
	lis, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(localIP.AsSlice()), Port: 0})
	require.NoError(t, err)
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(log))
	require.NoError(t, err)
	go func() {
		_ = srv.ServeUDP(lis)
	}()
	t.Cleanup(func() {
		_ = cli.Close()
		_ = srv.Close()
		_ = ua.Close()
		_ = lis.Close()
	})
	sipPort := lis.LocalAddr().(*net.UDPAddr).Port
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, waitForSignalingSocket(ctx, ua.TransportLayer(), addr))

	if rc.Registrar == "" {
		rc.Registrar = addr
	}
	if rc.Username == "" {
		rc.Username = testRegUser
	}
	if rc.Expiry == 0 {
		rc.Expiry = time.Minute
	}
	r, err := newRegistrant(
		logger.LogRLogger(logr.Discard()), cli, nil,
		&config.Config{SIPPort: sipPort},
		&ServiceConfig{SignalingIP: localIP},
		rc,
	)
	require.NoError(t, err)
	return r, sipPort
}

func TestRegistrantRegisters(t *testing.T) {
	var (
		mu   sync.Mutex
		chal *digest.Challenge
	)
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		if req.GetHeader("Authorization") == nil {
			resp := challenge(req, nil)
			chal, _ = digest.ParseChallenge(resp.GetHeader("WWW-Authenticate").Value())
			return resp
		}
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if c := req.Contact(); c != nil {
			resp.AppendHeader(c.Clone())
		}
		if e := req.GetHeader("Expires"); e != nil {
			resp.AppendHeader(sip.NewHeader("Expires", e.Value()))
		}
		return resp
	})

	r, sipPort := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{
		Password: testRegPass,
		Expiry:   90 * time.Second,
	})
	r.Start()
	t.Cleanup(r.Stop)

	reqs := reg.waitRequests(2)
	first, second := reqs[0], reqs[1]

	require.Equal(t, sip.REGISTER, first.Method)
	require.Nil(t, first.GetHeader("Authorization"), "first REGISTER must not be pre-authenticated")

	// RFC 3581: without rport the registrar answers the address in our Via, which behind NAT
	// is not reachable.
	via := first.Via()
	require.NotNil(t, via)
	require.True(t, via.Params.Has("rport"), "Via must request rport")

	// The REGISTER has to leave from the signaling socket: behind NAT the provider delivers
	// inbound INVITEs to the mapping it created, so a mapping for any other port is useless.
	_, srcPort, err := net.SplitHostPort(first.Source())
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(sipPort), srcPort)
	require.Equal(t, sipPort, via.Port)

	// The Request-URI names the registrar and carries no user part.
	require.Equal(t, "", first.Recipient.User)
	require.Equal(t, r.registrar.Host, first.Recipient.Host)

	// The address-of-record is registered, bound to our Contact.
	to := first.To()
	require.NotNil(t, to)
	require.Equal(t, testRegUser, to.Address.User)
	contact := first.Contact()
	require.NotNil(t, contact)
	require.Equal(t, testRegUser, contact.Address.User)
	require.Equal(t, sip.ExpiresHeader(90).Value(), first.GetHeader("Expires").Value())

	mu.Lock()
	c := chal
	mu.Unlock()
	checkCredentials(t, second, c, testRegUser, testRegPass)

	// RFC 3261 §10.2: the same Call-ID with an increasing CSeq for one address-of-record.
	require.Equal(t, first.CallID().Value(), second.CallID().Value())
	require.Equal(t, first.CSeq().SeqNo+1, second.CSeq().SeqNo)

	require.Eventually(t, func() bool {
		return r.State().Registered
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, 90*time.Second, r.State().Expiry)
}

func TestRegistrantUsesAuthUsername(t *testing.T) {
	const authUser = "digest-user"
	var (
		mu   sync.Mutex
		chal *digest.Challenge
	)
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		if req.GetHeader("Authorization") == nil {
			resp := challenge(req, []string{"auth"})
			chal, _ = digest.ParseChallenge(resp.GetHeader("WWW-Authenticate").Value())
			return resp
		}
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{
		AuthUsername: authUser,
		Password:     testRegPass,
	})
	r.Start()
	t.Cleanup(r.Stop)

	reqs := reg.waitRequests(2)
	require.Equal(t, testRegUser, reqs[1].To().Address.User, "the AOR keeps the configured username")

	mu.Lock()
	c := chal
	mu.Unlock()
	// qop=auth, so the credentials also carry a client nonce and nonce count.
	checkCredentials(t, reqs[1], c, authUser, testRegPass)
	cred, err := digest.ParseCredentials(reqs[1].GetHeader("Authorization").Value())
	require.NoError(t, err)
	require.Equal(t, "auth", cred.QOP)
	require.Equal(t, 1, cred.Nc)
}

func TestRegistrantNegotiatesMinExpires(t *testing.T) {
	const minExpires = 600
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		exp, _ := strconv.Atoi(req.GetHeader("Expires").Value())
		if exp < minExpires {
			resp := sip.NewResponseFromRequest(req, sip.StatusIntervalToBrief, "Interval Too Brief", nil)
			resp.AppendHeader(sip.NewHeader("Min-Expires", strconv.Itoa(minExpires)))
			return resp
		}
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		resp.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(exp)))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Expiry: 60 * time.Second})
	r.Start()
	t.Cleanup(r.Stop)

	reqs := reg.waitRequests(2)
	require.Equal(t, "60", reqs[0].GetHeader("Expires").Value())
	require.Equal(t, "600", reqs[1].GetHeader("Expires").Value())

	require.Eventually(t, func() bool {
		return r.State().Registered
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, minExpires*time.Second, r.State().Expiry)
}

func TestRegistrantRefreshesBeforeExpiry(t *testing.T) {
	// The registrar grants far less than we ask for, so a refresh is due almost immediately.
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		resp.AppendHeader(sip.NewHeader("Expires", "1"))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Expiry: time.Hour})
	r.Start()
	t.Cleanup(r.Stop)

	reqs := reg.waitRequests(3)
	require.Equal(t, reqs[0].CallID().Value(), reqs[2].CallID().Value())
	require.Greater(t, reqs[2].CSeq().SeqNo, reqs[1].CSeq().SeqNo)
	require.Equal(t, time.Second, r.State().Expiry)
}

func TestRegistrantUnregistersOnStop(t *testing.T) {
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{})
	r.Start()
	reg.waitRequests(1)
	require.Eventually(t, func() bool {
		return r.State().Registered
	}, 5*time.Second, 10*time.Millisecond)

	r.Stop()
	require.False(t, r.State().Registered)

	reqs := reg.waitRequests(2)
	last := reqs[len(reqs)-1]
	require.Equal(t, sip.REGISTER, last.Method)
	require.Equal(t, "0", last.GetHeader("Expires").Value(), "shutdown must remove the binding")
	require.NotNil(t, last.Contact(), "the un-REGISTER must name our own binding, not all of them")
}

func TestRegistrantSendsKeepalive(t *testing.T) {
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{
		Keepalive: 20 * time.Millisecond,
	})
	r.Start()
	t.Cleanup(r.Stop)

	var options *sip.Request
	require.Eventually(t, func() bool {
		for _, req := range reg.received() {
			if req.Method == sip.OPTIONS {
				options = req
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "no OPTIONS keepalive was sent")

	require.True(t, options.Via().Params.Has("rport"), "keepalive Via must request rport too")
	require.Equal(t, r.registrar.Host, options.Recipient.Host)
	// Keepalives share one Call-ID so the registrar sees a single stream, not a new
	// transaction identity every interval.
	require.Equal(t, r.kaCallID, options.CallID().Value())
	require.NotEqual(t, r.callID, options.CallID().Value())
}

func TestRegistrantStopsOnBadCredentials(t *testing.T) {
	// Same nonce, never stale: the registrar is rejecting the password, not expiring a nonce.
	chal := digest.Challenge{Realm: testRegRealm, Nonce: "fixed-nonce", Algorithm: "MD5"}
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		resp := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
		resp.AppendHeader(sip.NewHeader("WWW-Authenticate", chal.String()))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Password: "wrong"})
	r.Start()
	t.Cleanup(r.Stop)

	// Two requests: the unauthenticated one and one answering the challenge. A third would
	// mean we are looping on a nonce we already know is not accepted.
	reg.waitRequests(2)
	require.Eventually(t, func() bool {
		return r.State().Error != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.ErrorIs(t, r.State().Error, errAuthFailed)
	require.False(t, r.State().Registered)

	time.Sleep(200 * time.Millisecond)
	require.Len(t, reg.received(), 2, "backoff must keep us from hammering the registrar")
}

// freeAddr returns an address on the local IP that nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)
	lis, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(localIP.AsSlice()), Port: 0})
	require.NoError(t, err)
	addr := lis.LocalAddr().String()
	require.NoError(t, lis.Close())
	return addr
}

func TestRegistrantStopDuringRegister(t *testing.T) {
	// Nothing answers, so a REGISTER is in flight when Stop arrives. It must abort the
	// transaction rather than wait out its 32s timeout.
	r, _ := newTestRegistrant(t, freeAddr(t), config.SIPRegistrationConfig{})
	r.Start()
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	start := time.Now()
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}
	require.Less(t, time.Since(start), 2*time.Second)
	require.False(t, r.State().Registered)
	// Nothing was ever bound, so shutdown must not try to remove one.
	require.Nil(t, r.State().Error)
}

func TestParseRegistrarURI(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "host", in: "sip.example.com", want: "sip:sip.example.com"},
		{name: "host and port", in: "sip.example.com:5070", want: "sip:sip.example.com:5070"},
		{name: "uri", in: "sip:sip.example.com", want: "sip:sip.example.com"},
		{name: "uri with transport", in: "sip:sip.example.com;transport=tcp", want: "sip:sip.example.com;transport=tcp"},
		{name: "sips", in: "sips:sip.example.com", want: "sips:sip.example.com"},
		// A REGISTER Request-URI addresses the registrar, so any user part is dropped.
		{name: "user is dropped", in: "sip:alice@sip.example.com", want: "sip:sip.example.com"},
		{name: "empty", in: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := parseRegistrarURI(c.in)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, u.String())
		})
	}
}

func TestRegistrantDefaultsAORDomainToRegistrar(t *testing.T) {
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)
	sconf := &ServiceConfig{SignalingIP: localIP}
	log := logger.LogRLogger(logr.Discard())

	r, err := newRegistrant(log, nil, nil, &config.Config{SIPPort: 5060}, sconf, config.SIPRegistrationConfig{
		Registrar: "sip:sip.example.com",
		Username:  "alice",
	})
	require.NoError(t, err)
	require.Equal(t, "sip:alice@sip.example.com", r.aor.String())
	require.Equal(t, "alice", r.authUser)
	// The Contact is this node's own signaling address, so the registrar binds calls to us.
	require.Equal(t, netip.AddrPortFrom(localIP, 5060).String(), fmt.Sprintf("%s:%d", r.contact.Host, r.contact.Port))

	r, err = newRegistrant(log, nil, nil, &config.Config{SIPPort: 5060}, sconf, config.SIPRegistrationConfig{
		Registrar: "sip:sbc.example.com",
		Domain:    "example.com",
		Username:  "alice",
	})
	require.NoError(t, err)
	require.Equal(t, "sip:alice@example.com", r.aor.String())
	require.Equal(t, "sip:sbc.example.com", r.registrar.String())
}

func TestRefreshAfter(t *testing.T) {
	cases := []struct {
		granted time.Duration
		want    time.Duration
	}{
		{granted: time.Second, want: registerMinRefreshInterval}, // interval floor
		{granted: 10 * time.Second, want: 5 * time.Second},       // margin floor
		{granted: 11 * time.Second, want: 5500 * time.Millisecond},
		{granted: 60 * time.Second, want: 50 * time.Second},
		{granted: 600 * time.Second, want: 540 * time.Second},
		{granted: time.Hour, want: time.Hour - time.Minute}, // margin ceiling
	}
	var prev time.Duration
	for _, c := range cases {
		t.Run(c.granted.String(), func(t *testing.T) {
			got := refreshAfter(c.granted)
			require.Equal(t, c.want, got)
			require.LessOrEqual(t, got, c.granted, "a refresh must not be scheduled past the expiry")
			// A longer lifetime must never mean a shorter interval.
			require.GreaterOrEqual(t, got, prev)
			prev = got
		})
	}
}

func TestGrantedExpiry(t *testing.T) {
	contact := &sip.Uri{User: "alice", Host: "10.0.0.1", Port: 5060}
	newResp := func(build func(resp *sip.Response)) *sip.Response {
		req := sip.NewRequest(sip.REGISTER, sip.Uri{Host: "sip.example.com"})
		req.AppendHeader(&sip.ViaHeader{Params: sip.NewParams()})
		req.AppendHeader(&sip.FromHeader{Params: sip.NewParams()})
		req.AppendHeader(&sip.ToHeader{Params: sip.NewParams()})
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		build(resp)
		return resp
	}
	withContact := func(u sip.Uri, expires string) *sip.Response {
		return newResp(func(resp *sip.Response) {
			h := &sip.ContactHeader{Address: u, Params: sip.NewParams()}
			if expires != "" {
				h.Params.Add("expires", expires)
			}
			resp.AppendHeader(h)
		})
	}

	t.Run("from our contact", func(t *testing.T) {
		require.Equal(t, 120*time.Second, grantedExpiry(withContact(*contact, "120"), contact, time.Hour))
	})
	t.Run("port defaults to 5060", func(t *testing.T) {
		u := *contact
		u.Port = 0
		require.Equal(t, 120*time.Second, grantedExpiry(withContact(u, "120"), contact, time.Hour))
	})
	t.Run("our binding wins over another one", func(t *testing.T) {
		other := *contact
		other.Host = "10.0.0.2"
		resp := withContact(other, "60")
		resp.AppendHeader(&sip.ContactHeader{Address: *contact, Params: sip.HeaderParams{{K: "expires", V: "120"}}})
		require.Equal(t, 120*time.Second, grantedExpiry(resp, contact, time.Hour))
	})
	t.Run("falls back to the shortest binding", func(t *testing.T) {
		// Some registrars rewrite the Contact to the address they observed, so ours is not in
		// the list. Refreshing early is safe; letting the binding lapse is not.
		rewritten := *contact
		rewritten.Host = "203.0.113.7"
		rewritten.Port = 40000
		resp := withContact(rewritten, "60")
		other := *contact
		other.Host = "10.0.0.2"
		resp.AppendHeader(&sip.ContactHeader{Address: other, Params: sip.HeaderParams{{K: "expires", V: "600"}}})
		require.Equal(t, 60*time.Second, grantedExpiry(resp, contact, time.Hour))
	})
	t.Run("from the expires header", func(t *testing.T) {
		resp := newResp(func(resp *sip.Response) {
			resp.AppendHeader(sip.NewHeader("Expires", "300"))
		})
		require.Equal(t, 300*time.Second, grantedExpiry(resp, contact, time.Hour))
	})
	t.Run("contact wins over the expires header", func(t *testing.T) {
		resp := withContact(*contact, "120")
		resp.AppendHeader(sip.NewHeader("Expires", "300"))
		require.Equal(t, 120*time.Second, grantedExpiry(resp, contact, time.Hour))
	})
	t.Run("falls back to what we asked for", func(t *testing.T) {
		require.Equal(t, time.Hour, grantedExpiry(withContact(*contact, ""), contact, time.Hour))
	})
	t.Run("ignores a malformed value", func(t *testing.T) {
		require.Equal(t, time.Hour, grantedExpiry(withContact(*contact, "soon"), contact, time.Hour))
	})
}

func TestRegistrantAnswersProxyChallenge(t *testing.T) {
	var (
		mu   sync.Mutex
		chal *digest.Challenge
	)
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		if req.GetHeader("Proxy-Authorization") == nil {
			c := digest.Challenge{Realm: testRegRealm, Nonce: strconv.Itoa(rand.Int()), Algorithm: "MD5"}
			chal = &c
			resp := sip.NewResponseFromRequest(req, sip.StatusProxyAuthRequired, "Proxy Auth Required", nil)
			resp.AppendHeader(sip.NewHeader("Proxy-Authenticate", c.String()))
			return resp
		}
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Password: testRegPass})
	r.Start()
	t.Cleanup(r.Stop)

	reqs := reg.waitRequests(2)
	require.Nil(t, reqs[1].GetHeader("Authorization"), "a 407 must be answered with Proxy-Authorization")
	h := reqs[1].GetHeader("Proxy-Authorization")
	require.NotNil(t, h)
	cred, err := digest.ParseCredentials(h.Value())
	require.NoError(t, err)
	mu.Lock()
	c := chal
	mu.Unlock()
	want, err := digest.Digest(c, digest.Options{
		Method: "REGISTER", URI: cred.URI, Username: testRegUser, Password: testRegPass,
	})
	require.NoError(t, err)
	require.Equal(t, want.Response, cred.Response)

	require.Eventually(t, func() bool { return r.State().Registered }, 5*time.Second, 10*time.Millisecond)
}

func TestRegistrantRetriesStaleNonce(t *testing.T) {
	// The refresh reuses the cached challenge and the registrar has since expired the nonce.
	// A stale challenge must be answered with the new nonce, not read as a wrong password.
	var (
		mu       sync.Mutex
		nonce    = "first-nonce"
		accepted int
	)
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		if h := req.GetHeader("Authorization"); h != nil {
			cred, err := digest.ParseCredentials(h.Value())
			if err == nil && cred.Nonce == nonce {
				accepted++
				resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				resp.AppendHeader(sip.NewHeader("Expires", "1"))
				if accepted == 1 {
					nonce = "second-nonce" // expire it right after the first success
				}
				return resp
			}
		}
		c := digest.Challenge{Realm: testRegRealm, Nonce: nonce, Algorithm: "MD5", Stale: true}
		resp := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
		resp.AppendHeader(sip.NewHeader("WWW-Authenticate", c.String()))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Password: testRegPass})
	r.Start()
	t.Cleanup(r.Stop)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return accepted >= 2
	}, 15*time.Second, 20*time.Millisecond, "the refresh never recovered from the stale nonce")
	require.NoError(t, r.State().Error)
}

func TestRegistrantKeepsMinExpiresAcrossRefreshes(t *testing.T) {
	// The registrar refuses anything under minExpires but grants only a second, so a refresh
	// follows immediately. It must ask for the negotiated value directly rather than collect
	// another 423 every time.
	const minExpires = 600
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		exp, _ := strconv.Atoi(req.GetHeader("Expires").Value())
		if exp > 0 && exp < minExpires {
			resp := sip.NewResponseFromRequest(req, sip.StatusIntervalToBrief, "Interval Too Brief", nil)
			resp.AppendHeader(sip.NewHeader("Min-Expires", strconv.Itoa(minExpires)))
			return resp
		}
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		resp.AppendHeader(sip.NewHeader("Expires", "1"))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Expiry: 60 * time.Second})
	r.Start()
	t.Cleanup(r.Stop)

	reqs := reg.waitRequests(3)
	require.Equal(t, "60", reqs[0].GetHeader("Expires").Value())
	require.Equal(t, "600", reqs[1].GetHeader("Expires").Value())
	require.Equal(t, "600", reqs[2].GetHeader("Expires").Value(), "the refresh must not be refused again")
}

func TestRegistrantKeepsPingingAfterRefreshFails(t *testing.T) {
	// The NAT mapping the provider is already sending calls to has to be held open while
	// re-registration retries, not abandoned at the first failure.
	var (
		mu       sync.Mutex
		refused  bool
		gotAfter int
	)
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case sip.OPTIONS:
			if refused {
				gotAfter++
			}
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		default:
			if !refused {
				refused = true
				// The refresh comes due at half of this, leaving the binding live for the
				// same span again while it fails.
				resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				resp.AppendHeader(sip.NewHeader("Expires", "10"))
				return resp
			}
			return sip.NewResponseFromRequest(req, sip.StatusServiceUnavailable, "Service Unavailable", nil)
		}
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{
		Keepalive: 20 * time.Millisecond,
	})
	r.Start()
	t.Cleanup(r.Stop)

	require.Eventually(t, func() bool {
		return r.State().Error != nil
	}, 8*time.Second, 10*time.Millisecond, "the refresh never failed")
	// The binding has not expired yet, so it is still reported and still held open.
	require.True(t, r.State().Registered)

	mu.Lock()
	before := gotAfter
	mu.Unlock()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotAfter > before
	}, 4*time.Second, 10*time.Millisecond, "keepalives stopped once the refresh failed")
}

func TestRegistrantAuthenticatesUnregister(t *testing.T) {
	// Some registrars challenge the un-REGISTER with a fresh nonce; leaving the binding behind
	// would send inbound calls to a node that has stopped.
	var (
		mu    sync.Mutex
		nonce = "first-nonce"
		final *sip.Request
	)
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		if h := req.GetHeader("Authorization"); h != nil {
			if cred, err := digest.ParseCredentials(h.Value()); err == nil && cred.Nonce == nonce {
				if req.GetHeader("Expires").Value() == "0" {
					final = req
				} else {
					// Expire the nonce, so the un-REGISTER's cached credentials are refused
					// and it has to answer a fresh challenge.
					nonce = "unregister-nonce"
				}
				return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			}
		}
		c := digest.Challenge{Realm: testRegRealm, Nonce: nonce, Algorithm: "MD5"}
		resp := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
		resp.AppendHeader(sip.NewHeader("WWW-Authenticate", c.String()))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Password: testRegPass})
	r.Start()
	require.Eventually(t, func() bool { return r.State().Registered }, 5*time.Second, 10*time.Millisecond)

	r.Stop()
	mu.Lock()
	last := final
	mu.Unlock()
	require.NotNil(t, last, "the un-REGISTER was never accepted")
	require.NotNil(t, last.Contact(), "the un-REGISTER must name our own binding, not all of them")
	require.Len(t, reg.received(), 4, "expected a challenge and a retry for both the REGISTER and the un-REGISTER")
}

func TestWaitForSignalingSocket(t *testing.T) {
	log := slog.New(logger.ToSlogHandler(logger.LogRLogger(logr.Discard())))
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)
	ua, err := sipgo.NewUA(sipgo.WithUserAgent(UserAgent), sipgo.WithUserAgentLogger(log))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })

	// Nothing is serving yet, so the wait has to time out rather than report a socket that
	// would send registrations from an ephemeral port.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, waitForSignalingSocket(ctx, ua.TransportLayer(), "192.0.2.1:5060"), context.DeadlineExceeded)

	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(log))
	require.NoError(t, err)
	lis, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IP(localIP.AsSlice()), Port: 0})
	require.NoError(t, err)
	go func() {
		_ = srv.ServeUDP(lis)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = lis.Close()
	})

	ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, waitForSignalingSocket(ctx, ua.TransportLayer(), "192.0.2.1:5060"))
}

func TestValidateRegistrations(t *testing.T) {
	t.Run("bad registrar", func(t *testing.T) {
		require.Error(t, validateRegistrations(&config.Config{
			SIPRegistrations: []config.SIPRegistrationConfig{{Registrar: "sip:", Username: "alice"}},
		}))
	})
	t.Run("tls registrar without tls config", func(t *testing.T) {
		require.Error(t, validateRegistrations(&config.Config{
			SIPRegistrations: []config.SIPRegistrationConfig{{Registrar: "sips:sip.example.com", Username: "alice"}},
		}))
		require.NoError(t, validateRegistrations(&config.Config{
			TLS:              &config.TLSConfig{},
			SIPRegistrations: []config.SIPRegistrationConfig{{Registrar: "sips:sip.example.com", Username: "alice"}},
		}))
	})
	t.Run("ok", func(t *testing.T) {
		require.NoError(t, validateRegistrations(&config.Config{
			SIPRegistrations: []config.SIPRegistrationConfig{{Registrar: "sip.example.com", Username: "alice"}},
		}))
	})
}

func TestRegistrantStillBound(t *testing.T) {
	r, _ := newTestRegistrant(t, freeAddr(t), config.SIPRegistrationConfig{})
	require.False(t, r.stillBound(), "nothing has been registered yet")

	r.bound, r.boundUntil = true, time.Now().Add(time.Minute)
	require.True(t, r.stillBound())

	r.boundUntil = time.Now().Add(-time.Second)
	require.False(t, r.stillBound(), "a lapsed binding is not ours to claim")
}

func TestRegistrantReportsLapsedBinding(t *testing.T) {
	// The registrar grants a short lifetime and then stops accepting refreshes. The binding
	// survives the first failure, then lapses, and must stop being reported at that point:
	// this is what the registrations_active gauge is read for.
	var mu sync.Mutex
	accepted := false
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		mu.Lock()
		defer mu.Unlock()
		if accepted {
			return sip.NewResponseFromRequest(req, sip.StatusServiceUnavailable, "Service Unavailable", nil)
		}
		accepted = true
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		resp.AppendHeader(sip.NewHeader("Expires", "3"))
		return resp
	})

	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{})
	r.Start()
	t.Cleanup(r.Stop)

	// The refresh is due at 1.5s and fails, but the binding is good until 3s.
	require.Eventually(t, func() bool {
		st := r.State()
		return st.Error != nil && st.Registered
	}, 3*time.Second, 10*time.Millisecond, "the failed refresh should not drop a live binding")

	require.Eventually(t, func() bool {
		return !r.State().Registered
	}, 10*time.Second, 20*time.Millisecond, "the lapsed binding was still reported as live")
	require.Error(t, r.State().Error)
}
