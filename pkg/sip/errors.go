package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

// inviteFailure is the verdict for a failed outbound INVITE: how to record
// the call, how to bucket the SLI, and which error to surface back.
type inviteFailure struct {
	status    CallStatus
	term      stats.Termination
	reason    livekit.DisconnectReason
	reportErr error // nil skips writing SIPCallInfo.Error
	returnErr error
}

// inviteClassifier lets an error describe its own verdict instead of
// classifyInviteError carrying a switch over every variant. Source sites
// wrap raw errors in these types so the classification lives next to where
// the failure happened.
type inviteClassifier interface {
	error
	ClassifyInvite() inviteFailure
}

type SDPError struct {
	Err error
}

var _ inviteClassifier = SDPError{}

func (e SDPError) Error() string { return e.Err.Error() }
func (e SDPError) Unwrap() error { return e.Err }

// GRPCStatus lets psrpc.GetErrorCode (and any gRPC-aware caller) extract the
// failed-precondition code without manual psrpc.NewError wrapping.
func (e SDPError) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Err.Error())
}

func (e SDPError) ClassifyInvite() inviteFailure {
	res := inviteFailure{
		status:    callRejected,
		reason:    livekit.DisconnectReason_MEDIA_FAILURE,
		reportErr: e.Err,
		returnErr: e,
	}
	switch {
	case errors.Is(e.Err, sdp.ErrNoCommonMedia):
		res.term = stats.ClientError("no-common-codec")
	case errors.Is(e.Err, sdp.ErrNoCommonCrypto):
		res.term = stats.ClientError("encryption-required")
	default:
		res.term = stats.ClientError("sdp-error")
	}
	return res
}

// transactionTimeoutError is an INVITE transaction that terminated without a
// final response. responses = 1xx provisionals seen first: 0 = upstream went
// silent (Timer B), >0 = answered but never completed.
type transactionTimeoutError struct {
	responses int
}

var _ inviteClassifier = transactionTimeoutError{}

func (e transactionTimeoutError) Error() string {
	return fmt.Sprintf("transaction failed to complete (%d intermediate responses)", e.responses)
}

func (e transactionTimeoutError) ClassifyInvite() inviteFailure {
	reason := "upstream-no-response"
	if e.responses > 0 {
		reason = "no-final-response"
	}
	return inviteFailure{
		status:    callUnavailable,
		term:      stats.ClientError(reason),
		reason:    livekit.DisconnectReason_SIP_TRUNK_FAILURE,
		reportErr: e, // keep so the customer sees their destination didn't complete
		returnErr: psrpc.NewError(psrpc.Canceled, e),
	}
}

// classifyInviteError buckets an outbound INVITE error. Self-classifying
// errors describe themselves; the residual switch covers external types we
// can't extend (SIPStatus, net.*, context.*) and falls back to
// ServerError("invite-failed") so unknown modes still flag the SLI.
func classifyInviteError(err error) inviteFailure {
	if ic, ok := errors.AsType[inviteClassifier](err); ok {
		return ic.ClassifyInvite()
	}

	res := inviteFailure{
		status:    callDropped,
		term:      stats.ServerError("invite-failed"),
		reason:    livekit.DisconnectReason_UNKNOWN_REASON,
		reportErr: err,
		returnErr: err,
	}

	if sipStatus, ok := errors.AsType[*livekit.SIPStatus](err); ok {
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
				// Some upstreams (notably Twilio) return a 5xx when the customer's own trunk exceeds its configured CPS or
				// concurrent-call cap. That's a customer-side rate limit, not upstream infrastructure breakage, so it must not count
				// against the server-error SLI. Match on the response body — brittle, so kept narrow to the known phrases.
				body := strings.ToLower(sipStatus.GetStatus())
				switch {
				case strings.Contains(body, "cps limit exceeded"):
					res.status, res.term, res.reason = callRejected, stats.ClientError("cps-limit-exceeded"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
					// keep reportErr so the customer can see they hit their cap
				case strings.Contains(body, "concurrent call limit exceeded"):
					res.status, res.term, res.reason = callRejected, stats.ClientError("concurrent-limit-exceeded"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
					// keep reportErr so the customer can see they hit their cap
				default:
					// Carrier-side 5xx; keep reportErr for the detail.
					res.status, res.term, res.reason = callDropped, stats.UpstreamError(fmt.Sprintf("upstream-server-error-%d", code)), livekit.DisconnectReason_SIP_TRUNK_FAILURE
				}
			case code >= 600 && code < 700:
				res.status, res.term, res.reason = callRejected, stats.ClientError(fmt.Sprintf("global-decline-%d", code)), livekit.DisconnectReason_USER_REJECTED
				res.reportErr = nil
			}
		}
		return res
	}

	if errors.Is(err, ErrSIPRequestTimeout) {
		res.status, res.term, res.reason = callUnavailable, stats.ClientError("no-answer"), livekit.DisconnectReason_USER_UNAVAILABLE
		res.reportErr = nil
		return res
	}

	// Context cancellation / deadline. Check before *net.OpError because an
	// op error can wrap a context error, and the context cause is more
	// informative.
	if errors.Is(err, context.DeadlineExceeded) {
		res.status, res.term, res.reason = callDropped, stats.ServerError("deadline-exceeded"), livekit.DisconnectReason_UNKNOWN_REASON
		res.returnErr = psrpc.NewError(psrpc.DeadlineExceeded, err)
		return res
	}
	if errors.Is(err, context.Canceled) {
		res.status, res.term, res.reason = callRejected, stats.ClientError("canceled"), livekit.DisconnectReason_USER_UNAVAILABLE
		res.reportErr = nil
		res.returnErr = psrpc.NewError(psrpc.Canceled, err)
		return res
	}

	// Specific net error types before *net.OpError (which wraps them).
	if _, ok := errors.AsType[*net.AddrError](err); ok {
		res.status, res.term, res.reason = callDropped, stats.ClientError("address-error"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		res.returnErr = psrpc.NewError(psrpc.InvalidArgument, err)
		return res
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		res.status, res.term, res.reason = callDropped, stats.ClientError("dns-resolution"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		res.returnErr = psrpc.NewError(psrpc.InvalidArgument, err)
		return res
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		res.status, res.term, res.reason = callDropped, stats.ServerError("network-error"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		res.returnErr = psrpc.NewError(psrpc.Unavailable, err)
		return res
	}

	if errors.Is(err, ErrAuthMaxRetry) || errors.Is(err, ErrAuthMissingCreds) || errors.Is(err, ErrAuthNoHeader) {
		res.status, res.term, res.reason = callRejected, stats.ClientError("auth-failed"), livekit.DisconnectReason_USER_REJECTED
		// keep reportErr so the auth detail is recorded
		return res
	}

	return res
}
