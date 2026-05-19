package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"

	"github.com/livekit/sip/pkg/stats"
)

func TestClassifyInviteError(t *testing.T) {
	sipStatusErr := func(code int) error {
		return fmt.Errorf("INVITE failed: %w", &livekit.SIPStatus{
			Code:   livekit.SIPStatusCode(code),
			Status: "test",
		})
	}

	cases := []struct {
		name       string
		err        error
		wantStatus CallStatus
		wantTerm   stats.Termination
		wantReason livekit.DisconnectReason
		wantReport bool // true if reportErr should be non-nil
	}{
		// Specific SIP status codes
		{"401 Unauthorized", sipStatusErr(401), callRejected, stats.ClientError("auth-required"), livekit.DisconnectReason_USER_REJECTED, false},
		{"403 Forbidden", sipStatusErr(403), callRejected, stats.ClientError("forbidden"), livekit.DisconnectReason_USER_REJECTED, false},
		{"404 Not Found", sipStatusErr(404), callUnavailable, stats.ClientError("not-found"), livekit.DisconnectReason_USER_UNAVAILABLE, false},
		{"407 Proxy Auth Required", sipStatusErr(407), callRejected, stats.ClientError("auth-required"), livekit.DisconnectReason_USER_REJECTED, false},
		{"408 Request Timeout", sipStatusErr(408), callUnavailable, stats.ClientError("request-timeout"), livekit.DisconnectReason_USER_UNAVAILABLE, false},
		{"480 Temporarily Unavailable", sipStatusErr(480), callUnavailable, stats.ClientError("unavailable"), livekit.DisconnectReason_USER_UNAVAILABLE, false},
		{"486 Busy Here", sipStatusErr(486), callRejected, stats.ClientError("busy"), livekit.DisconnectReason_USER_REJECTED, false},
		{"488 Not Acceptable Here", sipStatusErr(488), callRejected, stats.ClientError("not-acceptable"), livekit.DisconnectReason_USER_REJECTED, false},

		// 4xx catch-all
		{"410 Gone (4xx catch-all)", sipStatusErr(410), callRejected, stats.ClientError("client-error-410"), livekit.DisconnectReason_USER_UNAVAILABLE, false},
		{"487 Request Terminated (4xx catch-all)", sipStatusErr(487), callRejected, stats.ClientError("client-error-487"), livekit.DisconnectReason_USER_UNAVAILABLE, false},

		// 5xx upstream server error
		{"500 Internal Server Error", sipStatusErr(500), callDropped, stats.ServerError("upstream-server-error-500"), livekit.DisconnectReason_SIP_TRUNK_FAILURE, true},
		{"503 Service Unavailable", sipStatusErr(503), callDropped, stats.ServerError("upstream-server-error-503"), livekit.DisconnectReason_SIP_TRUNK_FAILURE, true},

		// 6xx global decline
		{"600 Global Busy Everywhere", sipStatusErr(600), callRejected, stats.ClientError("global-decline-600"), livekit.DisconnectReason_USER_REJECTED, false},
		{"603 Global Decline", sipStatusErr(603), callRejected, stats.ClientError("global-decline-603"), livekit.DisconnectReason_USER_REJECTED, false},

		// SDP errors
		{"SDP no common media", SDPError{Err: sdp.ErrNoCommonMedia}, callRejected, stats.ClientError("no-common-codec"), livekit.DisconnectReason_MEDIA_FAILURE, false},
		{"SDP no common crypto", SDPError{Err: sdp.ErrNoCommonCrypto}, callRejected, stats.ClientError("encryption-required"), livekit.DisconnectReason_MEDIA_FAILURE, false},
		{"SDP other", SDPError{Err: errors.New("bad sdp")}, callRejected, stats.ClientError("sdp-error"), livekit.DisconnectReason_MEDIA_FAILURE, false},

		// Sentinel-based errors
		{"SIP request timeout (no answer)", psrpc.NewError(psrpc.Canceled, ErrSIPRequestTimeout), callUnavailable, stats.ClientError("no-answer"), livekit.DisconnectReason_USER_UNAVAILABLE, false},
		{"Auth max retry", psrpc.NewError(psrpc.FailedPrecondition, ErrAuthMaxRetry), callRejected, stats.ClientError("auth-failed"), livekit.DisconnectReason_USER_REJECTED, true},
		{"Auth missing creds", psrpc.NewError(psrpc.FailedPrecondition, ErrAuthMissingCreds), callRejected, stats.ClientError("auth-failed"), livekit.DisconnectReason_USER_REJECTED, true},
		{"Auth no header", psrpc.NewError(psrpc.FailedPrecondition, ErrAuthNoHeader), callRejected, stats.ClientError("auth-failed"), livekit.DisconnectReason_USER_REJECTED, true},

		// Context cancellation / deadline
		{"context.DeadlineExceeded", context.DeadlineExceeded, callDropped, stats.ServerError("deadline-exceeded"), livekit.DisconnectReason_UNKNOWN_REASON, true},
		{"context.Canceled", context.Canceled, callRejected, stats.ClientError("canceled"), livekit.DisconnectReason_USER_UNAVAILABLE, false},

		// Net errors
		{"DNS no such host", &net.DNSError{Err: "no such host", Name: "voip.example.com", IsNotFound: true}, callDropped, stats.ClientError("dns-resolution"), livekit.DisconnectReason_SIP_TRUNK_FAILURE, true},
		{"AddrError missing port", &net.AddrError{Err: "missing port", Addr: "voip.example.com"}, callDropped, stats.ClientError("address-error"), livekit.DisconnectReason_SIP_TRUNK_FAILURE, true},
		{"OpError dial refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, callDropped, stats.ServerError("network-error"), livekit.DisconnectReason_SIP_TRUNK_FAILURE, true},

		// Unknown — conservative default
		{"Unknown error", errors.New("something broke"), callDropped, stats.ServerError("invite-failed"), livekit.DisconnectReason_UNKNOWN_REASON, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyInviteError(tc.err)
			require.Equal(t, tc.wantStatus, res.status, "status")
			require.Equal(t, tc.wantTerm, res.term, "termination")
			require.Equal(t, tc.wantReason, res.reason, "disconnect reason")
			if tc.wantReport {
				require.NotNil(t, res.reportErr, "reportErr expected non-nil")
			} else {
				require.Nil(t, res.reportErr, "reportErr expected nil")
			}
		})
	}
}

func TestClassifyInviteError_SDPReplacesReturnErr(t *testing.T) {
	// SDP path rewraps the error as psrpc FailedPrecondition; confirm
	// returnErr differs from the input.
	in := SDPError{Err: sdp.ErrNoCommonMedia}
	res := classifyInviteError(in)
	require.NotEqual(t, error(in), res.returnErr)
	require.True(t, errors.Is(res.returnErr, sdp.ErrNoCommonMedia))
}

func TestClassifyInviteError_ReturnErrWrap(t *testing.T) {
	// Refined buckets that wrap returnErr with a specific psrpc code so the
	// RPC boundary reflects the originating failure mode instead of an
	// auto-wrapped Internal/500.
	cases := []struct {
		name     string
		err      error
		wantCode psrpc.ErrorCode
	}{
		{"context.DeadlineExceeded", context.DeadlineExceeded, psrpc.DeadlineExceeded},
		{"context.Canceled", context.Canceled, psrpc.Canceled},
		{"*net.DNSError", &net.DNSError{Err: "no such host", Name: "voip.example.com", IsNotFound: true}, psrpc.InvalidArgument},
		{"*net.AddrError", &net.AddrError{Err: "missing port", Addr: "voip.example.com"}, psrpc.InvalidArgument},
		{"*net.OpError", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, psrpc.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyInviteError(tc.err)
			var psErr psrpc.Error
			require.True(t, errors.As(res.returnErr, &psErr), "returnErr should be a psrpc.Error")
			require.Equal(t, tc.wantCode, psErr.Code())
		})
	}
}
