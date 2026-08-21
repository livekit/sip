package sip

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo/sip"
)

func TestHandleNotify(t *testing.T) {
	const headers = "\r\nX-Foo: bar\r\n\r\n"
	newNotify := func(event string, subState string, body string) *sip.Request {
		req := sip.NewRequest(sip.NOTIFY, sip.Uri{
			Host: "foo.bar",
		})
		req.AppendHeader(sip.NewHeader("Event", event))
		if subState != "" {
			req.AppendHeader(sip.NewHeader("Subscription-State", subState))
		}
		if body != "" {
			req.SetBody([]byte(body))
		}
		return req
	}

	cases := []struct {
		Name     string
		Event    string
		SubState string
		Body     string
		Expect   notifyInfo
		Error    bool
	}{
		{
			Name:   "no id",
			Event:  "refer",
			Body:   "SIP/2.0 200 OK" + headers,
			Expect: notifyInfo{Method: sip.REFER, Status: 200, Reason: "OK"},
		},
		{
			Name:   "id",
			Event:  "refer;id=1234",
			Body:   "SIP/2.0 200 OK" + headers,
			Expect: notifyInfo{Method: sip.REFER, CSeq: 1234, Status: 200, Reason: "OK"},
		},
		{
			Name:   "failure",
			Event:  "refer;id=1234",
			Body:   "SIP/2.0 404 Not found" + headers,
			Expect: notifyInfo{Method: sip.REFER, CSeq: 1234, Status: 404, Reason: "Not found"},
		},
		{
			Name:     "active",
			Event:    "refer",
			SubState: "active;expires=60",
			Body:     "SIP/2.0 100 Trying" + headers,
			Expect: notifyInfo{Method: sip.REFER, Status: 100, Reason: "Trying",
				Sub: SubscriptionState{State: "active", Expires: 60}},
		},
		{
			Name:     "terminated",
			Event:    "refer;id=1234",
			SubState: "terminated;reason=noresource",
			Body:     "SIP/2.0 100 Trying" + headers,
			Expect: notifyInfo{Method: sip.REFER, CSeq: 1234, Status: 100, Reason: "Trying",
				Sub: SubscriptionState{State: "terminated", Reason: "noresource"}},
		},
		{
			Name:     "terminated no body",
			Event:    "refer",
			SubState: "TERMINATED ; Reason = GiveUp",
			Expect: notifyInfo{Method: sip.REFER,
				Sub: SubscriptionState{State: "terminated", Reason: "giveup"}},
		},
		{
			Name:     "unparsable state",
			Event:    "refer",
			SubState: "???",
			Body:     "SIP/2.0 200 OK" + headers,
			Expect: notifyInfo{Method: sip.REFER, Status: 200, Reason: "OK",
				Sub: SubscriptionState{State: "???"}},
		},
		{
			Name:  "bad SIP version",
			Event: "refer;id=1234",
			Body:  "SIP/3.0 200 OK" + headers,
			Error: true,
		},
		{
			Name:  "unknown event",
			Event: "invite;id=1234",
			Body:  "SIP/2.0 200 OK" + headers,
			Error: true,
		},
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			info, err := handleNotify(newNotify(c.Event, c.SubState, c.Body))
			if c.Error {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.Expect, info)
		})
	}
}

func TestParseSubscriptionState(t *testing.T) {
	cases := []struct {
		Name       string
		Header     string
		State      SubscriptionState
		Terminated bool
	}{
		{
			Name:   "active",
			Header: "active",
			State:  SubscriptionState{State: "active"},
		},
		{
			Name:   "active with expires",
			Header: "active;expires=3600",
			State:  SubscriptionState{State: "active", Expires: 3600},
		},
		{
			Name:   "pending",
			Header: "pending;expires=0",
			State:  SubscriptionState{State: "pending"},
		},
		{
			Name:       "terminated",
			Header:     "terminated",
			State:      SubscriptionState{State: "terminated"},
			Terminated: true,
		},
		{
			Name:       "terminated with reason",
			Header:     "terminated;reason=noresource;retry-after=0",
			State:      SubscriptionState{State: "terminated", Reason: "noresource"},
			Terminated: true,
		},
		{
			Name:       "mixed case and spaces",
			Header:     " Terminated ; Reason = TimeOut ",
			State:      SubscriptionState{State: "terminated", Reason: "timeout"},
			Terminated: true,
		},
		{
			Name:   "params without values",
			Header: "active;foo;expires",
			State:  SubscriptionState{State: "active"},
		},
		{
			Name:   "empty",
			Header: "  ",
			State:  SubscriptionState{},
		},
		{
			Name:   "extension substate",
			Header: "waiting;reason=noresource",
			State:  SubscriptionState{State: "waiting", Reason: "noresource"},
		},
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			st := ParseSubscriptionState(c.Header)
			require.Equal(t, c.State, st)
			require.Equal(t, c.Terminated, st.Terminated())
		})
	}
}

func TestHandleReferNotify(t *testing.T) {
	const referCseq = 8
	cases := []struct {
		Name string
		Info notifyInfo
		// Expect describes the result handed to referDone. Nil means nothing at
		// all should be sent.
		Expect func(t *testing.T, err error)
	}{
		{
			Name: "provisional",
			Info: notifyInfo{Status: 100, Sub: SubscriptionState{State: "active"}},
		},
		{
			Name: "provisional no state",
			Info: notifyInfo{Status: 180},
		},
		{
			Name: "other refer",
			Info: notifyInfo{CSeq: referCseq + 1, Status: 200},
		},
		{
			Name: "success",
			Info: notifyInfo{CSeq: referCseq, Status: 200},
			Expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			Name: "success terminates subscription",
			Info: notifyInfo{Status: 200, Sub: SubscriptionState{State: "terminated", Reason: "noresource"}},
			Expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			Name: "terminated on provisional",
			Info: notifyInfo{Status: 100, Sub: SubscriptionState{State: "terminated", Reason: "noresource"}},
			Expect: func(t *testing.T, err error) {
				require.ErrorIs(t, err, errReferSubscriptionTerminated)
				require.Contains(t, err.Error(), "noresource")
				var psErr psrpc.Error
				require.ErrorAs(t, err, &psErr)
				require.Equal(t, psrpc.UpstreamServerError, psErr.Code())
				var sipErr *livekit.SIPStatus
				require.NotErrorAs(t, err, &sipErr, "no SIP status was reported for this transfer")
			},
		},
		{
			Name: "terminated without body",
			Info: notifyInfo{Sub: SubscriptionState{State: "terminated", Reason: "giveup"}},
			Expect: func(t *testing.T, err error) {
				require.ErrorIs(t, err, errReferSubscriptionTerminated)
				require.Contains(t, err.Error(), "giveup")
			},
		},
		{
			Name: "terminated without reason",
			Info: notifyInfo{Status: 100, Sub: SubscriptionState{State: "terminated"}},
			Expect: func(t *testing.T, err error) {
				require.ErrorIs(t, err, errReferSubscriptionTerminated)
				require.Contains(t, err.Error(), "unspecified")
			},
		},
		{
			Name: "failure",
			Info: notifyInfo{Status: 480, Reason: "Temporarily Unavailable",
				Sub: SubscriptionState{State: "terminated", Reason: "noresource"}},
			Expect: func(t *testing.T, err error) {
				require.Error(t, err)
				var sipErr *livekit.SIPStatus
				require.ErrorAs(t, err, &sipErr)
				require.Equal(t, livekit.SIPStatusCode(480), sipErr.Code)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			// Buffered so the cases expecting no result don't wait out notifyAckTimeout.
			referDone := make(chan error, 1)
			handleReferNotify(c.Info, referCseq, referDone)
			if c.Expect == nil {
				require.Empty(t, referDone, "expected no transfer result")
				return
			}
			require.Len(t, referDone, 1, "expected a transfer result")
			c.Expect(t, <-referDone)
		})
	}
}

func TestWaitReferResult(t *testing.T) {
	log := logger.GetLogger()

	closedChan := func() chan struct{} {
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	t.Run("result", func(t *testing.T) {
		referDone := make(chan error, 1)
		referDone <- nil
		require.NoError(t, waitReferResult(t.Context(), log, nil, referDone))
	})

	t.Run("result wins over call end", func(t *testing.T) {
		// Models a NOTIFY handler parked on the unbuffered handoff while the BYE
		// is processed elsewhere.
		referDone := make(chan error)
		go func() { referDone <- nil }()
		require.NoError(t, waitReferResult(t.Context(), log, closedChan(), referDone))
	})

	t.Run("failure wins over call end", func(t *testing.T) {
		referDone := make(chan error)
		want := psrpc.NewErrorf(psrpc.UpstreamClientError, "call transfer failed")
		go func() { referDone <- want }()
		require.ErrorIs(t, waitReferResult(t.Context(), log, closedChan(), referDone), want)
	})

	t.Run("call ended", func(t *testing.T) {
		start := time.Now()
		err := waitReferResult(t.Context(), log, closedChan(), make(chan error))
		require.ErrorIs(t, err, errTransferCallEnded)
		var psErr psrpc.Error
		require.ErrorAs(t, err, &psErr)
		require.Equal(t, psrpc.Aborted, psErr.Code())
		require.GreaterOrEqual(t, time.Since(start), referResultGrace)
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := waitReferResult(ctx, log, nil, make(chan error))
		var psErr psrpc.Error
		require.ErrorAs(t, err, &psErr)
		require.Equal(t, psrpc.Canceled, psErr.Code())
	})
}

func TestParseReason(t *testing.T) {
	cases := []struct {
		Name   string
		Header string
		Reason ReasonHeader
		Normal bool
	}{
		{
			Name:   "SIP",
			Header: `SIP ;cause=200 ;text="Call completed elsewhere"`,
			Reason: ReasonHeader{
				Type:  "sip",
				Cause: 200,
				Text:  "Call completed elsewhere",
			},
			Normal: true,
		},
		{
			Name:   "SIP no cause",
			Header: `SIP;description="User Hung Up"`,
			Reason: ReasonHeader{
				Type:  "sip",
				Cause: 0,
				Text:  "User Hung Up",
			},
			Normal: true,
		},
		{
			Name:   "Q.850",
			Header: `Q.850;cause=16;text="Terminated"`,
			Reason: ReasonHeader{
				Type:  "q.850",
				Cause: 16,
				Text:  "Terminated",
			},
			Normal: true,
		},
		{
			Name:   "X.int",
			Header: `X.int;text="0x00000000";add-info=05CC.0001.0001`,
			Reason: ReasonHeader{
				Type:  "x.int",
				Cause: 0x00,
			},
			Normal: true,
		},
		{
			Name:   "X.int not ok text",
			Header: `X.int;text="0x00000001";add-info=05CC.0001.0001`,
			Reason: ReasonHeader{
				Type:  "x.int",
				Cause: 0x01,
			},
			Normal: false,
		},
		{
			Name:   "X.int reason code",
			Header: `X.int;reasoncode=0x0000032D;add-info=05CC.0001.0004`,
			Reason: ReasonHeader{
				Type:  "x.int",
				Cause: 0x32D,
			},
			Normal: false,
		},
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			r, err := ParseReasonHeader(c.Header)
			require.NoError(t, err)
			require.Equal(t, c.Reason, r)
			require.Equal(t, c.Normal, r.IsNormal())
		})
	}
}
