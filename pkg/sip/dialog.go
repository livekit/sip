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
	"slices"
	"strings"
	"sync/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sipgo/sip"
)

// Dialog is the state of an established SIP dialog (RFC 3261 section 12),
// with the session descriptions currently in use. It holds what a user agent
// needs to send a new request within the dialog, such as a re-INVITE that
// changes the media address, or a BYE.
type Dialog struct {
	// Version increases with each snapshot. Snapshots can reach the handler
	// out of order; a snapshot with a lower Version than one already handled
	// is stale.
	Version   uint64
	State     DialogState
	Direction livekit.SIPCallDirection

	// CallID is the SIP Call-ID.
	CallID string
	// LocalTag and RemoteTag identify the dialog together with the Call-ID.
	// The local tag is the From tag of this side's requests.
	LocalTag  string
	RemoteTag string
	// LocalURI and RemoteURI are the From and To URIs of this side's
	// requests, without tags.
	LocalURI  string
	RemoteURI string
	// RemoteTarget is the remote side's Contact URI: the Request-URI of
	// this side's requests.
	RemoteTarget string
	// RouteSet is the dialog's route set in the order this side's requests
	// use it: the first entry is the next hop.
	RouteSet []string

	// NextLocalCSeq is the lowest CSeq a new request from this side may use.
	// Requests sent after the snapshot raise it, so a user agent that sends
	// from a snapshot should start well above it.
	NextLocalCSeq uint32
	// RemoteCSeq is the highest CSeq of the remote side's INVITEs: the one
	// that created the dialog and the re-INVITEs this side accepted.
	RemoteCSeq uint32

	// LocalSDP and RemoteSDP are the last session descriptions each side sent.
	LocalSDP  []byte
	RemoteSDP []byte
}

// DialogState is the state of a dialog (RFC 3261 section 12).
type DialogState string

const (
	// DialogConfirmed: a 2xx answered the INVITE that created the dialog.
	DialogConfirmed DialogState = "confirmed"
)

// dialogVersion orders the dialog snapshots of all calls: a later snapshot
// has a higher Version.
var dialogVersion atomic.Uint64

// DialogHandler is an optional extension of StateHandler. When the handler
// returned by GetStateHandler implements it, CallState sends it a snapshot of
// the call's dialog each time the dialog changes: when it is established and
// after each re-INVITE this side accepts.
//
// Snapshots can reach the handler out of order; a handler that keeps the
// dialog should drop a snapshot with a lower Version than one it already has.
//
// Like the StateHandler methods, it is called while CallState holds its
// internal mutex: implementations must not block and must not call back into
// the same CallState. The Dialog is a copy the handler may keep.
type DialogHandler interface {
	HandleDialog(d *Dialog)
}

// EmitDialog sends the dialog to the handler, if it takes dialogs. A nil
// dialog is dropped.
func (s *CallState) EmitDialog(d *Dialog) {
	if s == nil || d == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.handler.(DialogHandler); ok {
		h.HandleDialog(d)
	}
}

// routeSet renders Record-Route headers as URIs in the order this side's
// requests use them: the callee keeps the order of the request that created
// the dialog, the caller reverses the order of the response (RFC 3261
// sections 12.1.1 and 12.1.2).
func routeSet(hs []sip.Header, reverse bool) []string {
	var out []string
	for _, h := range hs {
		if rr, ok := h.(*sip.RecordRouteHeader); ok {
			out = append(out, rr.Address.String())
			continue
		}
		// A header that lists several routes, separated by commas.
		for _, v := range strings.Split(h.Value(), ",") {
			v = strings.TrimSpace(v)
			if i := strings.IndexByte(v, '<'); i >= 0 {
				v = v[i+1:]
				if j := strings.IndexByte(v, '>'); j >= 0 {
					v = v[:j]
				}
			}
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if reverse {
		slices.Reverse(out)
	}
	return out
}

// Dialog returns the dialog of an inbound call, nil before this side answered.
func (c *sipInbound) Dialog() *Dialog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.invite == nil || c.inviteOk == nil {
		return nil
	}
	// The version is taken under the lock that every change to the dialog
	// holds, so a snapshot of a later state gets a higher version.
	d := &Dialog{
		Version:       dialogVersion.Add(1),
		State:         DialogConfirmed,
		Direction:     livekit.SIPCallDirection_SCD_INBOUND,
		CallID:        c.sipCallID,
		LocalTag:      string(c.id),
		RemoteTag:     string(c.tag),
		RouteSet:      routeSet(c.invite.GetHeaders("Record-Route"), false),
		NextLocalCSeq: c.nextRequestCSeq,
		RemoteCSeq:    max(c.inviteCSeq, c.remoteCSeq),
		LocalSDP:      slices.Clone(c.lastSDP),
		RemoteSDP:     slices.Clone(c.invite.Body()),
	}
	if c.remoteSDP != nil {
		d.RemoteSDP = slices.Clone(c.remoteSDP)
	} else if len(d.RemoteSDP) == 0 {
		// An INVITE without an offer: the remote side's SDP came in the ACK.
		if ack := c.ack.Load(); ack != nil {
			d.RemoteSDP = slices.Clone(ack.Body())
		}
	}
	if c.to != nil {
		d.LocalURI = c.to.Address.String()
	}
	if c.from != nil {
		d.RemoteURI = c.from.Address.String()
	}
	if c.contact != nil {
		d.RemoteTarget = c.contact.Address.String()
	}
	return d
}

// acceptedReinvite records the CSeq and SDP of a re-INVITE this side
// accepted from the remote side.
func (c *sipInbound) acceptedReinvite(cseq uint32, sdp []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remoteCSeq = max(c.remoteCSeq, cseq)
	if len(sdp) != 0 {
		c.remoteSDP = slices.Clone(sdp)
	}
}

// Dialog returns the dialog of an outbound call, nil before it was answered.
func (c *sipOutbound) Dialog() *Dialog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.invite == nil || c.inviteOk == nil {
		return nil
	}
	d := &Dialog{
		Version:       dialogVersion.Add(1),
		State:         DialogConfirmed,
		Direction:     livekit.SIPCallDirection_SCD_OUTBOUND,
		CallID:        c.callID,
		LocalTag:      string(c.id),
		RemoteTag:     string(c.tag),
		RouteSet:      routeSet(c.inviteOk.GetHeaders("Record-Route"), true),
		NextLocalCSeq: max(c.nextCSeq, requestCSeq(c.invite)+1),
		RemoteCSeq:    c.latestInviteCSeq,
		LocalSDP:      slices.Clone(c.invite.Body()),
		RemoteSDP:     slices.Clone(c.inviteOk.Body()),
	}
	if c.localSDP != nil {
		d.LocalSDP = slices.Clone(c.localSDP)
	}
	if c.remoteSDP != nil {
		d.RemoteSDP = slices.Clone(c.remoteSDP)
	}
	if d.CallID == "" {
		if h := c.invite.CallID(); h != nil {
			d.CallID = h.Value()
		}
	}
	if c.from != nil {
		d.LocalURI = c.from.Address.String()
	}
	if c.to != nil {
		d.RemoteURI = c.to.Address.String()
	}
	if h := c.inviteOk.Contact(); h != nil {
		d.RemoteTarget = h.Address.String()
	} else if c.uri != nil {
		d.RemoteTarget = c.uri.String()
	}
	return d
}

// acceptedReinvite records the SDPs of a re-INVITE this side accepted from
// the remote side: the one it sent and the one this side answered with.
// RecordInvite records its CSeq.
func (c *sipOutbound) acceptedReinvite(remote, local []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(remote) != 0 {
		c.remoteSDP = slices.Clone(remote)
	}
	if len(local) != 0 {
		c.localSDP = slices.Clone(local)
	}
}
