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
	"strings"
	"testing"

	psdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"
)

const metaLikeOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 198.51.100.10\r\n" +
	"s=-\r\nt=0 0\r\n" +
	"a=fingerprint:sha-256 AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA\r\n" +
	"m=audio 40000 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 198.51.100.10\r\n" +
	"a=setup:actpass\r\na=rtcp-mux\r\na=rtpmap:111 opus/48000/2\r\n"

func TestParseMetaDTLSOffer(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	c, err := parseDTLSOffer([]byte(metaLikeOffer), cert)
	require.NoError(t, err)
	require.NotNil(t, c)
	require.False(t, c.isClient)
	require.Equal(t, "passive", c.localSetup)
}

func TestParseMetaDTLSOfferRequiresMuxAndFingerprint(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	_, err = parseDTLSOffer([]byte(strings.Replace(metaLikeOffer, "a=rtcp-mux\r\n", "", 1)), cert)
	require.ErrorIs(t, err, errDTLSSDP)
	_, err = parseDTLSOffer([]byte(strings.Replace(metaLikeOffer, "a=fingerprint:sha-256 AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA\r\n", "", 1)), cert)
	require.ErrorIs(t, err, errDTLSSDP)
}

func TestDTLSAnswerUsesSAVPF(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	conf, err := parseDTLSOffer([]byte(metaLikeOffer), cert)
	require.NoError(t, err)
	s := &psdp.SessionDescription{MediaDescriptions: []*psdp.MediaDescription{{MediaName: psdp.MediaName{Media: "audio", Port: psdp.RangedPort{Value: 10000}, Protos: []string{"RTP", "AVP"}, Formats: []string{"111"}}, Attributes: []psdp.Attribute{{Key: "rtpmap", Value: "111 opus/48000/2"}}}}}
	require.NoError(t, addDTLSAnswer(s, conf))
	raw, err := s.Marshal()
	require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "UDP/TLS/RTP/SAVPF")
	require.Contains(t, text, "a=setup:passive")
	require.Contains(t, text, "a=rtcp-mux")
	require.Contains(t, text, "a=fingerprint:sha-256 "+cert.fingerprint)
}

func TestParseMetaICEOfferAndAnswer(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	offer := strings.Replace(
		metaLikeOffer,
		"a=setup:actpass\r\n",
		"a=ice-lite\r\n"+
			"a=candidate:2 1 udp 2122262783 2001:db8::1 3480 typ host\r\n"+
			"a=candidate:1 1 udp 2122260223 198.51.100.10 3480 typ host\r\n"+
			"a=ice-ufrag:remote-user\r\n"+
			"a=ice-pwd:remote-password-value\r\n"+
			"a=setup:actpass\r\n",
		1,
	)
	conf, err := parseDTLSOffer([]byte(offer), cert)
	require.NoError(t, err)
	require.NotNil(t, conf.ice)
	require.Equal(t, "remote-user", conf.ice.remoteUfrag)
	require.Equal(t, "remote-password-value", conf.ice.remotePwd)
	require.Equal(t, "198.51.100.10:3480", conf.ice.remote.String())
	require.NotEmpty(t, conf.ice.localUfrag)
	require.NotEmpty(t, conf.ice.localPwd)

	var answer psdp.SessionDescription
	require.NoError(t, answer.Unmarshal([]byte(
		"v=0\r\n"+
			"o=- 1 1 IN IP4 203.0.113.20\r\n"+
			"s=-\r\n"+
			"c=IN IP4 203.0.113.20\r\n"+
			"t=0 0\r\n"+
			"m=audio 12000 RTP/AVP 111\r\n"+
			"a=rtpmap:111 opus/48000/2\r\n",
	)))
	require.NoError(t, addDTLSAnswer(&answer, conf))
	raw, err := answer.Marshal()
	require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "a=ice-ufrag:"+conf.ice.localUfrag)
	require.Contains(t, text, "a=ice-pwd:"+conf.ice.localPwd)
	require.Contains(t, text, "a=candidate:1 1 udp 2130706431 203.0.113.20 12000 typ host")
}

func TestParseMetaICEOfferRequiresIPv4Candidate(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	offer := strings.Replace(
		metaLikeOffer,
		"a=setup:actpass\r\n",
		"a=candidate:2 1 udp 2122262783 2001:db8::1 3480 typ host\r\n"+
			"a=ice-ufrag:remote-user\r\n"+
			"a=ice-pwd:remote-password-value\r\n"+
			"a=setup:actpass\r\n",
		1,
	)
	_, err = parseDTLSOffer([]byte(offer), cert)
	require.ErrorIs(t, err, errDTLSSDP)
}
