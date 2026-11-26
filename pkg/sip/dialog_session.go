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
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	esip "github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo/sip"
)

var (
	// ErrDialogEnded is returned for a request on a dialog that a BYE, or a
	// response that says the dialog is gone, has ended.
	ErrDialogEnded = errors.New("dialog ended")
	// ErrRequestPending is returned for an INVITE while another INVITE of
	// either side is in progress on the dialog (RFC 3261 section 14.1).
	ErrRequestPending = errors.New("an INVITE is in progress on the dialog")
)

// dialogInviteAttempts is how many times one Invite sends the request: the
// first attempt, and the retries after a 491 or a digest challenge.
const dialogInviteAttempts = 4

// glareRetryDelay is how long to wait before the retry of an INVITE that got
// a 491 (RFC 3261 section 14.1): 2.1 to 4 seconds for the owner of the
// Call-ID, 0 to 2 seconds for the other side, in units of 10 ms.
var glareRetryDelay = func(ownsCallID bool) time.Duration {
	if ownsCallID {
		return 2100*time.Millisecond + time.Duration(rand.IntN(191))*10*time.Millisecond
	}
	return time.Duration(rand.IntN(201)) * 10 * time.Millisecond
}

// DialogSessionOptions configures a DialogSession. OnOffer is required; the
// other fields are optional.
type DialogSessionOptions struct {
	Log logger.Logger
	// Username and Password answer a digest challenge (401 or 407) to a
	// request of this side.
	Username string
	Password string
	// Transport sends this side's requests over this transport. By default
	// the URI of the next hop selects it.
	Transport Transport
	// OnOffer applies the offer of a re-INVITE from the remote side to the
	// owner's media and returns the answer. An SDPError rejects the re-INVITE
	// with a 488, another error with a 500.
	OnOffer func(offer []byte) (answer []byte, err error)
	// Handler receives a snapshot of the dialog after each change: a
	// re-INVITE of either side that was accepted. It is called without a
	// lock held.
	Handler DialogHandler
}

// DialogSession is this side of an established SIP dialog, built from a
// Dialog snapshot. It owns the dialog's sequence numbers and sends requests
// within the dialog (RFC 3261 section 12.2.1).
//
// The session does not listen for requests. The owner of the SIP handlers
// finds it with Matches and passes the requests to the Handle methods.
type DialogSession struct {
	log  logger.Logger
	cli  *Client
	opts DialogSessionOptions

	local  sip.Uri
	remote sip.Uri

	mu     sync.Mutex
	d      Dialog
	target sip.Uri
	routes []sip.Uri
	// inviting is set while an INVITE of this side is in progress, answering
	// while this side answers an INVITE of the remote side.
	inviting  bool
	answering bool
	remoteBye *ReasonHeader
	ended     core.Fuse
}

// NewDialogSession creates the session for the dialog in the snapshot. The
// client sends the session's requests; it must be started before the first
// request.
func NewDialogSession(cli *Client, d *Dialog, opts DialogSessionOptions) (*DialogSession, error) {
	if cli == nil {
		return nil, errors.New("no SIP client")
	}
	if d == nil {
		return nil, errors.New("no dialog")
	}
	if d.State != DialogConfirmed {
		return nil, fmt.Errorf("dialog is %q, not confirmed", d.State)
	}
	if d.CallID == "" || d.LocalTag == "" || d.RemoteTag == "" {
		return nil, errors.New("dialog has no Call-ID or tags")
	}
	if opts.OnOffer == nil {
		return nil, errors.New("no OnOffer in the options")
	}
	s := &DialogSession{
		log:  opts.Log,
		cli:  cli,
		opts: opts,
		d:    *cloneDialog(d),
	}
	if s.log == nil {
		s.log = cli.log
	}
	s.log = s.log.WithValues("sipCallID", d.CallID, "callID", d.LocalTag)
	var err error
	if s.local, err = parseDialogURI(d.LocalURI); err != nil {
		return nil, fmt.Errorf("local URI: %w", err)
	}
	if s.remote, err = parseDialogURI(d.RemoteURI); err != nil {
		return nil, fmt.Errorf("remote URI: %w", err)
	}
	if s.target, err = parseDialogURI(d.RemoteTarget); err != nil {
		return nil, fmt.Errorf("remote target: %w", err)
	}
	for _, r := range d.RouteSet {
		u, err := parseDialogURI(r)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", r, err)
		}
		s.routes = append(s.routes, u)
	}
	return s, nil
}

func parseDialogURI(raw string) (sip.Uri, error) {
	var u sip.Uri
	if raw == "" {
		return u, errors.New("empty URI")
	}
	if err := esip.ParseUri(raw, &u); err != nil {
		return u, err
	}
	if u.Host == "" {
		return u, errors.New("URI has no host")
	}
	return u, nil
}

func cloneDialog(d *Dialog) *Dialog {
	c := *d
	c.RouteSet = slices.Clone(d.RouteSet)
	c.LocalSDP = slices.Clone(d.LocalSDP)
	c.RemoteSDP = slices.Clone(d.RemoteSDP)
	return &c
}

// Dialog returns a snapshot of the dialog as it is now.
func (s *DialogSession) Dialog() *Dialog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneDialog(&s.d)
}

// Ended is closed when the dialog ended: a BYE of either side, or a response
// that says the remote side no longer has the dialog.
func (s *DialogSession) Ended() <-chan struct{} {
	return s.ended.Watch()
}

// RemoteBye returns the reason of the remote side's BYE. It reports false
// when the remote side sent none.
func (s *DialogSession) RemoteBye() (ReasonHeader, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteBye == nil {
		return ReasonHeader{}, false
	}
	return *s.remoteBye, true
}

// Matches reports whether the request of the remote side belongs to this
// dialog: the same Call-ID, this side's tag in To and the remote tag in From.
func (s *DialogSession) Matches(req *sip.Request) bool {
	callID, from, to := req.CallID(), req.From(), req.To()
	if callID == nil || from == nil || to == nil || from.Params == nil || to.Params == nil {
		return false
	}
	toTag, _ := to.Params.Get("tag")
	fromTag, _ := from.Params.Get("tag")
	return callID.Value() == s.d.CallID && toTag == s.d.LocalTag && fromTag == s.d.RemoteTag
}

// changedLocked gives the dialog a new version and returns the snapshot for
// the handler. The version of the first snapshot can be above this process's
// counter, so the new version is also above the dialog's last one.
func (s *DialogSession) changedLocked() *Dialog {
	s.d.Version = max(s.d.Version+1, dialogVersion.Add(1))
	return cloneDialog(&s.d)
}

func (s *DialogSession) emit(d *Dialog) {
	if s.opts.Handler != nil && d != nil {
		s.opts.Handler.HandleDialog(d)
	}
}

// newRequestLocked builds a request within the dialog and takes the next
// sequence number for it (RFC 3261 section 12.2.1.1).
func (s *DialogSession) newRequestLocked(method sip.RequestMethod) *sip.Request {
	cseq := s.d.NextLocalCSeq
	s.d.NextLocalCSeq++
	return s.buildRequestLocked(method, cseq)
}

func (s *DialogSession) buildRequestLocked(method sip.RequestMethod, cseq uint32) *sip.Request {
	target, routes := s.target, s.routes
	strict := len(routes) > 0 && !routes[0].UriParams.Has("lr")
	if strict {
		// The first route is a strict router: it becomes the Request-URI,
		// and the remote target goes to the end of the route set.
		target = routes[0]
		routes = append(slices.Clone(routes[1:]), s.target)
	}
	req := sip.NewRequest(method, *target.Clone())
	if strict {
		// The transport layer takes the next hop from the first Route
		// header; with a strict router the next hop is the Request-URI.
		req.SetDestination(ConvertURI(&target).GetDest())
		if tr := transportFromURI(&target); tr != "" {
			req.SetTransport(strings.ToUpper(string(tr)))
		}
	}
	for _, r := range routes {
		req.AppendHeader(&sip.RouteHeader{Address: *r.Clone()})
	}
	from := &sip.FromHeader{Address: *s.local.Clone(), Params: sip.NewParams()}
	from.Params.Add("tag", s.d.LocalTag)
	to := &sip.ToHeader{Address: *s.remote.Clone(), Params: sip.NewParams()}
	to.Params.Add("tag", s.d.RemoteTag)
	callID := sip.CallIDHeader(s.d.CallID)
	maxForwards := sip.MaxForwardsHeader(70)
	req.AppendHeader(from)
	req.AppendHeader(to)
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	req.AppendHeader(&maxForwards)
	req.AppendHeader(sip.NewHeader("User-Agent", UserAgent))
	if s.opts.Transport != "" {
		req.SetTransport(strings.ToUpper(string(s.opts.Transport)))
	}
	return req
}

// contact is this side's Contact for a message sent over the transport.
func (s *DialogSession) contact(tr string) *sip.ContactHeader {
	uri := s.cli.ContactURI(Transport(strings.ToLower(tr)))
	return &sip.ContactHeader{Address: *uri.GetContactURI()}
}

// transaction starts the client transaction of a request of this side.
func (s *DialogSession) transaction(req *sip.Request) (sip.ClientTransaction, error) {
	return s.cli.sipCli.TransactionRequest(req)
}

// sendInvite sends the INVITE and waits for its final response. When ctx
// ends, it sends a CANCEL, and still returns a 2xx that the remote side sent
// before it saw the CANCEL: that 2xx needs its ACK (RFC 3261 section 9.1).
func (s *DialogSession) sendInvite(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	tx, err := s.transaction(req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	res, cancelled, err := sipResponse(ctx, tx, nil, nil)
	if cancelled != "" {
		if accepted := cancelledInviteAnswer(tx); accepted != nil {
			s.log.Infow("cancelled re-INVITE was accepted", "cseq", requestCSeq(req))
			return accepted, nil
		}
	}
	return res, err
}

// Invite sends a re-INVITE with the offer and returns the answer. It sends
// the ACK for a 2xx, and tries again after a 491 or a digest challenge.
//
// A 481 or a 408 ends the session: the error wraps ErrDialogEnded.
//
// When ctx ends, Invite cancels the request. If the remote side accepted the
// INVITE before it saw the CANCEL, Invite returns the answer and no error.
func (s *DialogSession) Invite(ctx context.Context, offer []byte) ([]byte, error) {
	s.mu.Lock()
	if s.ended.IsBroken() {
		s.mu.Unlock()
		return nil, ErrDialogEnded
	}
	if s.inviting || s.answering {
		s.mu.Unlock()
		return nil, ErrRequestPending
	}
	s.inviting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inviting = false
		s.mu.Unlock()
	}()

	var auth sip.Header
	for attempt := 1; ; attempt++ {
		s.mu.Lock()
		req := s.newRequestLocked(sip.INVITE)
		s.mu.Unlock()
		req.AppendHeader(s.contact(req.Transport()))
		req.AppendHeader(sip.NewHeader("Content-Type", contentTypeSDP))
		req.AppendHeader(sip.HeaderClone(allowHeader))
		if auth != nil {
			req.AppendHeader(sip.HeaderClone(auth))
		}
		req.SetBody(offer)

		s.log.Infow("sending re-INVITE", "cseq", requestCSeq(req), "attempt", attempt)
		res, err := s.sendInvite(ctx, req)
		if err != nil {
			if ctx.Err() == nil {
				// No final response: the same as a 408 (RFC 3261 section 12.2.1.2).
				s.ended.Break()
				return nil, fmt.Errorf("%w: re-INVITE got no response: %v", ErrDialogEnded, err)
			}
			return nil, fmt.Errorf("re-INVITE: %w", err)
		}
		last := attempt >= dialogInviteAttempts
		switch {
		case res.StatusCode >= 200 && res.StatusCode < 300:
			// Any 2xx accepts the request (RFC 3261 section 8.1.3.2).
			return s.inviteAccepted(req, res, offer, auth)
		case res.StatusCode == statusRequestPending && !last:
			delay := glareRetryDelay(s.d.Direction == livekit.SIPCallDirection_SCD_OUTBOUND)
			s.log.Infow("re-INVITE got 491, retrying", "delay", delay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-s.ended.Watch():
				return nil, ErrDialogEnded
			case <-time.After(delay):
			}
		case (res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired) && !last && s.opts.Username != "" && s.opts.Password != "":
			if auth, err = s.digestHeader(req, res); err != nil {
				return nil, err
			}
		case res.StatusCode == sip.StatusCallTransactionDoesNotExists || res.StatusCode == sip.StatusRequestTimeout:
			// The remote side no longer has the dialog (RFC 3261 section 12.2.1.2).
			s.ended.Break()
			return nil, fmt.Errorf("%w: re-INVITE answered %d %s", ErrDialogEnded, res.StatusCode, res.Reason)
		default:
			return nil, fmt.Errorf("re-INVITE rejected: %w", sipStatusFromResponse(res))
		}
	}
}

// digestHeader answers the challenge in the response to the request.
func (s *DialogSession) digestHeader(req *sip.Request, res *sip.Response) (sip.Header, error) {
	challengeName, name := "WWW-Authenticate", "Authorization"
	if res.StatusCode == sip.StatusProxyAuthRequired {
		challengeName, name = "Proxy-Authenticate", "Proxy-Authorization"
	}
	cred, err := digestCredentials(res, challengeName, req.Method, req.Recipient.String(), s.opts.Username, s.opts.Password)
	if err != nil {
		return nil, err
	}
	return sip.NewHeader(name, cred), nil
}

// inviteAccepted handles the 2xx to this side's re-INVITE: it refreshes the
// remote target, sends the ACK, and records the new session descriptions.
func (s *DialogSession) inviteAccepted(req *sip.Request, res *sip.Response, offer []byte, auth sip.Header) ([]byte, error) {
	answer := res.Body()
	s.mu.Lock()
	if c := res.Contact(); c != nil && c.Address.Host != "" {
		// A 2xx to a re-INVITE refreshes the remote target (RFC 3261 section 12.2.1.2).
		s.target = *c.Address.Clone()
		s.d.RemoteTarget = c.Address.String()
	}
	// The ACK has the sequence number of the INVITE and its credentials
	// (RFC 3261 section 13.2.2.4).
	ack := s.buildRequestLocked(sip.ACK, requestCSeq(req))
	s.d.LocalSDP = slices.Clone(offer)
	if len(answer) != 0 {
		s.d.RemoteSDP = slices.Clone(answer)
	}
	snapshot := s.changedLocked()
	s.mu.Unlock()

	if auth != nil {
		ack.AppendHeader(sip.HeaderClone(auth))
	}
	// The ACK goes out before the handler runs: the remote side sends the
	// 2xx again until it has the ACK. The dialog changed even if the ACK is
	// lost, so the handler gets the snapshot in both cases.
	err := s.cli.sipCli.WriteRequest(ack)
	s.emit(snapshot)
	if err != nil {
		return nil, fmt.Errorf("ACK: %w", err)
	}
	s.log.Infow("re-INVITE accepted", "cseq", requestCSeq(req), "status", res.StatusCode)
	return answer, nil
}

// Bye ends the dialog with a BYE and waits for its final response. The
// session is ended when Bye returns, whatever the response.
func (s *DialogSession) Bye(ctx context.Context, headers map[string]string) error {
	s.mu.Lock()
	if s.ended.IsBroken() {
		s.mu.Unlock()
		return ErrDialogEnded
	}
	req := s.newRequestLocked(sip.BYE)
	s.ended.Break()
	s.mu.Unlock()
	for k, v := range headers {
		req.AppendHeader(sip.NewHeader(k, v))
	}
	res, err := sendBye(ctx, s.log, s.transaction, req)
	if err != nil {
		return fmt.Errorf("BYE: %w", err)
	}
	// A 481 means that the remote side ended the dialog first.
	if !byeSucceeded(res, nil) && res.StatusCode != sip.StatusCallTransactionDoesNotExists {
		return fmt.Errorf("BYE rejected: %w", sipStatusFromResponse(res))
	}
	return nil
}

// HandleInvite answers a re-INVITE of the remote side (RFC 3261 sections
// 12.2.2 and 14.2). It rejects an INVITE that is out of order, or that
// arrives during another INVITE on the dialog.
//
// An INVITE without an offer gets the last description this side sent. The
// answer in its ACK is not applied.
func (s *DialogSession) HandleInvite(req *sip.Request, tx sip.ServerTransaction) {
	cseq := requestCSeq(req)
	offer := sdpBodyFromRequest(req)

	s.mu.Lock()
	var code sip.StatusCode
	retryAfter := false
	switch {
	case s.ended.IsBroken():
		code = sip.StatusCallTransactionDoesNotExists
	case cseq <= s.d.RemoteCSeq:
		code = sip.StatusInternalServerError
	case s.inviting:
		code = statusRequestPending
	case s.answering:
		code, retryAfter = sip.StatusInternalServerError, true
	}
	if code != 0 {
		s.mu.Unlock()
		s.log.Infow("rejecting re-INVITE", "cseq", cseq, "status", code)
		res := newResponse(req, code, "", nil)
		if retryAfter {
			res.AppendHeader(sip.NewHeader("Retry-After", strconv.Itoa(rand.IntN(11))))
		}
		_ = tx.Respond(res)
		return
	}
	s.answering = true
	answer := s.d.LocalSDP
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.answering = false
		s.mu.Unlock()
	}()

	if len(offer) != 0 {
		var err error
		if answer, err = s.opts.OnOffer(offer); err != nil {
			code, reason := reinviteRejection(err)
			s.log.Warnw("rejecting re-INVITE, cannot apply the offer", err, "cseq", cseq, "status", code)
			_ = tx.Respond(newResponse(req, code, reason, nil))
			return
		}
	}
	res := newResponse(req, sip.StatusOK, "", answer)
	res.AppendHeader(s.contact(string(legTransportFromReq(req))))
	res.AppendHeader(sip.NewHeader("Content-Type", contentTypeSDP))
	res.AppendHeader(sip.HeaderClone(allowHeader))
	if err := tx.Respond(res); err != nil {
		s.log.Warnw("cannot answer re-INVITE", err, "cseq", cseq)
		return
	}
	s.log.Infow("accepted re-INVITE", "cseq", cseq)

	s.mu.Lock()
	s.d.RemoteCSeq = max(s.d.RemoteCSeq, cseq)
	if len(offer) != 0 {
		s.d.RemoteSDP = slices.Clone(offer)
		s.d.LocalSDP = slices.Clone(answer)
	}
	if c := req.Contact(); c != nil && c.Address.Host != "" {
		// A re-INVITE refreshes the remote target (RFC 3261 section 12.2.2).
		s.target = *c.Address.Clone()
		s.d.RemoteTarget = c.Address.String()
	}
	snapshot := s.changedLocked()
	s.mu.Unlock()
	s.emit(snapshot)
}

// HandleAck takes the ACK for a 2xx this side sent to a re-INVITE. An ACK
// gets no response.
func (s *DialogSession) HandleAck(req *sip.Request, tx sip.ServerTransaction) {}

// HandleBye answers the BYE of the remote side and ends the session.
func (s *DialogSession) HandleBye(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(newResponse(req, sip.StatusOK, "", nil))
	var reason ReasonHeader
	if h := req.GetHeader("Reason"); h != nil {
		// A Reason that does not parse is no reason to keep the dialog.
		reason, _ = ParseReasonHeader(h.Value())
	}
	s.mu.Lock()
	if s.remoteBye == nil && !s.ended.IsBroken() {
		s.remoteBye = &reason
	}
	s.ended.Break()
	s.mu.Unlock()
	s.log.Infow("BYE from remote", "reason", reason.String())
}

// reinviteRejection is the response to a re-INVITE whose SDP could not be
// applied. An SDPError means that the peer offered something this side does
// not support: a 488. Another error is this side's own: a 500.
func reinviteRejection(err error) (sip.StatusCode, string) {
	if e := (SDPError{}); !errors.As(err, &e) {
		return sip.StatusInternalServerError, "Internal Server Error"
	}
	reason := "Invalid SDP"
	switch {
	case errors.Is(err, sdp.ErrNoCommonMedia):
		reason = "No Acceptable Codec"
	case errors.Is(err, sdp.ErrNoCommonCrypto):
		reason = "No Acceptable Encryption Profile"
	}
	return sip.StatusNotAcceptableHere, reason
}
