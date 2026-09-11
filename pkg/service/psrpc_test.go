package service

import (
	"context"
	"errors"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/sip"
)

type fakeIOClient struct {
	rpc.IOInfoSIPClient
	resp *rpc.GetSIPTrunkAuthenticationResponse
	err  error
}

func (c fakeIOClient) GetSIPTrunkAuthentication(context.Context, *rpc.GetSIPTrunkAuthenticationRequest, ...psrpc.RequestOption) (*rpc.GetSIPTrunkAuthenticationResponse, error) {
	return c.resp, c.err
}

func TestGetAuthCredentials(t *testing.T) {
	call := &rpc.SIPCall{
		LkCallId: "call",
		From:     &livekit.SIPUri{User: "from"},
		To:       &livekit.SIPUri{User: "to"},
	}
	cases := []struct {
		name string
		resp *rpc.GetSIPTrunkAuthenticationResponse
		want sip.AuthResult
	}{
		{"quota", &rpc.GetSIPTrunkAuthenticationResponse{ErrorCode: rpc.SIPTrunkAuthenticationError_SIP_TRUNK_AUTH_ERROR_QUOTA_EXCEEDED}, sip.AuthQuotaExceeded},
		{"no trunk", &rpc.GetSIPTrunkAuthenticationResponse{ErrorCode: rpc.SIPTrunkAuthenticationError_SIP_TRUNK_AUTH_ERROR_NO_TRUNK_FOUND}, sip.AuthNoTrunkFound},
		{"unknown code", &rpc.GetSIPTrunkAuthenticationResponse{ErrorCode: rpc.SIPTrunkAuthenticationError(99)}, sip.AuthFailureOther},
		{"accept", &rpc.GetSIPTrunkAuthenticationResponse{SipTrunkId: "T"}, sip.AuthAccept},
		{"drop", &rpc.GetSIPTrunkAuthenticationResponse{Drop: true}, sip.AuthDrop},
		{"password", &rpc.GetSIPTrunkAuthenticationResponse{SipTrunkId: "T", Username: "u", Password: "p", Realm: "r"}, sip.AuthPassword},
		{"username only", &rpc.GetSIPTrunkAuthenticationResponse{SipTrunkId: "T", Username: "u"}, sip.AuthAccept},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := GetAuthCredentials(context.Background(), fakeIOClient{resp: c.resp}, call)
			require.NoError(t, err)
			require.Equal(t, c.want, r.Result)
			if c.want == sip.AuthPassword {
				require.Equal(t, sip.InboundAuth{Username: "u", Password: "p", Realm: "r"}, r.Auth)
			}
		})
	}

	t.Run("transport error", func(t *testing.T) {
		_, err := GetAuthCredentials(context.Background(), fakeIOClient{err: psrpc.ErrNoResponse}, call)
		require.True(t, errors.Is(err, psrpc.ErrNoResponse))
	})
}
