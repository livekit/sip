package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
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
	EndCall
	returnErr error
}

// afterInvite makes a server-side failure non-retryable once an INVITE has been
// sent, so a fallback-capable SDK won't re-dial and place a duplicate call.
// Other result classes already map to non-retryable codes; Term is unchanged.
func (f inviteFailure) afterInvite() inviteFailure {
	if f.Term.Result == stats.ResultServerError {
		cause := f.returnErr
		if cause == nil {
			cause = errors.New(f.Term.Reason)
		}
		f.returnErr = psrpc.NewError(psrpc.FailedPrecondition, cause)
	}
	return f
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
		EndCall: EndCall{
			Status: callRejected,
			Reason: livekit.DisconnectReason_MEDIA_FAILURE,
			Report: e.Err,
		},
		returnErr: e,
	}
	switch {
	case errors.Is(e.Err, sdp.ErrNoCommonMedia):
		res.Term = stats.ClientError("no-common-codec")
	case errors.Is(e.Err, sdp.ErrNoCommonCrypto):
		res.Term = stats.ClientError("encryption-required")
	default:
		res.Term = stats.ClientError("sdp-error")
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
	// 0 provisionals: silent after transmit (Timer B), unattributable -> indeterminate.
	// >0: a 1xx proved the path works, so a later stall is client-side.
	term := stats.Indeterminate("upstream-no-response")
	if e.responses > 0 {
		term = stats.ClientError("no-final-response")
	}
	return inviteFailure{
		EndCall: EndCall{
			Status: callUnavailable,
			Term:   term,
			Reason: livekit.DisconnectReason_SIP_TRUNK_FAILURE,
			Report: e, // keep so the customer sees their destination didn't complete
		},
		returnErr: psrpc.NewError(psrpc.Canceled, e),
	}
}

// twilioPermanentBlockCodes are Twilio X-Twilio-Error codes for calls a carrier
// permanently blocks for fraud/regulatory reasons. Twilio surfaces these as a
// generic SIP 603 Decline, which is indistinguishable from an ordinary callee
// rejection, so we key off the proprietary error code instead. The distinct
// classification lets callers suppress the destination rather than retrying it.
//
// This lives alongside the Twilio rate-limit handling in classifyInviteError.
var twilioPermanentBlockCodes = map[int]struct{}{
	32203: {}, // call blocked as potential fraud / regulatory block
}

// carrierBlockedError is an outbound INVITE that a carrier permanently rejected
// (e.g. Twilio X-Twilio-Error 32203). It self-classifies so the failure reads
// distinctly from a normal decline, and unwraps to the underlying SIPStatus so
// the SIP code/text is still recorded on the call (CallStatusCode). Note it does
// not overwrite SIPStatus.Status — the SIP reason phrase is preserved; the
// provider block detail surfaces via the reported error and the SLI term.
type carrierBlockedError struct {
	status       *livekit.SIPStatus
	provider     string // e.g. "twilio"
	providerCode int    // e.g. 32203
}

var _ inviteClassifier = carrierBlockedError{}

func (e carrierBlockedError) Error() string {
	return fmt.Sprintf("carrier blocked call (%s error %d): %s", e.provider, e.providerCode, e.status.Error())
}

func (e carrierBlockedError) Unwrap() error { return e.status }

func (e carrierBlockedError) ClassifyInvite() inviteFailure {
	return inviteFailure{
		EndCall: EndCall{
			Status: callRejected,
			// Distinct SLI bucket: a carrier fraud/regulatory block is a
			// customer-side outcome, not upstream breakage.
			Term: stats.ClientError("carrier-blocked"),
			// No dedicated DisconnectReason enum exists yet; USER_REJECTED is the
			// closest fit. The differentiator is the term and the reported error,
			// which carry the provider block code for downstream consumers.
			Reason: livekit.DisconnectReason_USER_REJECTED,
			Report: e, // keep so the block (and provider code) is recorded
		},
		// Preserve the SIPStatus at the RPC boundary so ApplySIPStatus still maps
		// the SIP code (e.g. 603) for the caller, matching other SIPStatus paths.
		returnErr: e.status,
	}
}

// carrierBlockFromResponse returns a carrierBlockedError if resp carries a
// provider error code known to be a permanent carrier block, else nil. st is
// the SIPStatus already built for resp and is retained on the returned error.
func carrierBlockFromResponse(resp *sip.Response, st *livekit.SIPStatus) error {
	if h := resp.GetHeader("X-Twilio-Error"); h != nil {
		// Header format is "<code> <description>"; parse the leading code.
		if code := parseLeadingProviderCode(h.Value()); code != 0 {
			if _, ok := twilioPermanentBlockCodes[code]; ok {
				return carrierBlockedError{status: st, provider: "twilio", providerCode: code}
			}
		}
	}
	return nil
}

// parseLeadingProviderCode extracts the leading integer code from a provider
// error header value (e.g. "32203 Call blocked as potential fraud" -> 32203).
// Returns 0 when the value does not start with a number.
func parseLeadingProviderCode(v string) int {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
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
		EndCall: EndCall{
			Status: callDropped,
			Term:   stats.ServerError("invite-failed"),
			Reason: livekit.DisconnectReason_UNKNOWN_REASON,
			Report: err,
		},
		returnErr: err,
	}

	if sipStatus, ok := errors.AsType[*livekit.SIPStatus](err); ok {
		code := int(sipStatus.Code)
		switch code {
		case int(sip.StatusUnauthorized), int(sip.StatusProxyAuthRequired):
			res.Status, res.Term, res.Reason = callRejected, stats.ClientError("auth-required"), livekit.DisconnectReason_USER_REJECTED
			res.Report = nil
		case int(sip.StatusForbidden):
			res.Status, res.Term, res.Reason = callRejected, stats.ClientError("forbidden"), livekit.DisconnectReason_USER_REJECTED
			res.Report = nil
		case int(sip.StatusNotFound):
			res.Status, res.Term, res.Reason = callUnavailable, stats.ClientError("not-found"), livekit.DisconnectReason_USER_UNAVAILABLE
			res.Report = nil
		case int(sip.StatusRequestTimeout):
			res.Status, res.Term, res.Reason = callUnavailable, stats.ClientError("request-timeout"), livekit.DisconnectReason_USER_UNAVAILABLE
			res.Report = nil
		case int(sip.StatusTemporarilyUnavailable):
			res.Status, res.Term, res.Reason = callUnavailable, stats.ClientError("unavailable"), livekit.DisconnectReason_USER_UNAVAILABLE
			res.Report = nil
		case int(sip.StatusBusyHere):
			res.Status, res.Term, res.Reason = callRejected, stats.ClientError("busy"), livekit.DisconnectReason_USER_REJECTED
			res.Report = nil
		case int(sip.StatusNotAcceptableHere):
			res.Status, res.Term, res.Reason = callRejected, stats.ClientError("not-acceptable"), livekit.DisconnectReason_USER_REJECTED
			res.Report = nil
		default:
			switch {
			case code >= 400 && code < 500:
				res.Status, res.Term, res.Reason = callRejected, stats.ClientError(fmt.Sprintf("client-error-%d", code)), livekit.DisconnectReason_USER_UNAVAILABLE
				res.Report = nil
			case code >= 500 && code < 600:
				// Some upstreams (notably Twilio) return a 5xx when the customer's own trunk exceeds its configured CPS or
				// concurrent-call cap. That's a customer-side rate limit, not upstream infrastructure breakage, so it must not count
				// against the server-error SLI. Match on the response body — brittle, so kept narrow to the known phrases.
				body := strings.ToLower(sipStatus.GetStatus())
				switch {
				case strings.Contains(body, "cps limit exceeded"):
					res.Status, res.Term, res.Reason = callRejected, stats.ClientError("cps-limit-exceeded"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
					// keep reportErr so the customer can see they hit their cap
				case strings.Contains(body, "concurrent call limit exceeded"):
					res.Status, res.Term, res.Reason = callRejected, stats.ClientError("concurrent-limit-exceeded"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
					// keep reportErr so the customer can see they hit their cap
				default:
					// Carrier-side 5xx; keep reportErr for the detail.
					res.Status, res.Term, res.Reason = callDropped, stats.UpstreamError(fmt.Sprintf("upstream-server-error-%d", code)), livekit.DisconnectReason_SIP_TRUNK_FAILURE
				}
			case code >= 600 && code < 700:
				res.Status, res.Term, res.Reason = callRejected, stats.ClientError(fmt.Sprintf("global-decline-%d", code)), livekit.DisconnectReason_USER_REJECTED
				res.Report = nil
			}
		}
		return res
	}

	if errors.Is(err, ErrSIPRequestTimeout) {
		res.Status, res.Term, res.Reason = callUnavailable, stats.ClientError("no-answer"), livekit.DisconnectReason_USER_UNAVAILABLE
		res.Report = nil
		return res
	}

	// Context cancellation / deadline. Check before *net.OpError because an
	// op error can wrap a context error, and the context cause is more
	// informative.
	if errors.Is(err, context.DeadlineExceeded) {
		res.Status, res.Term, res.Reason = callDropped, stats.ServerError("deadline-exceeded"), livekit.DisconnectReason_UNKNOWN_REASON
		res.returnErr = psrpc.NewError(psrpc.DeadlineExceeded, err)
		return res
	}
	if errors.Is(err, context.Canceled) {
		res.Status, res.Term, res.Reason = callRejected, stats.ClientError("canceled"), livekit.DisconnectReason_USER_UNAVAILABLE
		res.Report = nil
		res.returnErr = psrpc.NewError(psrpc.Canceled, err)
		return res
	}

	// Specific net error types before *net.OpError (which wraps them).
	if _, ok := errors.AsType[*net.AddrError](err); ok {
		res.Status, res.Term, res.Reason = callDropped, stats.ClientError("address-error"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		res.returnErr = psrpc.NewError(psrpc.InvalidArgument, err)
		return res
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		res.Status, res.Term, res.Reason = callDropped, stats.ClientError("dns-resolution"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		res.returnErr = psrpc.NewError(psrpc.InvalidArgument, err)
		return res
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		res.Status, res.Term, res.Reason = callDropped, stats.ServerError("network-error"), livekit.DisconnectReason_SIP_TRUNK_FAILURE
		res.returnErr = psrpc.NewError(psrpc.Unavailable, err)
		return res
	}

	if errors.Is(err, ErrAuthMaxRetry) || errors.Is(err, ErrAuthMissingCreds) || errors.Is(err, ErrAuthNoHeader) {
		res.Status, res.Term, res.Reason = callRejected, stats.ClientError("auth-failed"), livekit.DisconnectReason_USER_REJECTED
		// keep reportErr so the auth detail is recorded
		return res
	}

	return res
}
