package sip

import (
	"fmt"
	"net"

	"github.com/pkg/errors"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

type SDPError struct {
	Err error
}

func (e SDPError) Error() string {
	return e.Err.Error()
}

func (e SDPError) Unwrap() error {
	return e.Err
}

// inviteErrorClassification captures everything connectSIP needs after a
// dialSIP failure: how to record the call, how to bucket the SLI, and which
// error (if any) to surface back to the caller and the call record.
type inviteErrorClassification struct {
	status    CallStatus
	term      stats.Termination
	reason    livekit.DisconnectReason
	reportErr error // passed to c.close; nil means don't write to SIPCallInfo.Error
	returnErr error // returned from connectSIP; defaults to the original err
}

// classifyInviteError buckets an outbound dialSIP error. The default is the
// conservative ServerError("invite-failed") so that genuinely unknown failure
// modes still get flagged. Known buckets:
//
//   - SIP status response from the customer's SIP trunk: 4xx → client_error,
//     5xx → server_error with a distinct upstream label, 6xx → client_error.
//     A handful of common codes get more specific reason labels.
//   - SDP negotiation failures → client_error subclasses.
//   - Ringing-timeout / context cancel (ErrSIPRequestTimeout) → client_error
//     "no-answer" since the callee never picked up.
//   - DNS resolution failures → client_error "dns-resolution" (caller's
//     trunk URI is wrong).
//   - Auth failures (missing creds, max-retry, missing header) → client_error
//     "auth-failed" (caller's trunk auth config is wrong).
func classifyInviteError(err error) inviteErrorClassification {
	res := inviteErrorClassification{
		status:    callDropped,
		term:      stats.ServerError("invite-failed"),
		reason:    livekit.DisconnectReason_UNKNOWN_REASON,
		reportErr: err,
		returnErr: err,
	}

	var sipStatus *livekit.SIPStatus
	if errors.As(err, &sipStatus) {
		code := int(sipStatus.Code)
		switch code {
		case int(sip.StatusUnauthorized), int(sip.StatusProxyAuthRequired):
			res.status, res.term, res.reason = callRejected, stats.ClientError("auth-required"), livekit.DisconnectReason_USER_REJECTED
			res.reportErr = nil
		case int(sip.StatusForbidden):
			res.status, res.term, res.reason = callRejected, stats.ClientError("forbidden"), livekit.DisconnectReason_USER_REJECTED
			res.reportErr = nil
		case int(sip.StatusNotFound):
			res.status, res.term, res.reason = callUnavailable, stats.ClientError("not-found"), livekit.DisconnectReason_USER_UNAVAILABLE
			res.reportErr = nil
		case int(sip.StatusRequestTimeout):
			res.status, res.term, res.reason = callUnavailable, stats.ClientError("request-timeout"), livekit.DisconnectReason_USER_UNAVAILABLE
			res.reportErr = nil
		case int(sip.StatusTemporarilyUnavailable):
			res.status, res.term, res.reason = callUnavailable, stats.ClientError("unavailable"), livekit.DisconnectReason_USER_UNAVAILABLE
			res.reportErr = nil
		case int(sip.StatusBusyHere):
			res.status, res.term, res.reason = callRejected, stats.ClientError("busy"), livekit.DisconnectReason_USER_REJECTED
			res.reportErr = nil
		case int(sip.StatusNotAcceptableHere):
			res.status, res.term, res.reason = callRejected, stats.ClientError("not-acceptable"), livekit.DisconnectReason_USER_REJECTED
			res.reportErr = nil
		default:
			switch {
			case code >= 400 && code < 500:
				res.status, res.term, res.reason = callRejected, stats.ClientError(fmt.Sprintf("client-error-%d", code)), livekit.DisconnectReason_USER_UNAVAILABLE
				res.reportErr = nil
			case code >= 500 && code < 600:
				res.status, res.term, res.reason = callDropped, stats.ServerError(fmt.Sprintf("upstream-server-error-%d", code)), livekit.DisconnectReason_SIP_TRUNK_FAILURE
				// keep reportErr so 5xx detail is recorded
			case code >= 600 && code < 700:
				res.status, res.term, res.reason = callRejected, stats.ClientError(fmt.Sprintf("global-decline-%d", code)), livekit.DisconnectReason_USER_REJECTED
				res.reportErr = nil
			}
		}
		return res
	}

	var sdpErr SDPError
	if errors.As(err, &sdpErr) {
		res.status, res.reason = callRejected, livekit.DisconnectReason_MEDIA_FAILURE
		res.reportErr = nil
		res.returnErr = psrpc.NewError(psrpc.FailedPrecondition, sdpErr.Err)
		switch {
		case errors.Is(sdpErr.Err, sdp.ErrNoCommonMedia):
			res.term = stats.ClientError("no-common-codec")
		case errors.Is(sdpErr.Err, sdp.ErrNoCommonCrypto):
			res.term = stats.ClientError("encryption-required")
		default:
			res.term = stats.ClientError("sdp-error")
		}
		return res
	}

	if errors.Is(err, ErrSIPRequestTimeout) {
		res.status, res.term, res.reason = callUnavailable, stats.ClientError("no-answer"), livekit.DisconnectReason_USER_UNAVAILABLE
		res.reportErr = nil
		return res
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		res.status, res.term, res.reason = callDropped, stats.ClientError("dns-resolution"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		// keep reportErr so the DNS detail is recorded in SIPCallInfo;
		// wrap returnErr so callers across the RPC boundary see a 400
		// (InvalidArgument — the trunk URI is bad) instead of an
		// auto-wrapped Internal/500.
		res.returnErr = psrpc.NewError(psrpc.InvalidArgument, err)
		return res
	}

	if errors.Is(err, ErrAuthMaxRetry) || errors.Is(err, ErrAuthMissingCreds) || errors.Is(err, ErrAuthNoHeader) {
		res.status, res.term, res.reason = callRejected, stats.ClientError("auth-failed"), livekit.DisconnectReason_USER_REJECTED
		// keep reportErr so the auth detail is recorded
		return res
	}

	return res
}
