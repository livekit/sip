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
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
)

const dialogTestTimeout = 2 * time.Second

func testDialog() *Dialog {
	return &Dialog{
		Version:       7,
		State:         DialogConfirmed,
		Direction:     livekit.SIPCallDirection_SCD_OUTBOUND,
		CallID:        "call-1",
		LocalTag:      "local-tag",
		RemoteTag:     "remote-tag",
		LocalURI:      "sip:+15550111@sip.example.com",
		RemoteURI:     "sip:+15550100@carrier.example.com",
		RemoteTarget:  "sip:callee@203.0.113.9:5070",
		RouteSet:      []string{"sip:10.0.0.1:5060;lr", "sip:far-proxy.example.com;lr"},
		NextLocalCSeq: 3,
		RemoteCSeq:    10,
		LocalSDP:      []byte("local-sdp"),
		RemoteSDP:     []byte("remote-sdp"),
	}
}

type sessionDialogs struct {
	mu  sync.Mutex
	got []*Dialog
}

func (r *sessionDialogs) HandleDialog(d *Dialog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, d)
}

func (r *sessionDialogs) all() []*Dialog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Dialog(nil), r.got...)
}

// testOnOffer answers each offer with the same description.
func testOnOffer([]byte) ([]byte, error) { return []byte("test-answer"), nil }

// newTestDialogSession creates a session; without OnOffer in opts it gets
// testOnOffer.
func newTestDialogSession(t *testing.T, d *Dialog, opts DialogSessionOptions) (*testSIPHarness, *DialogSession) {
	t.Helper()
	h := NewTestSIP(t, TestSIPConfig{})
	if opts.OnOffer == nil {
		opts.OnOffer = testOnOffer
	}
	s, err := NewDialogSession(h.Client, d, opts)
	require.NoError(t, err)
	return h, s
}

type inviteResult struct {
	answer []byte
	err    error
}

func startSessionInvite(s *DialogSession, offer string) <-chan inviteResult {
	out := make(chan inviteResult, 1)
	go func() {
		answer, err := s.Invite(context.Background(), []byte(offer))
		out <- inviteResult{answer, err}
	}()
	return out
}

func waitSessionInvite(t *testing.T, ch <-chan inviteResult) inviteResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(dialogTestTimeout):
		t.Fatal("Invite did not return")
		return inviteResult{}
	}
}

// remoteRequest builds a request of the remote side within the test dialog.
func remoteRequest(method sip.RequestMethod, cseq uint32, body string) *sip.Request {
	req := sip.NewRequest(method, sip.Uri{User: "+15550111", Host: "192.0.2.1", Port: 5060})
	from := &sip.FromHeader{Address: sip.Uri{User: "+15550100", Host: "carrier.example.com"}, Params: sip.NewParams()}
	from.Params.Add("tag", "remote-tag")
	to := &sip.ToHeader{Address: sip.Uri{User: "+15550111", Host: "sip.example.com"}, Params: sip.NewParams()}
	to.Params.Add("tag", "local-tag")
	callID := sip.CallIDHeader("call-1")
	via := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "203.0.113.9", Port: 5070, Params: sip.NewParams()}
	via.Params.Add("branch", sip.GenerateBranchN(16))
	req.AppendHeader(via)
	req.AppendHeader(from)
	req.AppendHeader(to)
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	if body != "" {
		req.AppendHeader(sip.NewHeader("Content-Type", contentTypeSDP))
		req.SetBody([]byte(body))
	}
	req.SetSource(testSIPSource)
	req.SetDestination(testSIPSource)
	return req
}

func newTestServerTx(h *testSIPHarness, req *sip.Request) *testSIPServerTransaction {
	return &testSIPServerTransaction{
		log:       h.log,
		req:       req,
		responses: make(chan *sip.Response, testSIPTxBuffer),
		acks:      make(chan *sip.Request, testSIPTxBuffer),
		cancels:   make(chan *sip.Request, testSIPTxBuffer),
		done:      make(chan struct{}),
		err:       make(chan error, 1),
	}
}

func TestNewDialogSessionValidates(t *testing.T) {
	h := NewTestSIP(t, TestSIPConfig{})
	opts := DialogSessionOptions{OnOffer: testOnOffer}
	for name, change := range map[string]func(d *Dialog){
		"no state":       func(d *Dialog) { d.State = "" },
		"no call id":     func(d *Dialog) { d.CallID = "" },
		"no remote tag":  func(d *Dialog) { d.RemoteTag = "" },
		"bad target":     func(d *Dialog) { d.RemoteTarget = "" },
		"bad local uri":  func(d *Dialog) { d.LocalURI = "not a uri" },
		"bad remote uri": func(d *Dialog) { d.RemoteURI = "" },
		"bad route":      func(d *Dialog) { d.RouteSet = []string{""} },
	} {
		t.Run(name, func(t *testing.T) {
			d := testDialog()
			change(d)
			_, err := NewDialogSession(h.Client, d, opts)
			require.Error(t, err)
		})
	}
	_, err := NewDialogSession(nil, testDialog(), opts)
	require.Error(t, err)
	_, err = NewDialogSession(h.Client, nil, opts)
	require.Error(t, err)
	// A session without OnOffer could not answer an offer.
	_, err = NewDialogSession(h.Client, testDialog(), DialogSessionOptions{})
	require.Error(t, err)
	_, err = NewDialogSession(h.Client, testDialog(), opts)
	require.NoError(t, err)
}

// A re-INVITE is built from the dialog, its 2xx is acknowledged, and the
// dialog takes the new descriptions and the new remote target.
func TestDialogSessionInvite(t *testing.T) {
	rec := &sessionDialogs{}
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{Handler: rec})
	res := startSessionInvite(s, "new-offer")

	tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	req := tr.req
	require.Equal(t, sip.INVITE, req.Method)
	require.Equal(t, "sip:callee@203.0.113.9:5070", req.Recipient.String())
	routes := req.GetHeaders("Route")
	require.Len(t, routes, 2)
	require.Equal(t, "<sip:10.0.0.1:5060;lr>", routes[0].Value())
	require.Equal(t, "<sip:far-proxy.example.com;lr>", routes[1].Value())
	require.Equal(t, "10.0.0.1:5060", req.Destination(), "the request goes to the first route")
	require.Equal(t, "call-1", req.CallID().Value())
	require.Equal(t, "sip:+15550111@sip.example.com", req.From().Address.String())
	require.Equal(t, "local-tag", req.From().Params.GetOr("tag", ""))
	require.Equal(t, "sip:+15550100@carrier.example.com", req.To().Address.String())
	require.Equal(t, "remote-tag", req.To().Params.GetOr("tag", ""))
	require.Equal(t, uint32(3), req.CSeq().SeqNo)
	require.NotNil(t, req.Contact())
	require.Equal(t, contentTypeSDP, req.ContentType().Value())
	require.Equal(t, "new-offer", string(req.Body()))

	ok := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", []byte("new-answer"))
	ok.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "callee", Host: "203.0.113.20", Port: 5080}})
	require.NoError(t, tr.transaction.SendResponse(ok))

	got := waitSessionInvite(t, res)
	require.NoError(t, got.err)
	require.Equal(t, "new-answer", string(got.answer))

	ack := h.WaitRequest(t, dialogTestTimeout, "local-tag", "").req
	require.Equal(t, sip.ACK, ack.Method)
	require.Equal(t, uint32(3), ack.CSeq().SeqNo, "the ACK has the INVITE's sequence number")
	require.Equal(t, sip.ACK, ack.CSeq().MethodName)
	require.Equal(t, "sip:callee@203.0.113.20:5080", ack.Recipient.String(), "the ACK goes to the refreshed target")
	require.Len(t, ack.GetHeaders("Route"), 2)

	d := s.Dialog()
	require.Greater(t, d.Version, uint64(7))
	require.Equal(t, uint32(4), d.NextLocalCSeq)
	require.Equal(t, "sip:callee@203.0.113.20:5080", d.RemoteTarget)
	require.Equal(t, "new-offer", string(d.LocalSDP))
	require.Equal(t, "new-answer", string(d.RemoteSDP))
	require.Equal(t, []*Dialog{d}, rec.all())
}

func TestDialogSessionTransportOption(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{Transport: TransportTCP})
	startSessionInvite(s, "offer")
	req := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "").req
	require.Equal(t, "TCP", req.Transport())
	tr, _ := req.Contact().Address.UriParams.Get("transport")
	require.Equal(t, "tcp", tr)
}

// A first route without the lr parameter is a strict router: it is the
// Request-URI, and the remote target is the last route.
func TestDialogSessionStrictRoute(t *testing.T) {
	d := testDialog()
	d.RouteSet = []string{"sip:10.0.0.1:5060", "sip:far-proxy.example.com;lr"}
	h, s := newTestDialogSession(t, d, DialogSessionOptions{})
	startSessionInvite(s, "offer")
	req := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "").req
	require.Equal(t, "sip:10.0.0.1:5060", req.Recipient.String())
	routes := req.GetHeaders("Route")
	require.Len(t, routes, 2)
	require.Equal(t, "<sip:far-proxy.example.com;lr>", routes[0].Value())
	require.Equal(t, "<sip:callee@203.0.113.9:5070>", routes[1].Value())
	require.Equal(t, "10.0.0.1:5060", req.Destination(), "the request goes to the strict router")
}

// deadTx is a client transaction that ends without a response.
type deadTx struct {
	sip.ClientTransaction
	done chan struct{}
}

func (t *deadTx) Responses() <-chan *sip.Response { return nil }
func (t *deadTx) Done() <-chan struct{}           { return t.done }
func (t *deadTx) Err() error                      { return nil }
func (t *deadTx) Terminate()                      {}
func (t *deadTx) Cancel() error                   { return nil }

type deadTxClient struct{ SIPClient }

func (deadTxClient) TransactionRequest(*sip.Request, ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	done := make(chan struct{})
	close(done)
	return &deadTx{done: done}, nil
}

// A re-INVITE that gets no final response ends the dialog, as a 408 does.
func TestDialogSessionInviteNoResponse(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	h.Client.sipCli = deadTxClient{h.Client.sipCli}

	_, err := s.Invite(context.Background(), []byte("offer"))
	require.ErrorIs(t, err, ErrDialogEnded)
	select {
	case <-s.Ended():
	default:
		t.Fatal("the session did not end")
	}
}

// cancelWatchClient closes cancelled when the transaction it started is
// cancelled.
type cancelWatchClient struct {
	SIPClient
	cancelled chan struct{}
}

func (c cancelWatchClient) TransactionRequest(req *sip.Request, opts ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	tx, err := c.SIPClient.TransactionRequest(req, opts...)
	if err != nil {
		return nil, err
	}
	return &cancelWatchTx{ClientTransaction: tx, cancelled: c.cancelled}, nil
}

type cancelWatchTx struct {
	sip.ClientTransaction
	cancelled chan struct{}
}

func (t *cancelWatchTx) Cancel() error {
	close(t.cancelled)
	return nil
}

// The remote side can accept a re-INVITE before it sees the CANCEL. The
// session then sends the ACK and Invite returns the answer.
func TestDialogSessionInviteCancelled(t *testing.T) {
	prev := cancelResponseGrace
	t.Cleanup(func() { cancelResponseGrace = prev })

	// start calls Invite and cancels it when the request is out. It returns
	// the INVITE's transaction after the CANCEL.
	start := func(t *testing.T, h *testSIPHarness, s *DialogSession) (*transactionRequest, <-chan inviteResult) {
		t.Helper()
		cancelled := make(chan struct{})
		h.Client.sipCli = cancelWatchClient{h.Client.sipCli, cancelled}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		out := make(chan inviteResult, 1)
		go func() {
			answer, err := s.Invite(ctx, []byte("offer"))
			out <- inviteResult{answer, err}
		}()
		tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
		cancel()
		select {
		case <-cancelled:
		case <-time.After(dialogTestTimeout):
			t.Fatal("no CANCEL was sent")
		}
		return tr, out
	}

	t.Run("accepted", func(t *testing.T) {
		cancelResponseGrace = dialogTestTimeout
		rec := &sessionDialogs{}
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{Handler: rec})
		tr, res := start(t, h, s)
		require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusOK, "OK", []byte("answer"))))

		got := waitSessionInvite(t, res)
		require.NoError(t, got.err)
		require.Equal(t, "answer", string(got.answer))
		ack := h.WaitRequest(t, dialogTestTimeout, "local-tag", "").req
		require.Equal(t, sip.ACK, ack.Method)
		require.Equal(t, uint32(3), ack.CSeq().SeqNo)
		d := s.Dialog()
		require.Equal(t, "offer", string(d.LocalSDP))
		require.Equal(t, "answer", string(d.RemoteSDP))
		require.Len(t, rec.all(), 1)
	})
	for name, final := range map[string]sip.StatusCode{
		"terminated":  sip.StatusRequestTerminated,
		"no response": 0,
	} {
		t.Run(name, func(t *testing.T) {
			cancelResponseGrace = 20 * time.Millisecond
			rec := &sessionDialogs{}
			h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{Handler: rec})
			tr, res := start(t, h, s)
			if final != 0 {
				require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, final, "", nil)))
			}

			got := waitSessionInvite(t, res)
			require.Error(t, got.err)
			require.NotErrorIs(t, got.err, ErrDialogEnded)
			select {
			case <-s.Ended():
				t.Fatal("the session ended")
			default:
			}
			require.Equal(t, "local-sdp", string(s.Dialog().LocalSDP))
			require.Empty(t, rec.all())
		})
	}
}

// After a 491 the session waits and sends the INVITE again with the next
// sequence number.
func TestDialogSessionInviteGlare(t *testing.T) {
	prev := glareRetryDelay
	var owner bool
	glareRetryDelay = func(ownsCallID bool) time.Duration { owner = ownsCallID; return 0 }
	t.Cleanup(func() { glareRetryDelay = prev })

	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	res := startSessionInvite(s, "offer")

	tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, statusRequestPending, "Request Pending", nil)))
	tr = h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	require.Equal(t, uint32(4), tr.req.CSeq().SeqNo)
	require.True(t, owner, "the caller owns the Call-ID")
	require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusOK, "OK", []byte("answer"))))

	got := waitSessionInvite(t, res)
	require.NoError(t, got.err)
	require.Equal(t, "answer", string(got.answer))
	require.Equal(t, uint32(4), h.WaitRequest(t, dialogTestTimeout, "local-tag", "").req.CSeq().SeqNo)
}

func TestDialogSessionGlareDelay(t *testing.T) {
	for range 100 {
		d := glareRetryDelay(true)
		require.GreaterOrEqual(t, d, 2100*time.Millisecond)
		require.LessOrEqual(t, d, 4*time.Second)
		d = glareRetryDelay(false)
		require.GreaterOrEqual(t, d, time.Duration(0))
		require.LessOrEqual(t, d, 2*time.Second)
	}
}

// A digest challenge is answered when the session has credentials, and the
// ACK carries the same credentials.
func TestDialogSessionInviteDigest(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{Username: "user", Password: "pass"})
	res := startSessionInvite(s, "offer")

	tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	require.Nil(t, tr.req.GetHeader("Proxy-Authorization"))
	challenge := sip.NewResponseFromRequest(tr.req, sip.StatusProxyAuthRequired, "Proxy Authentication Required", nil)
	challenge.AppendHeader(sip.NewHeader("Proxy-Authenticate", `Digest realm="example.com", nonce="abc123", algorithm=MD5`))
	require.NoError(t, tr.transaction.SendResponse(challenge))

	tr = h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	auth := tr.req.GetHeader("Proxy-Authorization")
	require.NotNil(t, auth)
	require.Contains(t, auth.Value(), `username="user"`)
	require.Contains(t, auth.Value(), `uri="sip:callee@203.0.113.9:5070"`)
	require.Equal(t, uint32(4), tr.req.CSeq().SeqNo)
	require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusOK, "OK", []byte("answer"))))

	require.NoError(t, waitSessionInvite(t, res).err)
	ack := h.WaitRequest(t, dialogTestTimeout, "local-tag", "").req
	require.Equal(t, auth.Value(), ack.GetHeader("Proxy-Authorization").Value())
}

func TestDialogSessionInviteNoCredentials(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	res := startSessionInvite(s, "offer")
	tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusUnauthorized, "Unauthorized", nil)))

	err := waitSessionInvite(t, res).err
	var st *livekit.SIPStatus
	require.ErrorAs(t, err, &st)
	require.Equal(t, livekit.SIPStatusCode(401), st.Code)
	require.Equal(t, []byte("local-sdp"), s.Dialog().LocalSDP, "a rejected offer changes nothing")
}

// A 481 means that the remote side no longer has the dialog.
func TestDialogSessionInviteDialogGone(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	res := startSessionInvite(s, "offer")
	tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusCallTransactionDoesNotExists, "Call Does Not Exist", nil)))

	require.ErrorIs(t, waitSessionInvite(t, res).err, ErrDialogEnded)
	select {
	case <-s.Ended():
	default:
		t.Fatal("the session did not end")
	}
	_, err := s.Invite(context.Background(), []byte("offer"))
	require.ErrorIs(t, err, ErrDialogEnded)
	require.ErrorIs(t, s.Bye(context.Background(), nil), ErrDialogEnded)
}

func TestDialogSessionBye(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	done := make(chan error, 1)
	go func() { done <- s.Bye(context.Background(), map[string]string{"X-Test": "1"}) }()

	tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")
	require.Equal(t, sip.BYE, tr.req.Method)
	require.Equal(t, uint32(3), tr.req.CSeq().SeqNo)
	require.Equal(t, "sip:callee@203.0.113.9:5070", tr.req.Recipient.String())
	require.Len(t, tr.req.GetHeaders("Route"), 2)
	require.Equal(t, "1", tr.req.GetHeader("X-Test").Value())
	require.Nil(t, tr.req.Contact())
	require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusOK, "OK", nil)))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(dialogTestTimeout):
		t.Fatal("Bye did not return")
	}
	<-s.Ended()
	_, sent := s.RemoteBye()
	require.False(t, sent)
	require.ErrorIs(t, s.Bye(context.Background(), nil), ErrDialogEnded)
}

func TestDialogSessionMatches(t *testing.T) {
	_, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	require.True(t, s.Matches(remoteRequest(sip.BYE, 11, "")))

	other := remoteRequest(sip.BYE, 11, "")
	other.To().Params.Add("tag", "another-tag")
	require.False(t, s.Matches(other))

	other = remoteRequest(sip.BYE, 11, "")
	other.From().Params.Add("tag", "another-tag")
	require.False(t, s.Matches(other))

	other = remoteRequest(sip.BYE, 11, "")
	other.RemoveHeader("Call-ID")
	callID := sip.CallIDHeader("call-2")
	other.AppendHeader(&callID)
	require.False(t, s.Matches(other))
}

// A re-INVITE of the remote side is answered with the result of OnOffer, and
// the dialog takes the new descriptions, sequence number and remote target.
func TestDialogSessionHandleInvite(t *testing.T) {
	rec := &sessionDialogs{}
	var offered []byte
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{
		Handler: rec,
		OnOffer: func(offer []byte) ([]byte, error) {
			offered = offer
			return []byte("local-answer"), nil
		},
	})
	req := remoteRequest(sip.INVITE, 11, "remote-offer")
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "callee", Host: "203.0.113.30", Port: 5090}})
	tx := newTestServerTx(h, req)
	s.HandleInvite(req, tx)

	res := tx.WaitResponseTimeout(t, dialogTestTimeout)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	require.Equal(t, "local-answer", string(res.Body()))
	require.NotNil(t, res.Contact())
	require.Equal(t, "remote-offer", string(offered))

	d := s.Dialog()
	require.Equal(t, uint32(11), d.RemoteCSeq)
	require.Equal(t, "remote-offer", string(d.RemoteSDP))
	require.Equal(t, "local-answer", string(d.LocalSDP))
	require.Equal(t, "sip:callee@203.0.113.30:5090", d.RemoteTarget)
	require.Equal(t, []*Dialog{d}, rec.all())

	// The ACK gets no response.
	ack := remoteRequest(sip.ACK, 11, "")
	ackTx := newTestServerTx(h, ack)
	s.HandleAck(ack, ackTx)
	select {
	case res := <-ackTx.responses:
		t.Fatalf("unexpected response to ACK: %v", res)
	default:
	}
}

// An INVITE without an offer is answered with the description this side sent
// last, without a call of OnOffer, and changes no description.
func TestDialogSessionHandleInviteKeepsSession(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{
		OnOffer: func([]byte) ([]byte, error) {
			t.Error("OnOffer was called without an offer")
			return nil, nil
		},
	})
	req := remoteRequest(sip.INVITE, 11, "")
	tx := newTestServerTx(h, req)
	s.HandleInvite(req, tx)
	res := tx.WaitResponseTimeout(t, dialogTestTimeout)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	require.Equal(t, "local-sdp", string(res.Body()))
	d := s.Dialog()
	require.Equal(t, uint32(11), d.RemoteCSeq)
	require.Equal(t, "local-sdp", string(d.LocalSDP))
	require.Equal(t, "remote-sdp", string(d.RemoteSDP))
}

func TestDialogSessionHandleInviteRejects(t *testing.T) {
	t.Run("sequence number not above the last one", func(t *testing.T) {
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
		for _, cseq := range []uint32{9, 10} {
			req := remoteRequest(sip.INVITE, cseq, "remote-offer")
			tx := newTestServerTx(h, req)
			s.HandleInvite(req, tx)
			require.Equal(t, sip.StatusInternalServerError, tx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
		}
		require.Equal(t, uint32(10), s.Dialog().RemoteCSeq)
		require.Equal(t, "remote-sdp", string(s.Dialog().RemoteSDP))
	})
	t.Run("another INVITE of the remote side in progress", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{
			OnOffer: func([]byte) ([]byte, error) {
				close(entered)
				<-release
				return []byte("local-answer"), nil
			},
		})
		first := remoteRequest(sip.INVITE, 11, "remote-offer")
		firstTx := newTestServerTx(h, first)
		go s.HandleInvite(first, firstTx)
		<-entered

		second := remoteRequest(sip.INVITE, 12, "remote-offer-2")
		secondTx := newTestServerTx(h, second)
		s.HandleInvite(second, secondTx)
		res := secondTx.WaitResponseTimeout(t, dialogTestTimeout)
		require.Equal(t, sip.StatusInternalServerError, res.StatusCode)
		require.NotNil(t, res.GetHeader("Retry-After"))
		_, err := s.Invite(context.Background(), []byte("offer"))
		require.ErrorIs(t, err, ErrRequestPending)

		close(release)
		require.Equal(t, sip.StatusOK, firstTx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
	})
	t.Run("offer not supported", func(t *testing.T) {
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{
			OnOffer: func([]byte) ([]byte, error) { return nil, SDPError{Err: errors.New("no audio")} },
		})
		req := remoteRequest(sip.INVITE, 11, "remote-offer")
		tx := newTestServerTx(h, req)
		s.HandleInvite(req, tx)
		require.Equal(t, sip.StatusNotAcceptableHere, tx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
		d := s.Dialog()
		require.Equal(t, uint32(10), d.RemoteCSeq)
		require.Equal(t, "remote-sdp", string(d.RemoteSDP))
	})
	t.Run("offer fails", func(t *testing.T) {
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{
			OnOffer: func([]byte) ([]byte, error) { return nil, errors.New("port closed") },
		})
		req := remoteRequest(sip.INVITE, 11, "remote-offer")
		tx := newTestServerTx(h, req)
		s.HandleInvite(req, tx)
		require.Equal(t, sip.StatusInternalServerError, tx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
	})
	t.Run("own INVITE in progress", func(t *testing.T) {
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
		res := startSessionInvite(s, "offer")
		tr := h.WaitTransaction(t, dialogTestTimeout, "local-tag", "")

		req := remoteRequest(sip.INVITE, 11, "remote-offer")
		tx := newTestServerTx(h, req)
		s.HandleInvite(req, tx)
		require.Equal(t, statusRequestPending, tx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
		_, err := s.Invite(context.Background(), []byte("offer"))
		require.ErrorIs(t, err, ErrRequestPending)

		require.NoError(t, tr.transaction.SendResponse(sip.NewResponseFromRequest(tr.req, sip.StatusOK, "OK", []byte("answer"))))
		require.NoError(t, waitSessionInvite(t, res).err)
	})
	t.Run("after the dialog ended", func(t *testing.T) {
		h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
		bye := remoteRequest(sip.BYE, 11, "")
		s.HandleBye(bye, newTestServerTx(h, bye))
		req := remoteRequest(sip.INVITE, 12, "remote-offer")
		tx := newTestServerTx(h, req)
		s.HandleInvite(req, tx)
		require.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
	})
}

func TestDialogSessionHandleBye(t *testing.T) {
	h, s := newTestDialogSession(t, testDialog(), DialogSessionOptions{})
	req := remoteRequest(sip.BYE, 11, "")
	req.AppendHeader(sip.NewHeader("Reason", `Q.850;cause=16;text="Normal call clearing"`))
	tx := newTestServerTx(h, req)
	s.HandleBye(req, tx)
	require.Equal(t, sip.StatusOK, tx.WaitResponseTimeout(t, dialogTestTimeout).StatusCode)
	select {
	case <-s.Ended():
	default:
		t.Fatal("the session did not end")
	}
	reason, ok := s.RemoteBye()
	require.True(t, ok)
	require.Equal(t, 16, reason.Cause)
	require.True(t, reason.IsNormal())
}

// A port that continues a session keeps the origin of the session in its
// offers and raises the version by one.
func TestMediaPortSessionOrigin(t *testing.T) {
	prev := []byte("v=0\r\no=- 4242 4250 IN IP4 198.51.100.7\r\ns=LiveKit\r\nc=IN IP4 198.51.100.7\r\nt=0 0\r\nm=audio 10000 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
	origin, err := ParseSDPOrigin(prev)
	require.NoError(t, err)
	require.Equal(t, uint64(4242), origin.SessionID)
	require.Equal(t, uint64(4250), origin.SessionVersion)

	_, err = ParseSDPOrigin([]byte("not sdp"))
	require.Error(t, err)

	mp, err := NewMediaPort(logger.NewTestLogger(t), newTestCallMonitor(t), &MediaOptions{
		IP:            netip.MustParseAddr("203.0.113.5"),
		SessionOrigin: origin,
	}, RoomSampleRate)
	require.NoError(t, err)
	t.Cleanup(mp.Close)

	offer, err := mp.GenerateOffer()
	require.NoError(t, err)
	got, err := ParseSDPOrigin(offer)
	require.NoError(t, err)
	want := *origin
	want.SessionVersion = 4251
	require.Equal(t, want, *got)
	require.Equal(t, uint64(4250), origin.SessionVersion, "the caller's origin is not changed")

	// The same pending offer is returned again without a new version.
	again, err := mp.GenerateOffer()
	require.NoError(t, err)
	require.Equal(t, offer, again)

	// An answer to the remote side's offer continues the same session.
	remote := newTestMediaPort(t, "")
	remoteOffer, err := remote.GenerateOffer()
	require.NoError(t, err)
	answer, err := mp.GenerateAnswer(remoteOffer)
	require.NoError(t, err)
	got, err = ParseSDPOrigin(answer)
	require.NoError(t, err)
	want.SessionVersion = 4252
	require.Equal(t, want, *got)
}
