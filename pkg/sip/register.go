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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/icholy/digest"

	esip "github.com/emiago/sipgo/sip"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo/sip"
	"github.com/livekit/sipgo/transport"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	// registerMaxAttempts bounds the REGISTER transactions sent for one refresh. A refresh
	// needs one request, plus one retry per challenge and one per expiry renegotiation.
	registerMaxAttempts = 5

	registerMinBackoff = 2 * time.Second
	registerMaxBackoff = 2 * time.Minute

	// registerTimeout bounds one REGISTER exchange, including its authentication retry. A
	// non-INVITE client transaction dies at timer B after 32s anyway, and a registrar that
	// did not answer the first request is better handled by the retry loop than by waiting.
	registerTimeout = 32 * time.Second
	// unregisterTimeout bounds the un-REGISTER we send on shutdown, which delays it.
	unregisterTimeout = 5 * time.Second
	// keepaliveTimeout bounds one OPTIONS keepalive exchange. Keepalives share the goroutine
	// that refreshes the registration, so this must stay well below registerMinRefreshMargin:
	// a registrar that stops answering must not be able to delay a refresh past the expiry.
	keepaliveTimeout = 3 * time.Second

	// A refresh is sent this long before the registration expires, so a failure leaves
	// room for a retry while the current binding is still valid.
	registerMinRefreshMargin = 10 * time.Second
	registerMaxRefreshMargin = time.Minute
	// registerMinRefreshInterval keeps a peer that grants an absurdly short lifetime from
	// turning refreshes into a hot loop.
	registerMinRefreshInterval = time.Second

	// signalingSocketTimeout bounds the wait for the SIP server's UDP listener at startup.
	signalingSocketTimeout = 10 * time.Second
)

// errAuthFailed marks an authentication failure that retrying cannot fix on its own: the
// credentials have to change, here or at the provider.
var errAuthFailed = errors.New("registration credentials were not accepted")

// RegistrationState is a snapshot of one registration.
type RegistrationState struct {
	// Registered reports that the registrar has a binding for us. It stays true while a
	// refresh is failing, because the binding that refresh would replace is still live until
	// its granted lifetime runs out, and false once it does.
	Registered bool
	// Expiry is the lifetime the registrar granted for the current binding.
	Expiry time.Duration
	// Error is the failure that ended the last attempt, if it failed.
	Error error
}

// registrant keeps a single address-of-record registered with a remote registrar, so that a
// provider can deliver inbound calls without addressing this node directly (RFC 3261, §10).
//
// Registration is a prerequisite for inbound calls, not a path for them: once the provider has
// a binding, its INVITEs arrive on the normal inbound path and are authenticated and dispatched
// by trunk as usual.
type registrant struct {
	log logger.Logger
	cli SIPClient
	mon *stats.Monitor

	// registrar is the Request-URI of the REGISTER: the registrar's own address, no user part.
	registrar sip.Uri
	// aor is the address-of-record being registered, used for From and To.
	aor sip.Uri
	// contact is the address we ask the registrar to bind the AOR to.
	contact sip.Uri
	// transport is the SIP transport used to reach the registrar.
	transport string
	// viaHost is the host this node puts in Via, matching what the SIP client announces.
	viaHost string

	authUser  string
	password  string
	keepalive time.Duration

	// callID stays constant for the lifetime of the registration and cseq only increases, so
	// that the registrar can order our requests for this AOR (RFC 3261, §10.2).
	callID    string
	fromTag   string
	kaCallID  string
	kaFromTag string

	// Fields below are owned by run() and must not be touched from other goroutines.
	cseq   uint32
	kaCSeq uint32
	// expiry is the lifetime we ask for, raised in place when a registrar demands more.
	expiry time.Duration
	// challenge is the last challenge we were given. One slot is enough for the providers
	// this targets: a peer that wants both a 401 and a 407 answered on the same request is
	// not supported.
	challenge *digest.Challenge
	// authHeader is the request header that answers the cached challenge: "Authorization"
	// for a 401, "Proxy-Authorization" for a 407.
	authHeader string
	nonceCount int
	observed   string
	// bound records that the registrar has accepted a binding that has not been removed, so
	// shutdown still withdraws it after a refresh failed. A binding left pointing at a stopped
	// node sends inbound calls nowhere until it expires on its own.
	bound bool
	// granted is the lifetime of the current binding, and boundUntil when it lapses. Past
	// that point the binding is gone whatever we last heard, so we stop claiming it.
	granted    time.Duration
	boundUntil time.Time

	state   atomic.Pointer[RegistrationState]
	started atomic.Bool

	stop core.Fuse
	done chan struct{}
}

func newRegistrant(log logger.Logger, cli SIPClient, mon *stats.Monitor, conf *config.Config, sconf *ServiceConfig, rc config.SIPRegistrationConfig) (*registrant, error) {
	registrar, err := parseRegistrarURI(rc.Registrar)
	if err != nil {
		return nil, err
	}
	tr := registrarTransport(&registrar)

	domain := rc.Domain
	if domain == "" {
		domain = registrar.Host
	}
	aor := sip.Uri{Scheme: registrar.Scheme, User: rc.Username, Host: domain}

	contact := getContactURI(conf, sconf.SignalingIP, tr)
	contact.User = rc.Username

	authUser := rc.AuthUsername
	if authUser == "" {
		authUser = rc.Username
	}
	expiry := rc.Expiry
	if expiry <= 0 {
		// Config.Init sets this; a zero here would ask for a binding of no length at all.
		expiry = config.DefaultSIPRegistrationExpiry
	}

	r := &registrant{
		cli:       cli,
		mon:       mon,
		registrar: registrar,
		aor:       aor,
		contact:   *contact.GetContactURI(),
		transport: strings.ToUpper(string(tr)),
		viaHost:   sconf.SignalingIP.String(),
		authUser:  authUser,
		password:  rc.Password,
		expiry:    expiry,
		keepalive: rc.Keepalive,
		callID:    sip.GenerateTagN(32),
		fromTag:   sip.GenerateTagN(16),
		kaCallID:  sip.GenerateTagN(32),
		kaFromTag: sip.GenerateTagN(16),
		done:      make(chan struct{}),
	}
	r.log = log.WithValues(
		"registrar", r.registrar.String(),
		"aor", r.aor.String(),
		"contact", r.contact.String(),
	)
	r.setState(RegistrationState{})
	return r, nil
}

// parseRegistrarURI accepts a SIP URI or a bare "host[:port]" address.
func parseRegistrarURI(s string) (sip.Uri, error) {
	if !strings.HasPrefix(s, "sip:") && !strings.HasPrefix(s, "sips:") {
		s = "sip:" + s
	}
	var u sip.Uri
	if err := esip.ParseUri(s, &u); err != nil {
		return sip.Uri{}, fmt.Errorf("invalid registrar %q: %w", s, err)
	}
	if u.Host == "" {
		return sip.Uri{}, fmt.Errorf("invalid registrar %q: no host", s)
	}
	// The Request-URI of a REGISTER names the registrar, never a user on it.
	u.User, u.Password = "", ""
	return u, nil
}

// registrarTransport reports the SIP transport to reach a registrar with. sipgo only derives
// TLS from a sips: URI when a transport parameter already says TCP, so resolve it here and set
// it on every request instead.
func registrarTransport(u *sip.Uri) Transport {
	if u.IsEncrypted() {
		return TransportTLS
	}
	if t := transportFromURI(u); t != "" {
		return t
	}
	return TransportUDP
}

// dest is the transport address requests to the registrar are sent to.
func (r *registrant) dest() string {
	return r.registrar.Host + ":" + strconv.Itoa(uriPort(&r.registrar))
}

func (r *registrant) Start() {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	go r.run()
}

// Stop unregisters and waits for the registration to shut down. It must be called before the
// SIP client is closed, since the un-REGISTER is sent over it.
func (r *registrant) Stop() {
	r.stop.Break()
	if r.started.Load() {
		<-r.done
	}
}

func (r *registrant) State() RegistrationState {
	return *r.state.Load()
}

func (r *registrant) setState(st RegistrationState) {
	r.state.Store(&st)
	if r.mon != nil {
		r.mon.RegistrationActive(r.registrar.String(), st.Registered)
	}
}

func (r *registrant) run() {
	defer close(r.done)

	// Fires immediately for the initial registration, then rescheduled per response.
	refresh := time.NewTimer(0)
	defer refresh.Stop()

	// Fires when the current binding lapses, so a long backoff cannot leave us reporting one
	// that is already gone. Every successful refresh pushes it out again.
	lapse := time.NewTimer(0)
	<-lapse.C
	defer lapse.Stop()

	var keepalive <-chan time.Time
	if r.keepalive > 0 {
		t := time.NewTicker(r.keepalive)
		defer t.Stop()
		keepalive = t.C
	}

	backoff := registerMinBackoff
	for {
		select {
		case <-r.stop.Watch():
			r.unregister()
			return
		case <-refresh.C:
			granted, err := r.registerOnce(r.refreshTimeout(), r.expiry)
			if err == nil {
				// Record the binding before anything else can return: a REGISTER answered as
				// shutdown starts still created one, and the stop branch has to withdraw it.
				r.bound, r.granted, r.boundUntil = true, granted, time.Now().Add(granted)
			}
			if r.stop.IsBroken() {
				continue // shutting down; the stop branch unregisters
			}
			if err != nil {
				// A binding that has not expired yet is still live, so keep reporting it and
				// keep the keepalives going: they hold open the NAT mapping the provider is
				// already sending calls to. Once it lapses we stop claiming it.
				r.setState(RegistrationState{Registered: r.stillBound(), Expiry: r.granted, Error: err})
				if errors.Is(err, errAuthFailed) {
					// Nothing here is fixed by trying again soon; the account has to change.
					backoff = registerMaxBackoff
					r.log.Errorw("SIP registration was rejected", err, "retryIn", backoff)
				} else {
					r.log.Warnw("SIP registration failed, retrying", err, "retryIn", backoff)
				}
				refresh.Reset(backoff)
				if backoff *= 2; backoff > registerMaxBackoff {
					backoff = registerMaxBackoff
				}
				continue
			}
			backoff = registerMinBackoff
			after := refreshAfter(granted)
			if r.State().Registered {
				r.log.Debugw("SIP registration refreshed", "expiry", granted, "refreshIn", after)
			} else {
				r.log.Infow("SIP registration established", "expiry", granted, "refreshIn", after)
			}
			r.setState(RegistrationState{Registered: true, Expiry: granted})
			refresh.Reset(after)
			lapse.Reset(granted)
		case <-lapse.C:
			if r.stop.IsBroken() || r.stillBound() {
				continue // refreshed in the meantime
			}
			last := r.State()
			r.bound, r.granted, r.boundUntil = false, 0, time.Time{}
			r.setState(RegistrationState{Error: last.Error})
			r.log.Warnw("SIP registration lapsed; inbound calls will not arrive", last.Error)
		case <-keepalive:
			if r.stop.IsBroken() || !r.stillBound() {
				continue // shutting down, or no binding to hold open
			}
			r.sendKeepalive()
		}
	}
}

// stillBound reports whether the registrar should still have a binding for us.
func (r *registrant) stillBound() bool {
	return r.bound && time.Now().Before(r.boundUntil)
}

// refreshTimeout bounds one refresh attempt: there is little point waiting out the full
// transaction timeout past the moment the current binding lapses, when failing sooner puts us
// back in the retry loop while there is still time to replace it. The floor keeps a nearly
// lapsed binding from cutting the attempt uselessly short, since a late 200 still rebinds.
func (r *registrant) refreshTimeout() time.Duration {
	timeout := registerTimeout
	if remaining := time.Until(r.boundUntil); r.bound && remaining > 0 && remaining < timeout {
		timeout = remaining
	}
	if timeout < registerMinRefreshMargin {
		timeout = registerMinRefreshMargin
	}
	return timeout
}

// refreshAfter returns how long to wait before refreshing a registration granted for d.
func refreshAfter(d time.Duration) time.Duration {
	margin := d / 10
	if margin < registerMinRefreshMargin {
		margin = registerMinRefreshMargin
	} else if margin > registerMaxRefreshMargin {
		margin = registerMaxRefreshMargin
	}
	after := d - margin
	if d <= margin {
		after = d / 2
	}
	if after < registerMinRefreshInterval {
		after = registerMinRefreshInterval
	}
	return after
}

// registerOnce runs one REGISTER exchange, answering an auth challenge and renegotiating the
// expiry if the registrar demands a longer one, and returns the granted lifetime. An expires
// of 0 removes the binding.
func (r *registrant) registerOnce(timeout time.Duration, expires time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// The un-REGISTER on shutdown must survive the stop signal that triggered it.
	var stop <-chan struct{}
	if expires > 0 {
		stop = r.stop.Watch()
	}

	minExpiresApplied := false
	for attempt := 0; attempt < registerMaxAttempts; attempt++ {
		req := r.newRequest(sip.REGISTER, r.callID, r.fromTag, &r.cseq)
		req.AppendHeader(&sip.ContactHeader{Address: cloneURI(r.contact)})
		exp := sip.ExpiresHeader(expires / time.Second)
		req.AppendHeader(&exp)
		if r.challenge != nil {
			if err := r.authorize(req); err != nil {
				return 0, err
			}
		}

		resp, err := r.roundtrip(ctx, stop, req)
		if err != nil {
			return 0, err
		}
		switch resp.StatusCode {
		case sip.StatusOK:
			r.logObservedAddress(resp)
			granted := grantedExpiry(resp, &r.contact, expires)
			if expires > 0 && granted <= 0 {
				// A 200 that grants nothing means the binding was not created after all.
				return 0, errors.New("registrar accepted the REGISTER but granted no binding")
			}
			return granted, nil
		case sip.StatusUnauthorized, sip.StatusProxyAuthRequired:
			if err := r.setChallenge(resp); err != nil {
				return 0, err
			}
		case sip.StatusForbidden:
			// Registrars commonly answer bad credentials with 403 rather than another 401.
			return 0, fmt.Errorf("%w: %w", errAuthFailed, sipStatusError(resp))
		case sip.StatusIntervalToBrief:
			// The registrar demands a longer lifetime than we asked for.
			if expires == 0 || minExpiresApplied {
				return 0, sipStatusError(resp)
			}
			minExpires, ok := headerSeconds(resp, "Min-Expires")
			if !ok || minExpires <= expires || minExpires > config.MaxSIPRegistrationExpiry {
				return 0, fmt.Errorf("registrar demands an unusable expiry: %w", sipStatusError(resp))
			}
			r.log.Infow("registrar requires a longer expiry", "requested", expires, "minExpires", minExpires)
			expires, minExpiresApplied = minExpires, true
			// Ask for it directly from now on, instead of being refused once per refresh.
			r.expiry = minExpires
		default:
			return 0, sipStatusError(resp)
		}
	}
	return 0, fmt.Errorf("REGISTER did not complete in %d attempts", registerMaxAttempts)
}

func (r *registrant) unregister() {
	if !r.stillBound() {
		return // nothing bound, or it has lapsed on its own
	}
	r.bound, r.granted, r.boundUntil = false, 0, time.Time{}
	if _, err := r.registerOnce(unregisterTimeout, 0); err != nil {
		r.log.Warnw("could not unregister", err)
	} else {
		r.log.Infow("SIP registration removed")
	}
	r.setState(RegistrationState{})
}

// sendKeepalive sends an OPTIONS request to the registrar. A registration lifetime long enough
// for a provider to accept (often 10 minutes or more) far outlives a NAT UDP mapping, which is
// commonly dropped after 30s of silence. Without traffic on the signaling socket the binding
// the provider recorded stops being reachable, and inbound calls quietly stop arriving while
// the registration still looks healthy. Any response, including an error, keeps the mapping
// open and proves the path is still there.
func (r *registrant) sendKeepalive() {
	ctx, cancel := context.WithTimeout(context.Background(), keepaliveTimeout)
	defer cancel()

	req := r.newRequest(sip.OPTIONS, r.kaCallID, r.kaFromTag, &r.kaCSeq)
	resp, err := r.roundtrip(ctx, r.stop.Watch(), req)
	if err != nil {
		if !r.stop.IsBroken() {
			r.log.Warnw("no response to SIP keepalive; inbound calls may not reach this node", err)
		}
		return
	}
	r.log.Debugw("SIP keepalive answered", "status", resp.StatusCode)
	r.logObservedAddress(resp)
}

// newRequest builds an out-of-dialog request towards the registrar.
func (r *registrant) newRequest(method sip.RequestMethod, callID, fromTag string, cseq *uint32) *sip.Request {
	req := sip.NewRequest(method, r.registrar)
	req.SetTransport(r.transport)

	// sipgo only adds a Via of its own when the request has none, and the one it adds has no
	// rport. Behind NAT our Via carries an address the registrar cannot reply to, so it must
	// ask the registrar to answer the source address and port it actually observed
	// (RFC 3581); without it the response never arrives and registration never completes.
	via := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       r.transport,
		Host:            r.viaHost,
		Params:          sip.NewParams(),
	}
	via.Params.Add("branch", sip.GenerateBranchN(16))
	via.Params.Add("rport", "")
	req.AppendHeader(via)

	from := &sip.FromHeader{Address: cloneURI(r.aor), Params: sip.NewParams()}
	from.Params.Add("tag", fromTag)
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: cloneURI(r.aor), Params: sip.NewParams()})

	cid := sip.CallIDHeader(callID)
	req.AppendHeader(&cid)

	*cseq++
	req.AppendHeader(&sip.CSeqHeader{MethodName: method, SeqNo: *cseq})

	req.AppendHeader(sip.NewHeader("User-Agent", UserAgent))
	return req
}

func (r *registrant) roundtrip(ctx context.Context, stop <-chan struct{}, req *sip.Request) (*sip.Response, error) {
	tx, err := r.cli.TransactionRequest(req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	return sipResponse(ctx, tx, stop, nil)
}

// setChallenge stores the challenge from a 401 or 407 so the next request can answer it.
func (r *registrant) setChallenge(resp *sip.Response) error {
	name, authHeader := "WWW-Authenticate", "Authorization"
	if resp.StatusCode == sip.StatusProxyAuthRequired {
		name, authHeader = "Proxy-Authenticate", "Proxy-Authorization"
	}
	h := resp.GetHeader(name)
	if h == nil {
		return fmt.Errorf("%w: %s", ErrAuthNoHeader, name)
	}
	chal, err := digest.ParseChallenge(h.Value())
	if err != nil {
		return fmt.Errorf("invalid %s challenge %q: %w", name, h.Value(), err)
	}
	// A repeat of a nonce we already answered, without stale=true, means the credentials were
	// wrong rather than expired. Retrying would send the same digest and loop.
	if r.challenge != nil && r.challenge.Nonce == chal.Nonce && !chal.Stale {
		r.challenge = nil
		return fmt.Errorf("%w: the registrar repeated its challenge", errAuthFailed)
	}
	if r.password == "" {
		return fmt.Errorf("%w: %w", errAuthFailed, ErrAuthMissingCreds)
	}
	// RFC 2617: the nonce count must keep increasing for a given nonce, so only a new one
	// restarts the count. A stale challenge may repeat the nonce.
	if r.challenge == nil || r.challenge.Nonce != chal.Nonce {
		r.nonceCount = 0
	}
	r.challenge, r.authHeader = chal, authHeader
	return nil
}

func (r *registrant) authorize(req *sip.Request) error {
	r.nonceCount++
	cred, err := digest.Digest(r.challenge, digest.Options{
		Method: req.Method.String(),
		// The digest URI is the Request-URI (RFC 3261, §22.4). For REGISTER that is the
		// registrar, which is not the same as the address-of-record in To.
		URI:      req.Recipient.String(),
		Username: r.authUser,
		Password: r.password,
		Count:    r.nonceCount,
	})
	if err != nil {
		// Typically an algorithm icholy/digest does not implement, which no retry resolves.
		return fmt.Errorf("%w: %w", errAuthFailed, err)
	}
	req.AppendHeader(sip.NewHeader(r.authHeader, cred.String()))
	return nil
}

// logObservedAddress reports the source address the registrar saw, which RFC 3581 asks it to
// echo in the Via. Behind NAT it is the public mapping, and it will not match our Contact.
// Registrars differ in which of the two they route inbound calls to, so surfacing the
// difference separates "registered, but the provider is calling an address we do not have"
// from "not registered".
func (r *registrant) logObservedAddress(resp *sip.Response) {
	via := resp.Via()
	if via == nil {
		return
	}
	host, _ := via.Params.Get("received")
	port, _ := via.Params.Get("rport")
	if host == "" && port == "" {
		return
	}
	observed := host
	if port != "" {
		observed += ":" + port
	}
	if observed == r.observed {
		return
	}
	r.observed = observed
	// For TLS the Contact carries sip_hostname, which never equals an observed IP, so there is
	// nothing to compare and no advice worth giving.
	if host != "" && host != r.contact.Host && r.transport != strings.ToUpper(string(TransportTLS)) {
		r.log.Infow("registrar sees this node at a different address than the Contact we sent; "+
			"inbound calls only arrive if it routes to the observed address, otherwise set nat_1_to_1_ip",
			"observed", observed)
		return
	}
	r.log.Debugw("registrar confirmed our address", "observed", observed)
}

// grantedExpiry reports the lifetime the registrar granted, preferring the most specific
// source: the expires parameter on our own binding, then the Expires header, then the shortest
// binding it listed, then the value we asked for. The third step covers registrars that rewrite
// the Contact to the address they observed, which stops it matching ours. Underestimating the
// lifetime only costs an early refresh, while overestimating lets the binding lapse.
func grantedExpiry(resp *sip.Response, contact *sip.Uri, requested time.Duration) time.Duration {
	var shortest time.Duration
	for _, h := range resp.GetHeaders("Contact") {
		c, ok := h.(*sip.ContactHeader)
		if !ok {
			continue
		}
		v, ok := c.Params.Get("expires")
		if !ok {
			continue
		}
		sec, err := strconv.Atoi(v)
		if err != nil || sec < 0 {
			continue
		}
		d := time.Duration(sec) * time.Second
		if sameContact(&c.Address, contact) {
			return d
		}
		if d > 0 && (shortest == 0 || d < shortest) {
			shortest = d
		}
	}
	if d, ok := headerSeconds(resp, "Expires"); ok {
		return d
	}
	if shortest != 0 {
		return shortest
	}
	return requested
}

func sameContact(a, b *sip.Uri) bool {
	return a.User == b.User &&
		strings.EqualFold(a.Host, b.Host) &&
		uriPort(a) == uriPort(b)
}

func uriPort(u *sip.Uri) int {
	if u.Port != 0 {
		return u.Port
	}
	if u.IsEncrypted() {
		return 5061
	}
	return 5060
}

func headerSeconds(resp *sip.Response, name string) (time.Duration, bool) {
	h := resp.GetHeader(name)
	if h == nil {
		return 0, false
	}
	sec, err := strconv.Atoi(strings.TrimSpace(h.Value()))
	if err != nil || sec < 0 {
		return 0, false
	}
	return time.Duration(sec) * time.Second, true
}

func sipStatusError(resp *sip.Response) error {
	return &livekit.SIPStatus{
		Code:   livekit.SIPStatusCode(resp.StatusCode),
		Status: resp.Reason,
	}
}

func cloneURI(u sip.Uri) sip.Uri {
	u.UriParams = u.UriParams.Clone()
	u.Headers = u.Headers.Clone()
	return u
}

// waitForSignalingSocket blocks until the SIP server's UDP listener is registered with the
// transport layer. sipgo reuses that socket for outbound UDP requests, so waiting makes
// REGISTER leave from the signaling port: providers deliver inbound INVITEs to the NAT mapping
// the REGISTER created, and a mapping for some other port routes them nowhere. Sending before
// the listener exists binds a separate socket, which is then cached for the registrar address
// and used for every later request.
func waitForSignalingSocket(ctx context.Context, tp *transport.Layer, dest string) error {
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := tp.GetConnection("udp", dest); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}
