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

func testCall() *rpc.SIPCall {
	return &rpc.SIPCall{
		LkCallId: "call",
		From:     &livekit.SIPUri{User: "from"},
		To:       &livekit.SIPUri{User: "to"},
	}
}

func TestGetAuthCredentials(t *testing.T) {
	call := testCall()
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

}

func TestGetAuthCredentials_ServiceErrorPassesThrough(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"no response", psrpc.ErrNoResponse},
		{"timeout", psrpc.ErrRequestTimedOut},
		{"canceled", psrpc.ErrRequestCanceled},
		{"internal", psrpc.NewErrorf(psrpc.Internal, "db down")},
		{"uncoded", errors.New("plain")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := GetAuthCredentials(context.Background(), fakeIOClient{err: c.err}, testCall())
			require.ErrorIs(t, err, c.err)
		})
	}
}

func TestGetAuthCredentials_RejectionErrorBecomesResult(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"failed precondition", psrpc.NewErrorf(psrpc.FailedPrecondition, "multiple trunks")},
		{"invalid argument", psrpc.NewErrorf(psrpc.InvalidArgument, "bad source ip")},
		{"permission denied", psrpc.NewErrorf(psrpc.PermissionDenied, "anycast not allowed")},
		{"not found", psrpc.NewErrorf(psrpc.NotFound, "no trunk")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := GetAuthCredentials(context.Background(), fakeIOClient{err: c.err}, testCall())
			require.NoError(t, err)
			require.Equal(t, sip.AuthRejectedAsError, r.Result)
		})
	}
}
