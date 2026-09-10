// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	pice "github.com/pion/ice/v4"
	psdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/g711"
	mediasdp "github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/logger"
)

func dtlsICEOffer(fingerprint, ufrag, pwd string) []byte {
	return []byte(fmt.Sprintf("v=0\r\n"+
		"o=- 1 1 IN IP4 127.0.0.1\r\n"+
		"s=-\r\nt=0 0\r\n"+
		"m=audio 40000 UDP/TLS/RTP/SAVPF 0\r\n"+
		"c=IN IP4 127.0.0.1\r\n"+
		"a=candidate:remote 1 udp 2130706431 127.0.0.1 40000 typ host\r\n"+
		"a=ice-ufrag:%s\r\na=ice-pwd:%s\r\n"+
		"a=fingerprint:sha-256 %s\r\n"+
		"a=setup:actpass\r\na=rtcp-mux\r\n"+
		"a=rtpmap:0 PCMU/8000\r\na=sendrecv\r\n", ufrag, pwd, fingerprint))
}

func answerICECredentials(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	var answer psdp.SessionDescription
	require.NoError(t, answer.Unmarshal(raw))
	require.NotEmpty(t, answer.MediaDescriptions)
	ufrag, ok := mediaAttribute(answer.MediaDescriptions[0], "ice-ufrag")
	require.True(t, ok)
	pwd, ok := mediaAttribute(answer.MediaDescriptions[0], "ice-pwd")
	require.True(t, ok)
	return ufrag, pwd
}

func newDTLSTestMediaPort(t *testing.T, timeout time.Duration) *mediaPort {
	t.Helper()
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	return newTestPort(t, logger.NewTestLogger(t), newTestConn(1), &MediaOptions{
		IP:                   newIP("127.0.0.1"),
		Codecs:               testCodecSet(g711.ULawSDPNameAndRate),
		DTLSEnabled:          true,
		DTLSCertificate:      cert,
		DTLSHandshakeTimeout: timeout,
	}, RoomSampleRate)
}

func TestDTLSRefreshReusesActiveICETransport(t *testing.T) {
	m := newDTLSTestMediaPort(t, 30*time.Second)
	offer := dtlsICEOffer(m.opts.DTLSCertificate.fingerprint, "remote-user", "remote-password-value")

	first, err := m.GenerateAnswer(offer)
	require.NoError(t, err)
	firstPipeline := m.pipeline
	firstUfrag, firstPwd := answerICECredentials(t, first)

	second, err := m.GenerateAnswer(offer)
	require.NoError(t, err)
	secondUfrag, secondPwd := answerICECredentials(t, second)

	require.Same(t, firstPipeline, m.pipeline, "session refresh must keep the active media transport")
	require.Equal(t, firstUfrag, secondUfrag, "answer must not advertise unused ICE credentials")
	require.Equal(t, firstPwd, secondPwd, "answer must not advertise unused ICE credentials")
}

func TestDTLSICERestartRebuildsTransport(t *testing.T) {
	m := newDTLSTestMediaPort(t, 30*time.Second)
	first, err := m.GenerateAnswer(dtlsICEOffer(m.opts.DTLSCertificate.fingerprint, "remote-user-one", "remote-password-value-one"))
	require.NoError(t, err)
	firstPipeline := m.pipeline
	firstUfrag, firstPwd := answerICECredentials(t, first)

	second, err := m.GenerateAnswer(dtlsICEOffer(m.opts.DTLSCertificate.fingerprint, "remote-user-two", "remote-password-value-two"))
	require.NoError(t, err)
	secondUfrag, secondPwd := answerICECredentials(t, second)

	require.NotSame(t, firstPipeline, m.pipeline, "ICE restart must rebuild the media transport")
	require.NotEqual(t, firstUfrag, secondUfrag)
	require.NotEqual(t, firstPwd, secondPwd)
}

func TestDTLSSessionImmediateCloseCancelsStartup(t *testing.T) {
	m := newDTLSTestMediaPort(t, time.Hour)
	offer := dtlsICEOffer(m.opts.DTLSCertificate.fingerprint, "remote-user", "remote-password-value")
	_, err := m.GenerateAnswer(offer)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("immediate close waited for DTLS handshake timeout")
	}
}

func TestDTLSSessionConcurrentCloseDuringStartup(t *testing.T) {
	for range 20 {
		m := newDTLSTestMediaPort(t, time.Hour)
		offer := dtlsICEOffer(m.opts.DTLSCertificate.fingerprint, "remote-user", "remote-password-value")
		_, err := m.GenerateAnswer(offer)
		require.NoError(t, err)

		var wg sync.WaitGroup
		for range 4 {
			wg.Go(m.Close)
		}
		closed := make(chan struct{})
		go func() {
			wg.Wait()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("concurrent close blocked during DTLS/ICE startup")
		}
	}
}

func TestLegacyMediaDoesNotUseDTLSState(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	newPort := func(encryption mediasdp.Encryption) *mediaPort {
		return newTestPort(t, logger.NewTestLogger(t), newTestConn(1), &MediaOptions{
			IP:              newIP("127.0.0.1"),
			Codecs:          testCodecSet(g711.ULawSDPNameAndRate),
			Encryption:      encryption,
			DTLSEnabled:     true,
			DTLSCertificate: cert,
		}, RoomSampleRate)
	}

	t.Run("RTP AVP", func(t *testing.T) {
		m := newPort(mediasdp.EncryptionNone)
		_, err := m.GenerateAnswer(offerAt(t, netip.MustParseAddrPort("127.0.0.1:40000")))
		require.NoError(t, err)
		require.Nil(t, m.dtls)
		_, isDTLS := m.pipeline.sess.(*dtlsSrtpSession)
		require.False(t, isDTLS)
	})

	t.Run("SDES SRTP", func(t *testing.T) {
		m := newPort(mediasdp.EncryptionRequire)
		_, err := m.GenerateAnswer(offerAtEnc(t, netip.MustParseAddrPort("127.0.0.1:40002"), mediasdp.EncryptionRequire))
		require.NoError(t, err)
		require.Nil(t, m.dtls)
		_, isDTLS := m.pipeline.sess.(*dtlsSrtpSession)
		require.False(t, isDTLS)
	})
}

func gatherICECandidates(t *testing.T, agent *pice.Agent) []pice.Candidate {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	require.NoError(t, agent.OnCandidate(func(candidate pice.Candidate) {
		if candidate == nil {
			once.Do(func() { close(done) })
		}
	}))
	require.NoError(t, agent.GatherCandidates())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ICE candidate gathering timed out")
	}
	candidates, err := agent.GetLocalCandidates()
	require.NoError(t, err)
	require.NotEmpty(t, candidates)
	return candidates
}

func TestICEChecksFallBackToLaterCandidate(t *testing.T) {
	newAgent := func(lite bool) *pice.Agent {
		agent, err := pice.NewAgent(&pice.AgentConfig{
			NetworkTypes:    []pice.NetworkType{pice.NetworkTypeUDP4},
			CandidateTypes:  []pice.CandidateType{pice.CandidateTypeHost},
			IncludeLoopback: true,
			Lite:            lite,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = agent.Close() })
		return agent
	}

	controlling := newAgent(false)
	controlled := newAgent(true)
	controllingCandidates := gatherICECandidates(t, controlling)
	controlledCandidates := gatherICECandidates(t, controlled)

	for _, candidate := range controllingCandidates {
		copyCandidate, err := pice.UnmarshalCandidate(candidate.Marshal())
		require.NoError(t, err)
		require.NoError(t, controlled.AddRemoteCandidate(copyCandidate))
	}

	remoteCandidates := []dtlsICECandidate{{raw: "unreachable 1 udp 4294967295 192.0.2.1 9 typ host"}}
	for _, candidate := range controlledCandidates {
		remoteCandidates = append(remoteCandidates, dtlsICECandidate{raw: candidate.Marshal()})
	}
	addCtx, addCancel := context.WithTimeout(context.Background(), time.Second)
	defer addCancel()
	require.NoError(t, addRemoteICECandidates(addCtx, controlling, remoteCandidates))
	got, err := controlling.GetRemoteCandidates()
	require.NoError(t, err)
	require.Len(t, got, len(remoteCandidates))

	controllingUfrag, controllingPwd, err := controlling.GetLocalUserCredentials()
	require.NoError(t, err)
	controlledUfrag, controlledPwd, err := controlled.GetLocalUserCredentials()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acceptResult := make(chan error, 1)
	go func() {
		conn, acceptErr := controlled.Accept(ctx, controllingUfrag, controllingPwd)
		if acceptErr == nil {
			acceptErr = conn.Close()
		}
		acceptResult <- acceptErr
	}()
	conn, err := controlling.Dial(ctx, controlledUfrag, controlledPwd)
	require.NoError(t, err, "ICE should connect through the later usable candidate")
	require.NoError(t, conn.Close())
	require.NoError(t, <-acceptResult)
}
