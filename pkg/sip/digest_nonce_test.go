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
	crand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/icholy/digest"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/sdp"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/sealer"
	"github.com/livekit/sip/pkg/stats"
)

func newTestSealer(t testing.TB, keys ...string) *sealer.AESGCM {
	t.Helper()
	s, err := sealer.NewAESGCM(keys)
	require.NoError(t, err)
	return s
}

// newTestNonces returns a digest nonce sealer with a random key, as the open source build uses.
func newTestNonces(t testing.TB) sealer.Sealer {
	return newTestSealer(t, crand.Text())
}

func TestDigestNonce(t *testing.T) {
	now := time.Now()
	issuer := newTestSealer(t, "k")
	nonce, err := issueDigestNonce(issuer, "call-1", now)
	require.NoError(t, err)
	require.NotContains(t, nonce, `"`)
	require.NotContains(t, nonce, `\`)

	// Same key accepts it, with clock jitter
	other := newTestSealer(t, "k")
	require.NoError(t, checkDigestNonce(other, nonce, "call-1", now))
	require.NoError(t, checkDigestNonce(other, nonce, "call-1", now.Add(digestNonceTTL-time.Millisecond)))
	require.NoError(t, checkDigestNonce(other, nonce, "call-1", now.Add(-digestNonceTTL+time.Millisecond)))

	// Nonces are not reused, even for the same call at the same time.
	again, err := issueDigestNonce(issuer, "call-1", now)
	require.NoError(t, err)
	require.NotEqual(t, nonce, again)

	ts := binary.BigEndian.AppendUint64(nil, uint64(now.UnixMicro()))
	purposeless, err := issuer.Seal(nil, ts, []byte("call-1")) // right key and payload, no purpose
	require.NoError(t, err)

	cases := []struct {
		name  string
		s     sealer.Sealer
		nonce string
		call  string
		now   time.Time
		err   error
	}{
		{"other call", other, nonce, "call-2", now, sealer.ErrInvalid},
		{"other key", newTestSealer(t, "other"), nonce, "call-1", now, sealer.ErrInvalid},
		{"expired", other, nonce, "call-1", now.Add(digestNonceTTL + time.Second), errDigestNonceExpired},
		{"from the future", other, nonce, "call-1", now.Add(-digestNonceTTL - time.Second), errDigestNonceExpired},
		{"tampered", other, strings.ToUpper(nonce), "call-1", now, sealer.ErrInvalid},
		{"not base64url", other, nonce + "!", "call-1", now, sealer.ErrInvalid},
		{"empty", other, "", "call-1", now, sealer.ErrInvalid},
		{"no purpose", other, base64.RawURLEncoding.EncodeToString(purposeless), "call-1", now, sealer.ErrInvalid},
		{"old format", other, fmt.Sprintf("%d-%s", now.UnixMicro(), "call-1"), "call-1", now, sealer.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.ErrorIs(t, checkDigestNonce(c.s, c.nonce, c.call, c.now), c.err)
		})
	}
}

func TestDigestNonceWrongPayload(t *testing.T) {
	s := newTestSealer(t, "k")
	b, err := s.Seal(nil, []byte("short"), digestNonceAAD("call-1"))
	require.NoError(t, err)
	err = checkDigestNonce(s, base64.RawURLEncoding.EncodeToString(b), "call-1", time.Now())
	require.ErrorIs(t, err, sealer.ErrInvalid)
}

func TestNewCallManagerRequiresNonces(t *testing.T) {
	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)
	_, err = NewCallManager("", &config.Config{}, logger.NewTestLogger(t), mon, nil, nil)
	require.Error(t, err)
}

// startAuthService starts a service on sipPort that requires digest auth, sealing nonces with nonces.
func startAuthService(t *testing.T, sipPort int, nonces sealer.Sealer, username, password string) string {
	h := &TestHandler{
		GetAuthCredentialsFunc: func(ctx context.Context, call *rpc.SIPCall) (AuthInfo, error) {
			return AuthInfo{
				Result: AuthPassword,
				Auth:   InboundAuth{Username: username, Password: password},
			}, nil
		},
		DispatchCallFunc: func(ctx context.Context, info *CallInfo) CallDispatch {
			return CallDispatch{Result: DispatchNoRuleReject}
		},
	}
	localIP, err := config.GetLocalIP()
	require.NoError(t, err)
	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9})
	require.NoError(t, err)
	conf := &config.Config{
		SIPPort:       sipPort,
		SIPPortListen: sipPort,
		RTPPort:       rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
	}
	log := logger.NewTestLogger(t)
	manager, err := NewCallManager("", conf, log, mon, nonces, func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) StateHandler {
		return NewRPCStateHandler(nil)
	})
	require.NoError(t, err)
	s, err := NewService(conf, mon, log, manager)
	require.NoError(t, err)
	t.Cleanup(s.Stop)
	s.SetHandler(h)
	require.NoError(t, s.Start())
	return fmt.Sprintf("%s:%d", localIP, sipPort)
}

// TestDigestAuthAcrossServers sends the challenged INVITE to one server and the authenticated
// retry to another, as a load balancer may.
func TestDigestAuthAcrossServers(t *testing.T) {
	const (
		username = "user"
		password = "pass"
	)
	// The servers run at once, so each needs its own port.
	challenger := startAuthService(t, testPortSIPMin, newTestSealer(t, "shared"), username, password)
	peer := startAuthService(t, testPortSIPMin+1, newTestSealer(t, "shared"), username, password)
	stranger := startAuthService(t, testPortSIPMin+2, newTestSealer(t, "other"), username, password)

	localIP, err := config.GetLocalIP()
	require.NoError(t, err)
	ua, err := sipgo.NewUA(sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(logger.NewTestLogger(t)))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	offer, err := sdp.NewOfferWith(defaultCodecs, localIP, 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerData, err := offer.SDP.Marshal()
	require.NoError(t, err)

	invite := func(addr, callID, auth string) *sip.Response {
		req := sip.NewRequest(sip.INVITE, sip.Uri{User: "agent", Host: addr})
		req.SetDestination(addr)
		req.SetBody(offerData)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.AppendHeader(sip.NewHeader("Call-ID", callID))
		if auth != "" {
			req.AppendHeader(sip.NewHeader("Proxy-Authorization", auth))
		}
		tx, err := client.TransactionRequest(req)
		require.NoError(t, err)
		t.Cleanup(tx.Terminate)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		return getFinalResponseOrFail(t, ctx, tx)
	}
	answer := func(callID, retryAddr string) *sip.Response {
		res := invite(challenger, callID, "")
		require.Equal(t, sip.StatusCode(407), res.StatusCode)
		chal, err := digest.ParseChallenge(res.GetHeader("Proxy-Authenticate").Value())
		require.NoError(t, err)
		cred, err := digest.Digest(chal, digest.Options{
			Method:   "INVITE",
			URI:      "sip:agent@" + retryAddr,
			Username: username,
			Password: password,
		})
		require.NoError(t, err)
		return invite(retryAddr, callID, cred.String())
	}

	// Authenticated, so the call proceeds to dispatch, which rejects it with no rule.
	res := answer("cross-server@test", peer)
	require.NotEqual(t, sip.StatusCode(401), res.StatusCode)
	require.NotEqual(t, sip.StatusCode(407), res.StatusCode)

	// A server with another key can't verify the challenge.
	res = answer("other-key@test", stranger)
	require.Equal(t, sip.StatusCode(401), res.StatusCode)
}
