package sip

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/sip"
)

func TestHandleNotify(t *testing.T) {
	const headers = "\r\nX-Foo: bar\r\n\r\n"
	req := sip.NewRequest(sip.NOTIFY, sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer"))
	req.SetBody([]byte("SIP/2.0 200 OK" + headers))

	m, c, s, r, err := handleNotify(req)
	require.NoError(t, err)
	require.Equal(t, sip.REFER, m)
	require.Equal(t, uint32(0), c)
	require.Equal(t, 200, s)
	require.Equal(t, "OK", r)

	req = sip.NewRequest(sip.NOTIFY, sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer;id=1234"))
	req.SetBody([]byte("SIP/2.0 200 OK" + headers))

	m, c, s, r, err = handleNotify(req)
	require.NoError(t, err)
	require.Equal(t, sip.REFER, m)
	require.Equal(t, uint32(1234), c)
	require.Equal(t, 200, s)
	require.Equal(t, "OK", r)

	req = sip.NewRequest(sip.NOTIFY, sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer;id=1234"))
	req.SetBody([]byte("SIP/2.0 404 Not found" + headers))

	m, c, s, r, err = handleNotify(req)
	require.NoError(t, err)
	require.Equal(t, sip.REFER, m)
	require.Equal(t, uint32(1234), c)
	require.Equal(t, 404, s)
	require.Equal(t, "Not found", r)

	req = sip.NewRequest(sip.NOTIFY, sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer;id=1234"))
	req.SetBody([]byte("SIP/3.0 200 OK" + headers))

	m, c, s, r, err = handleNotify(req)
	require.Error(t, err)

	req = sip.NewRequest(sip.NOTIFY, sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "invite;id=1234"))
	req.SetBody([]byte("SIP/2.0 200 OK" + headers))

	m, c, s, r, err = handleNotify(req)
	require.Error(t, err)
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
