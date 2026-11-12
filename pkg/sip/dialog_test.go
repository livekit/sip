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
	"testing"
	"time"

	sipgo "github.com/emiago/sipgo/sip"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func dialogTestURI(t *testing.T, s string) sip.Uri {
	t.Helper()
	var u sip.Uri
	require.NoError(t, sipgo.ParseUri(s, &u))
	return u
}

func TestDialogRouteSet(t *testing.T) {
	hs := []sip.Header{
		sip.NewHeader("Record-Route", "<sip:proxy-a.example.com;lr>, <sip:proxy-b.example.com;lr>"),
		sip.NewHeader("Record-Route", "<sip:10.0.0.1:5060;transport=tcp;lr>"),
	}
	require.Equal(t, []string{
		"sip:proxy-a.example.com;lr", "sip:proxy-b.example.com;lr", "sip:10.0.0.1:5060;transport=tcp;lr",
	}, routeSet(hs, false))
	require.Equal(t, []string{
		"sip:10.0.0.1:5060;transport=tcp;lr", "sip:proxy-b.example.com;lr", "sip:proxy-a.example.com;lr",
	}, routeSet(hs, true))
	require.Nil(t, routeSet(nil, true))
}

func TestInboundDialog(t *testing.T) {
	invite := sip.NewRequest(sip.INVITE, dialogTestURI(t, "sip:+15550100@sip.example.com"))
	invite.AppendHeader(sip.NewHeader("Record-Route", "<sip:edge.example.com;lr>"))
	invite.AppendHeader(sip.NewHeader("Record-Route", "<sip:10.0.0.1:5060;lr>"))
	invite.SetBody([]byte("remote-offer"))
	from := &sip.FromHeader{Address: dialogTestURI(t, "sip:+15550111@carrier.example.com"), Params: sip.NewParams()}
	from.Params.Add("tag", "remote-tag")
	to := &sip.ToHeader{Address: dialogTestURI(t, "sip:+15550100@sip.example.com"), Params: sip.NewParams()}
	to.Params.Add("tag", "local-tag")

	c := &sipInbound{
		id:              "local-tag",
		tag:             "remote-tag",
		sipCallID:       "call-1",
		invite:          invite,
		inviteCSeq:      7,
		from:            from,
		to:              to,
		contact:         &sip.ContactHeader{Address: dialogTestURI(t, "sip:carrier-ua@198.51.100.7:5060")},
		nextRequestCSeq: 8,
	}
	require.Nil(t, c.Dialog(), "no dialog before the answer")

	c.inviteOk = sip.NewResponseFromRequest(invite, sip.StatusOK, "OK", []byte("local-answer"))
	c.lastSDP = []byte("local-answer")
	d := c.Dialog()
	require.NotZero(t, d.Version)
	require.Equal(t, &Dialog{
		Version:       d.Version,
		State:         DialogConfirmed,
		Direction:     livekit.SIPCallDirection_SCD_INBOUND,
		CallID:        "call-1",
		LocalTag:      "local-tag",
		RemoteTag:     "remote-tag",
		LocalURI:      "sip:+15550100@sip.example.com",
		RemoteURI:     "sip:+15550111@carrier.example.com",
		RemoteTarget:  "sip:carrier-ua@198.51.100.7:5060",
		RouteSet:      []string{"sip:edge.example.com;lr", "sip:10.0.0.1:5060;lr"},
		NextLocalCSeq: 8,
		RemoteCSeq:    7,
		LocalSDP:      []byte("local-answer"),
		RemoteSDP:     []byte("remote-offer"),
	}, d)

	// The snapshot is a copy.
	d.LocalSDP[0] = 'X'
	require.Equal(t, []byte("local-answer"), c.lastSDP)

	// An INVITE without an offer takes the remote SDP from the ACK.
	c.invite.SetBody(nil)
	c.remoteSDP = nil
	ack := sip.NewRequest(sip.ACK, dialogTestURI(t, "sip:+15550100@sip.example.com"))
	ack.SetBody([]byte("remote-late-offer"))
	c.ack.Store(ack)
	require.Equal(t, []byte("remote-late-offer"), c.Dialog().RemoteSDP)
	c.invite.SetBody([]byte("remote-offer"))

	c.acceptedReinvite(9, []byte("remote-reoffer"))
	prev := d.Version
	d = c.Dialog()
	require.Greater(t, d.Version, prev)
	require.Equal(t, uint32(9), d.RemoteCSeq)
	require.Equal(t, []byte("remote-reoffer"), d.RemoteSDP)
}

func TestOutboundDialog(t *testing.T) {
	from := &sip.FromHeader{Address: dialogTestURI(t, "sip:+15550111@sip.example.com"), Params: sip.NewParams()}
	from.Params.Add("tag", "local-tag")
	to := &sip.ToHeader{Address: dialogTestURI(t, "sip:+15550100@carrier.example.com"), Params: sip.NewParams()}
	reqURI := dialogTestURI(t, "sip:+15550100@carrier.example.com")

	invite := sip.NewRequest(sip.INVITE, reqURI)
	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})
	callID := sip.CallIDHeader("call-2")
	invite.AppendHeader(&callID)
	invite.SetBody([]byte("local-offer"))
	ok := sip.NewResponseFromRequest(invite, sip.StatusOK, "OK", []byte("remote-answer"))
	// The response lists the route from the remote side's end; the caller
	// uses it reversed.
	ok.AppendHeader(sip.NewHeader("Record-Route", "<sip:far-proxy.example.com;lr>"))
	ok.AppendHeader(sip.NewHeader("Record-Route", "<sip:10.0.0.1:5060;lr>"))
	ok.AppendHeader(&sip.ContactHeader{Address: dialogTestURI(t, "sip:callee@203.0.113.9:5060")})

	c := &sipOutbound{
		id:       "local-tag",
		tag:      "remote-tag",
		uri:      &reqURI,
		from:     from,
		to:       to,
		invite:   invite,
		nextCSeq: 1,
	}
	require.Nil(t, c.Dialog(), "no dialog before the answer")

	c.inviteOk = ok
	d := c.Dialog()
	require.NotZero(t, d.Version)
	require.Equal(t, &Dialog{
		Version:       d.Version,
		State:         DialogConfirmed,
		Direction:     livekit.SIPCallDirection_SCD_OUTBOUND,
		CallID:        "call-2",
		LocalTag:      "local-tag",
		RemoteTag:     "remote-tag",
		LocalURI:      "sip:+15550111@sip.example.com",
		RemoteURI:     "sip:+15550100@carrier.example.com",
		RemoteTarget:  "sip:callee@203.0.113.9:5060",
		RouteSet:      []string{"sip:10.0.0.1:5060;lr", "sip:far-proxy.example.com;lr"},
		NextLocalCSeq: 3,
		LocalSDP:      []byte("local-offer"),
		RemoteSDP:     []byte("remote-answer"),
	}, d)

	c.RecordInvite(5)
	c.acceptedReinvite([]byte("remote-reoffer"), []byte("local-reanswer"))
	d = c.Dialog()
	require.Equal(t, uint32(5), d.RemoteCSeq)
	require.Equal(t, []byte("remote-reoffer"), d.RemoteSDP)
	require.Equal(t, []byte("local-reanswer"), d.LocalSDP)
}

type dialogRecorder struct {
	StateHandler
	got []*Dialog
}

func (r *dialogRecorder) HandleDialog(d *Dialog) { r.got = append(r.got, d) }

func TestEmitDialog(t *testing.T) {
	rec := &dialogRecorder{StateHandler: NewRPCStateHandler(nil)}
	st := NewCallState(rec, nil)
	st.EmitDialog(&Dialog{Version: 4, CallID: "call-3"})
	st.EmitDialog(nil)
	require.Equal(t, []*Dialog{{Version: 4, CallID: "call-3"}}, rec.got)

	// A handler without HandleDialog is skipped, and a nil state is safe.
	require.NotPanics(t, func() {
		NewCallState(NewRPCStateHandler(nil), nil).EmitDialog(&Dialog{})
		(*CallState)(nil).EmitDialog(&Dialog{})
	})
}

// An answered inbound call reports its dialog once the ACK arrived.
func TestInboundCallReportsDialog(t *testing.T) {
	h := &recordingStateHandler{}
	st := NewServiceTest(t, &serviceTestConfig{GetStateHandler: h.GetStateHandler()})
	_, ic := st.CreateInboundCall(t)
	require.Eventually(t, func() bool { return len(h.Dialogs()) > 0 }, 5*time.Second, 10*time.Millisecond)

	d := h.Dialogs()[0]
	require.Equal(t, DialogConfirmed, d.State)
	require.Equal(t, livekit.SIPCallDirection_SCD_INBOUND, d.Direction)
	require.Equal(t, ic.cc.SIPCallID(), d.CallID)
	require.NotEmpty(t, d.LocalTag)
	require.NotEmpty(t, d.RemoteTag)
	require.NotEmpty(t, d.LocalSDP)
	require.NotEmpty(t, d.RemoteSDP)
}

// An answered outbound call reports its dialog once the ACK was sent.
func TestOutboundCallReportsDialog(t *testing.T) {
	req := MinimalCreateSIPParticipantRequest()
	states, h, tr := startOutboundCall(t, req)
	answerOutbound(t, h, tr, req.SipCallId)
	require.Eventually(t, func() bool { return len(states.Dialogs()) > 0 }, time.Second, 10*time.Millisecond)

	d := states.Dialogs()[0]
	require.Equal(t, DialogConfirmed, d.State)
	require.Equal(t, livekit.SIPCallDirection_SCD_OUTBOUND, d.Direction)
	require.Equal(t, tr.req.CallID().Value(), d.CallID)
	require.Equal(t, tr.req.CSeq().SeqNo+1, d.NextLocalCSeq)
	require.Equal(t, []byte(testMinimalSDP), d.RemoteSDP)
}
