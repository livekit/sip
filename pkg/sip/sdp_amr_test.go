package sip

import (
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/amrwb"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	sdpsdk "github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
)

func amrEnabledCodecs() *msdk.CodecSet {
	s := defaultCodecs.NewSet()
	s.SetEnabled(amrwb.SDPNameAndRate, true)
	return s
}

func TestFilterAMROfferSDP_OctetAlign(t *testing.T) {
	// Offer from livekit/sip#747 style: PT 96 is octet-aligned (unsupported),
	// PT 98 is bandwidth-efficient with mode-change-capability (supported).
	offer := []byte(`v=0
o=- 0 0 IN IP4 1.2.3.4
s=-
c=IN IP4 1.2.3.4
t=0 0
m=audio 10000 RTP/AVP 96 98 97
a=rtpmap:96 AMR-WB/16000
a=fmtp:96 octet-align=1;mode-change-capability=2
a=rtpmap:98 AMR-WB/16000
a=fmtp:98 mode-change-capability=2
a=rtpmap:97 telephone-event/8000
a=fmtp:97 0-15
`)
	filtered, err := filterAMROfferSDP(offer)
	require.NoError(t, err)
	s := string(filtered)
	require.NotContains(t, s, "rtpmap:96")
	require.NotContains(t, s, "fmtp:96")
	require.Contains(t, s, "rtpmap:98 AMR-WB/16000")
	require.Contains(t, s, "fmtp:98 mode-change-capability=2")
	require.Contains(t, s, "rtpmap:97 telephone-event/8000")
	require.Contains(t, s, "m=audio 10000 RTP/AVP 98 97")
}

func TestFilterAMROfferSDP_OnlyOctetAlign(t *testing.T) {
	offer := []byte(`v=0
o=- 0 0 IN IP4 1.2.3.4
s=-
c=IN IP4 1.2.3.4
t=0 0
m=audio 10000 RTP/AVP 96 97
a=rtpmap:96 AMR-WB/16000
a=fmtp:96 octet-align=1;mode-change-capability=2
a=rtpmap:97 telephone-event/8000
a=fmtp:97 0-15
`)
	filtered, err := filterAMROfferSDP(offer)
	require.NoError(t, err)
	s := string(filtered)
	require.NotContains(t, s, "AMR-WB")
	require.Contains(t, s, "telephone-event")
	require.Contains(t, s, "m=audio 10000 RTP/AVP 97")
}

func TestAppendAMRFmtpToAnswer_EchoesModeChangeCapability(t *testing.T) {
	offer := []byte(`v=0
o=- 0 0 IN IP4 1.2.3.4
s=-
c=IN IP4 1.2.3.4
t=0 0
m=audio 10000 RTP/AVP 98
a=rtpmap:98 AMR-WB/16000
a=fmtp:98 mode-change-capability=2
`)
	var answer sdp.SessionDescription
	require.NoError(t, answer.Unmarshal([]byte(`v=0
o=- 0 0 IN IP4 5.6.7.8
s=LiveKit
c=IN IP4 5.6.7.8
t=0 0
m=audio 20000 RTP/AVP 98
a=rtpmap:98 AMR-WB/16000
a=ptime:20
a=sendrecv
`)))
	appendAMRFmtpToAnswer(&answer, offer)
	out, err := answer.Marshal()
	require.NoError(t, err)
	require.Contains(t, string(out), "a=fmtp:98 mode-change-capability=2")
}

func TestAMRFmtpForAnswer(t *testing.T) {
	require.Equal(t, "", amrFmtpForAnswer("octet-align=1;mode-change-capability=2"))
	require.Equal(t, "mode-change-capability=2", amrFmtpForAnswer("mode-change-capability=2"))
	require.Equal(t, "mode-change-capability=2;mode-set=0,1,2", amrFmtpForAnswer("mode-change-capability=2;mode-set=0,1,2"))
	require.Equal(t, "", amrFmtpForAnswer("octet-align=0")) // default omitted
	require.Equal(t, "", amrFmtpForAnswer(""))
}

func TestSetOffer_AMRRejectsOctetAlignOnly(t *testing.T) {
	// Exact mismatch from livekit/sip#747: offer only has octet-aligned AMR-WB.
	// We must reject rather than answer without fmtp (which implies octet-align=0).
	codecs := amrEnabledCodecs()
	c1, _ := newUDPPipe()
	port, err := NewMediaPortWith(1, logger.GetLogger(), newTestCallMonitor(t), c1, &MediaOptions{
		IP:    newIP("1.1.1.1"),
		Ports: rtcconfig.PortRange{Start: 10000},
	}, 16000)
	require.NoError(t, err)
	t.Cleanup(port.Close)

	offer := []byte(`v=0
o=- 0 0 IN IP4 2.2.2.2
s=-
c=IN IP4 2.2.2.2
t=0 0
m=audio 20000 RTP/AVP 96
a=rtpmap:96 AMR-WB/16000
a=fmtp:96 octet-align=1;mode-change-capability=2
`)
	_, _, err = port.SetOffer(offer, codecs, sdpsdk.EncryptionNone)
	require.Error(t, err)
}

func TestSetOffer_AMRAcceptsBandwidthEfficientAndEchoesFmtp(t *testing.T) {
	codecs := amrEnabledCodecs()
	// Disable G.711/G.722 so negotiation must pick AMR-WB.
	codecs.SetEnabled(g711.ALawSDPNameAndRate, false)
	codecs.SetEnabled(g711.ULawSDPNameAndRate, false)
	codecs.SetEnabled(g722.SDPNameAndRate, false)
	codecs.SetEnabled(dtmf.SDPNameAndRate, true)

	c1, _ := newUDPPipe()
	port, err := NewMediaPortWith(1, logger.GetLogger(), newTestCallMonitor(t), c1, &MediaOptions{
		IP:    newIP("1.1.1.1"),
		Ports: rtcconfig.PortRange{Start: 10000},
	}, 16000)
	require.NoError(t, err)
	t.Cleanup(port.Close)

	// PT 96 unsupported, PT 98 supported — answer must select 98 and echo fmtp.
	offer := []byte(`v=0
o=- 0 0 IN IP4 2.2.2.2
s=-
c=IN IP4 2.2.2.2
t=0 0
m=audio 20000 RTP/AVP 96 98 97
a=rtpmap:96 AMR-WB/16000
a=fmtp:96 octet-align=1;mode-change-capability=2
a=rtpmap:98 AMR-WB/16000
a=fmtp:98 mode-change-capability=2
a=rtpmap:97 telephone-event/8000
a=fmtp:97 0-15
`)
	answer, mc, err := port.SetOffer(offer, codecs, sdpsdk.EncryptionNone)
	require.NoError(t, err)
	require.NotNil(t, mc)
	require.Equal(t, amrwb.SDPNameAndRate, mc.Audio.Codec.Info().SDPName)
	require.Equal(t, byte(98), mc.Audio.Type)

	answerData, err := answer.SDP.Marshal()
	require.NoError(t, err)
	s := string(answerData)
	require.Contains(t, s, "a=rtpmap:98 AMR-WB/16000")
	require.Contains(t, s, "a=fmtp:98 mode-change-capability=2")
	require.NotContains(t, s, "rtpmap:96")
	// Ensure we did not invent octet-align=1 in the answer.
	require.False(t, strings.Contains(s, "octet-align=1"))
}
