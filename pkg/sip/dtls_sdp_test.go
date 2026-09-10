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

	pice "github.com/pion/ice/v4"
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
	require.Len(t, conf.ice.remoteCandidates, 1)
	require.Equal(t, "198.51.100.10:3480", conf.ice.remoteCandidates[0].address.String())
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

func TestParseAllSupportedICECandidates(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	offer := strings.Replace(metaLikeOffer, "a=setup:actpass\r\n",
		"a=candidate:dead 1 udp 2130706431 192.0.2.1 9 typ host generation 0\r\n"+
			"a=candidate:live 1 udp 1694498815 198.51.100.10 3480 typ srflx raddr 10.0.0.1 rport 5000 generation 0\r\n"+
			"a=candidate:rtcp 2 udp 2130706430 198.51.100.10 3481 typ host\r\n"+
			"a=candidate:v6 1 udp 2130706431 2001:db8::1 3480 typ host\r\n"+
			"a=ice-ufrag:remote-user\r\na=ice-pwd:remote-password-value\r\na=setup:actpass\r\n", 1)

	conf, err := parseDTLSOffer([]byte(offer), cert)
	require.NoError(t, err)
	require.Len(t, conf.ice.remoteCandidates, 2)
	require.Equal(t, "dead", conf.ice.remoteCandidates[0].foundation)
	require.Equal(t, uint32(2130706431), conf.ice.remoteCandidates[0].priority)
	require.Equal(t, pice.CandidateTypeServerReflexive, conf.ice.remoteCandidates[1].typ)
	require.Equal(t, "10.0.0.1", conf.ice.remoteCandidates[1].related.Address)
	require.Equal(t, 5000, conf.ice.remoteCandidates[1].related.Port)
	require.Contains(t, conf.ice.remoteCandidates[1].raw, "generation 0")
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

func TestDTLSRemoteTransportIdentity(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	baseOffer := strings.Replace(metaLikeOffer, "a=setup:actpass\r\n",
		"a=candidate:one 1 udp 2130706431 198.51.100.10 3480 typ host\r\n"+
			"a=ice-ufrag:remote-user\r\na=ice-pwd:remote-password-value\r\na=setup:actpass\r\n", 1)
	base, err := parseDTLSOffer([]byte(baseOffer), cert)
	require.NoError(t, err)

	tests := map[string]string{
		"fingerprint": strings.Replace(baseOffer, "AA:AA:AA", "BB:AA:AA", 1),
		"setup role":  strings.Replace(baseOffer, "a=setup:actpass", "a=setup:passive", 1),
		"ice ufrag":   strings.Replace(baseOffer, "remote-user", "changed-user", 1),
		"ice pwd":     strings.Replace(baseOffer, "remote-password-value", "changed-password-value", 1),
		"candidate":   strings.Replace(baseOffer, "198.51.100.10 3480", "198.51.100.11 3481", 1),
	}
	for name, offer := range tests {
		t.Run(name, func(t *testing.T) {
			changed, err := parseDTLSOffer([]byte(offer), cert)
			require.NoError(t, err)
			require.False(t, sameDTLSRemoteTransport(base, changed))
		})
	}
}
