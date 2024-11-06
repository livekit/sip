package sip

import (
	"testing"

	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestHandleNotify(t *testing.T) {
	req := sip.NewRequest(sip.NOTIFY, &sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer"))
	req.SetBody([]byte("SIP/2.0 200"))

	m, c, s, err := handleNotify(req)
	require.NoError(t, err)
	require.Equal(t, sip.REFER, m)
	require.Equal(t, uint32(0), c)
	require.Equal(t, 200, s)

	req = sip.NewRequest(sip.NOTIFY, &sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer;id=1234"))
	req.SetBody([]byte("SIP/2.0 200"))

	m, c, s, err = handleNotify(req)
	require.NoError(t, err)
	require.Equal(t, sip.REFER, m)
	require.Equal(t, uint32(1234), c)
	require.Equal(t, 200, s)

	req = sip.NewRequest(sip.NOTIFY, &sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer;id=1234"))
	req.SetBody([]byte("SIP/2.0 404"))

	m, c, s, err = handleNotify(req)
	require.NoError(t, err)
	require.Equal(t, sip.REFER, m)
	require.Equal(t, uint32(1234), c)
	require.Equal(t, 404, s)

	req = sip.NewRequest(sip.NOTIFY, &sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "refer;id=1234"))
	req.SetBody([]byte("SIP/3.0 200"))

	m, c, s, err = handleNotify(req)
	require.Error(t, err)

	req = sip.NewRequest(sip.NOTIFY, &sip.Uri{
		Host: "foo.bar",
	})

	req.AppendHeader(sip.NewHeader("Event", "invite;id=1234"))
	req.SetBody([]byte("SIP/2.0 200"))

	m, c, s, err = handleNotify(req)
	require.Error(t, err)
}
