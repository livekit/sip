// Copyright 2024 LiveKit, Inc.
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
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
)

const (
	notifyAckTimeout = 5 * time.Second
	referByeTimeout  = time.Second
	// referResultGrace is how long a REFER result is still accepted after the
	// original call ended. Once the transfer target answers, our peer reports it
	// in the final NOTIFY and then BYEs the original leg, which is no longer
	// needed. We handle that NOTIFY and that BYE on separate goroutines, so the
	// BYE often wins the race. Without this window a transfer that actually
	// completed would be reported as aborted.
	referResultGrace = time.Second
)

var (
	referIdRegexp = regexp.MustCompile(`^refer(;id=(\d+))?$`)
)

type Result struct {
	Code   sip.StatusCode
	Status string
}

func (r Result) NewResponse(req *sip.Request) *sip.Response {
	if r.Code == 0 {
		r.Code = sip.StatusServiceUnavailable
	}
	if r.Status == "" {
		r.Status = sipStatus(r.Code)
	}
	return sip.NewResponseFromRequest(req, r.Code, r.Status, nil)
}

type EndCall struct {
	Report  error      // reported to LiveKit analytics
	Status  CallStatus // TODO: legacy
	Term    stats.Termination
	Reason  livekit.DisconnectReason // disconnect reason for LiveKit participant
	Headers map[string]string        // extra headers to send to SIP peer
}

var statusNamesMap = map[int]string{
	100: "Trying",
	180: "Ringing",
	181: "Call Is Forwarded",
	182: "Queued",
	183: "Session In Progress",

	200: "OK",
	202: "Accepted",

	301: "Moved Permanently",
	302: "Moved Temporarily",
	305: "Use Proxy",

	400: "Bad Request",
	401: "Unauthorized",
	402: "Payment Required",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	406: "Not Acceptable",
	407: "Proxy Auth Required",
	408: "Request Timeout",
	409: "Conflict",
	410: "Gone",
	413: "Request Entity Too Large",
	414: "Request URI Too Long",
	415: "Unsupported Media Type",
	416: "Requested Range Not Satisfiable",
	420: "Bad Extension",
	421: "Extension Required",
	423: "Interval Too Brief",
	480: "Temporarily Unavailable",
	481: "Call Transaction Does Not Exists",
	482: "Loop Detected",
	483: "Too Many Hops",
	484: "Address Incomplete",
	485: "Ambiguous",
	486: "Busy Here",
	487: "Request Terminated",
	488: "Not Acceptable Here",

	500: "Internal Server Error",
	501: "Not Implemented",
	502: "Bad Gateway",
	503: "Service Unavailable",
	504: "Gateway Timeout",
	505: "Version Not Supported",
	513: "Message Too Large",

	600: "Global Busy Everywhere",
	603: "Global Decline",
	604: "Global Does Not Exist Anywhere",
	606: "Global Not Acceptable",
}

func sipStatus(code sip.StatusCode) string {
	if name := statusNamesMap[int(code)]; name != "" {
		return name
	}
	return fmt.Sprintf("Status %d", int(code))
}

func statusName(status int) string {
	if name := statusNamesMap[status]; name != "" {
		return fmt.Sprintf("%d-%s", status, strings.ReplaceAll(name, " ", ""))
	}
	return fmt.Sprintf("status-%d", status)
}

// Sentinel errors emitted on outbound dial failure paths so callers can match
// them with errors.Is without depending on the human-readable message.
var (
	ErrSIPRequestTimeout = errors.New("sip request timed out")
	ErrAuthMaxRetry      = errors.New("max auth retry attempts reached for SIP invite")
	ErrAuthMissingCreds  = errors.New("sip server required auth, but no username or password was provided")
	ErrAuthNoHeader      = errors.New("no auth header in sip invite response")
)

// Sentinel errors for cold transfer outcomes the bridge decides on its own,
// without a SIP status from the peer. Wrapped in psrpc errors at the point they
// are produced; errors.Is still matches through the wrapper.
var (
	errTransferCallEnded           = errors.New("call ended before transfer completed")
	errReferSubscriptionTerminated = errors.New("REFER subscription terminated without a final status")
)

type setHeadersFunc func(headers map[string]string) map[string]string

type Signaling interface {
	Address() sip.Uri
	From() sip.Uri
	To() sip.Uri
	ID() LocalTag
	Tag() RemoteTag
	SIPCallID() string
	RemoteHeaders() Headers

	WriteRequest(req *sip.Request) error
	Transaction(req *sip.Request) (sip.ClientTransaction, error)

	Drop()
}

func transportFromURI(u *sip.Uri) Transport {
	if tr, _ := u.UriParams.Get("transport"); tr != "" {
		return Transport(strings.ToLower(tr))
	}
	return ""
}

// callTransportFromReq returns the SIP transport used between LK SIP and the provider.
// For the actual transport used between SIP server and the edge, see legTransportFromReq.
func callTransportFromReq(req *sip.Request) Transport {
	if to := req.To(); to != nil {
		if tr := transportFromURI(&to.Address); tr != "" {
			return tr
		}
		if tr, _ := to.Params.Get("transport"); tr != "" {
			return Transport(strings.ToLower(tr))
		}
	}
	if via := req.Via(); via != nil {
		return Transport(strings.ToLower(via.Transport))
	}
	return ""
}

// legTransportFromReq returns the SIP transport used between SIP server and LK SIP edge.
// For the transport used between LK SIP and the provider, see callTransportFromReq.
func legTransportFromReq(req *sip.Request) Transport {
	if via := req.Via(); via != nil {
		return Transport(strings.ToLower(via.Transport))
	}
	if tr := transportFromURI(&req.Recipient); tr != "" {
		return tr
	}
	if to := req.To(); to != nil {
		if tr := transportFromURI(&to.Address); tr != "" {
			return tr
		}
		if tr, _ := to.Params.Get("transport"); tr != "" {
			return Transport(strings.ToLower(tr))
		}
	}
	return ""
}

func transportPort(c *config.Config, t Transport) int {
	if t == TransportTLS {
		if tc := c.TLS; tc != nil {
			return tc.Port
		}
	}
	return c.SIPPort
}

func getContactURI(c *config.Config, ip netip.Addr, t Transport) URI {
	hostname := "" // use signaling IP by default, it's more robust
	if t == TransportTLS {
		hostname = c.SIPHostname
	}
	return URI{
		Host:      hostname,
		Addr:      netip.AddrPortFrom(ip, uint16(transportPort(c, t))),
		Transport: t,
	}
}

// sendBye sends a BYE and waits for its final response. BYE is a non-INVITE
// transaction (RFC 3261 §17.1.2): the response ends it, no ACK is sent.
func sendBye(ctx context.Context, log logger.Logger, c Signaling, req *sip.Request) {
	tx, err := c.Transaction(req)
	if err != nil {
		log.Infow("cannot send BYE", "error", err)
		return
	}
	defer tx.Terminate()
	if _, err := sipResponse(ctx, tx, nil, nil); err != nil {
		log.Infow("no response to BYE", "error", err)
	}
}

func NewReferRequest(inviteRequest *sip.Request, inviteResponse *sip.Response, contactHeader *sip.ContactHeader, referToUrl string, headers map[string]string) *sip.Request {
	req := sip.NewRequest(sip.REFER, inviteRequest.Recipient)

	req.SipVersion = inviteRequest.SipVersion
	sip.CopyHeaders("Via", inviteRequest, req)
	// if inviteResponse.IsSuccess() {
	// update branch, 2xx ACK is separate Tx
	viaHop := req.Via()
	viaHop.Params.Add("branch", sip.GenerateBranch())
	// }

	if len(inviteRequest.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", inviteRequest, req)
	} else {
		hdrs := inviteResponse.GetHeaders("Record-Route")
		for i := len(hdrs) - 1; i >= 0; i-- {
			rrh, ok := hdrs[i].(*sip.RecordRouteHeader)
			if !ok {
				continue
			}

			h := rrh.Clone()
			req.AppendHeader(h)
		}
	}

	maxForwardsHeader := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxForwardsHeader)

	if h := inviteRequest.From(); h != nil {
		sip.CopyHeaders("From", inviteRequest, req)
	}

	if h := inviteResponse.To(); h != nil {
		sip.CopyHeaders("To", inviteResponse, req)
	}

	if h := inviteRequest.CallID(); h != nil {
		sip.CopyHeaders("Call-ID", inviteRequest, req)
	}

	if h := inviteRequest.CSeq(); h != nil {
		sip.CopyHeaders("CSeq", inviteRequest, req)
	}

	req.AppendHeader(contactHeader)

	cseq := req.CSeq()
	cseq.SeqNo = cseq.SeqNo + 1
	cseq.MethodName = sip.REFER

	// Set Refer-To header
	referTo := sip.NewHeader("Refer-To", referToUrl)
	req.AppendHeader(referTo)
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	req.SetTransport(inviteRequest.Transport())
	req.SetSource(inviteRequest.Source())
	req.SetDestination(inviteRequest.Destination())

	for k, v := range headers {
		req.AppendHeader(sip.NewHeader(k, v))
	}

	req.SetBody(nil)

	return req
}

func sendRefer(ctx context.Context, c Signaling, req *sip.Request, stop <-chan struct{}) (*sip.Response, error) {
	ctx, span := Tracer.Start(ctx, "sip.sendRefer")
	defer span.End()
	tx, err := c.Transaction(req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()

	ctx = context.WithoutCancel(ctx)
	resp, err := sipResponse(ctx, tx, stop, nil)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case sip.StatusOK, 202: // 202 is Accepted
		return resp, nil
	default:
		return resp, &livekit.SIPStatus{
			Code:   livekit.SIPStatusCode(resp.StatusCode),
			Status: resp.Reason,
		}
	}
}

func parseNotifyBody(body string) (int, string, error) {
	v := strings.SplitN(body, " ", 3)

	if len(v) < 2 {
		return 0, "", psrpc.NewErrorf(psrpc.InvalidArgument, "invalid notify body: not enough tokens")
	}

	if strings.ToUpper(v[0]) != "SIP/2.0" {
		return 0, "", psrpc.NewErrorf(psrpc.InvalidArgument, "invalid notify body: wrong prefix or SIP version")
	}

	c, err := strconv.Atoi(v[1])
	if err != nil {
		return 0, "", psrpc.NewError(psrpc.InvalidArgument, err)
	}
	if len(v) < 3 {
		return c, "", nil
	}
	reason := v[2]
	if i := strings.Index(reason, "\n"); i != -1 {
		reason = strings.TrimSuffix(reason[:i], "\r")
	}
	return c, reason, nil
}

// notifyInfo is the parsed content of a NOTIFY, for the event packages we
// implement (currently only "refer").
type notifyInfo struct {
	Method sip.RequestMethod // event package; sip.REFER for "refer[;id=N]"
	CSeq   uint32            // REFER CSeq from the event id param, 0 if absent
	Status int               // sipfrag status code, 0 if the NOTIFY carried no body
	Reason string            // sipfrag reason phrase
	Sub    SubscriptionState // Subscription-State header, zero value if absent
}

func handleNotify(req *sip.Request) (notifyInfo, error) {
	event := req.GetHeader("Event")
	if event == nil {
		event = req.GetHeader("o")
	}
	if event == nil {
		return notifyInfo{}, psrpc.NewErrorf(psrpc.MalformedRequest, "no event in NOTIFY request")
	}

	m := referIdRegexp.FindStringSubmatch(strings.ToLower(event.Value()))
	if len(m) == 0 {
		return notifyInfo{}, psrpc.NewErrorf(psrpc.Unimplemented, "unknown event")
	}

	// REFER Notify
	info := notifyInfo{Method: sip.REFER}
	if len(m) >= 3 {
		cseq64, _ := strconv.ParseUint(m[2], 10, 32)
		info.CSeq = uint32(cseq64)
	}
	if h := req.GetHeader("Subscription-State"); h != nil {
		info.Sub = ParseSubscriptionState(h.Value())
	}
	// RFC 3515 requires a sipfrag body, but RFC 6665 lets a notifier omit the
	// state from the NOTIFY that terminates the subscription, and providers do.
	// Status 0 then means "no status reported" and Subscription-State decides.
	if body := strings.TrimSpace(string(req.Body())); body != "" {
		status, reason, err := parseNotifyBody(body)
		if err != nil {
			return notifyInfo{}, err
		}
		info.Status, info.Reason = status, reason
	}
	return info, nil
}

func handleReferNotify(info notifyInfo, referCseq uint32, referDone chan<- error) {
	if info.CSeq != 0 && info.CSeq != referCseq {
		// NOTIFY for a different REFER, skip
		return
	}
	var result error
	switch {
	case info.Status == 200:
		// Success. Checked before the terminated subscription below: the final
		// NOTIFY of a successful transfer also terminates the subscription.
		result = nil
	case info.Status == 0 || (info.Status >= 100 && info.Status < 200):
		// No final status yet, but if this NOTIFY ended the subscription, no
		// further NOTIFY can arrive and the provisional status we have is all
		// we will ever get, so fail now rather than waiting out the transfer
		// deadline. RFC 3515 lets an agent that does not want to hold subscription
		// state terminate with its very first NOTIFY, and for a call still in
		// progress, that NOTIFY carries a 100. A subscription that expires, or
		// whose notifier gives up, ends the same way.
		if !info.Sub.Terminated() {
			// still trying
			return
		}
		reason := info.Sub.Reason
		if reason == "" {
			reason = "unspecified"
		}
		result = psrpc.NewErrorf(psrpc.UpstreamServerError, "call transfer failed: %w (reason %q, last status %d)",
			errReferSubscriptionTerminated, reason, info.Status)
	default:
		// Failure
		st := &livekit.SIPStatus{
			Code:   livekit.SIPStatusCode(info.Status),
			Status: info.Reason,
		}
		// Converts SIP status to GRPC via SIPStatus.GRPCStatus(), then converts to psrpc via ErrorCodeFromGRPC()
		errorCode, _ := psrpc.GetErrorCode(st)
		if errorCode == psrpc.Internal || errorCode == psrpc.Unavailable {
			// Temporarily overwrite the code until we support a direct SIPStatus -> psrpc.ErrorCode conversion
			errorCode = psrpc.UpstreamServerError
			if info.Status < 500 || info.Status >= 600 { // Common 6xx codes: 603 Declined, 608 Rejected
				errorCode = psrpc.UpstreamClientError
			}
		}
		result = psrpc.NewErrorf(errorCode, "call transfer failed: %w", st)
	}
	select {
	case referDone <- result:
	case <-time.After(notifyAckTimeout):
	}
}

// waitReferResult waits for the outcome of an accepted REFER: a NOTIFY carrying
// a final status, or the subscription ending before one arrives.
//
// callDone fires when the call itself ends (remote BYE, room deletion, local
// hangup). That leaves the transfer outcome unknown, which is a failure and not
// a success. A result that is already on its way wins over it, because a
// successful transfer ends this call too: our peer BYEs the original leg right
// after reporting the outcome. referDone is unbuffered, so its NOTIFY handler
// can still be parked on the handoff while the BYE is processed elsewhere.
func waitReferResult(ctx context.Context, log logger.Logger, callDone <-chan struct{}, referDone <-chan error) error {
	select {
	case <-ctx.Done():
		// Wrap ctx.Err() so callers can still tell a blown deadline from a cancel.
		return psrpc.NewErrorf(psrpc.Canceled, "refer canceled: %w", ctx.Err())
	case err := <-referDone:
		return err
	case <-callDone:
		select {
		case err := <-referDone:
			log.Infow("refer result raced call end", "error", err)
			return err
		case <-time.After(referResultGrace):
		}
		log.Infow("refer failed: call ended before transfer completed")
		return psrpc.NewError(psrpc.Aborted, errTransferCallEnded)
	}
}

func sipStatusForErrorCode(code psrpc.ErrorCode) sip.StatusCode {
	switch code {
	case psrpc.OK:
		return sip.StatusOK
	case psrpc.Canceled, psrpc.DeadlineExceeded:
		return sip.StatusRequestTimeout
	case psrpc.Unknown, psrpc.MalformedResponse, psrpc.Internal, psrpc.DataLoss:
		return sip.StatusInternalServerError
	case psrpc.InvalidArgument, psrpc.MalformedRequest:
		return sip.StatusBadRequest
	case psrpc.NotFound:
		return sip.StatusNotFound
	case psrpc.NotAcceptable:
		return sip.StatusNotAcceptable
	case psrpc.AlreadyExists, psrpc.Aborted:
		return sip.StatusConflict
	case psrpc.PermissionDenied:
		return sip.StatusForbidden
	case psrpc.ResourceExhausted:
		return sip.StatusTemporarilyUnavailable
	case psrpc.FailedPrecondition:
		return sip.StatusCallTransactionDoesNotExists
	case psrpc.OutOfRange:
		return sip.StatusRequestedRangeNotSatisfiable
	case psrpc.Unimplemented:
		return sip.StatusNotImplemented
	case psrpc.Unavailable:
		return sip.StatusServiceUnavailable
	case psrpc.Unauthenticated:
		return sip.StatusUnauthorized
	case psrpc.UpstreamServerError:
		return sip.StatusBadGateway
	case psrpc.UpstreamClientError:
		return sip.StatusTemporarilyUnavailable
	default:
		return sip.StatusInternalServerError
	}
}

func sipCodeAndMessageFromError(err error) (code sip.StatusCode, msg string) {
	code = 200
	var psrpcErr psrpc.Error
	if errors.As(err, &psrpcErr) {
		code = sipStatusForErrorCode(psrpcErr.Code())
	} else if err != nil {
		code = 500
	}

	msg = "success"
	if err != nil {
		msg = err.Error()
	}

	return code, msg
}

func setCSeq(req *sip.Request, cseq uint32) {
	h := &sip.CSeqHeader{
		MethodName: req.Method,
		SeqNo:      cseq,
	}

	req.RemoveHeader(h.Name())
	req.AppendHeader(h)
}

func ToSIPUri(ip string, u sip.Uri) *livekit.SIPUri {
	tr, _ := u.UriParams.Get("transport")
	url := &livekit.SIPUri{
		User:      u.User,
		Host:      u.Host,
		Ip:        ip,
		Port:      uint32(u.Port),
		Transport: SIPTransportFrom(Transport(tr)),
	}
	return url
}

// SubscriptionState is a parsed Subscription-State header. Every NOTIFY must
// carry one, including the NOTIFYs of the subscription a REFER creates
// implicitly, but not every provider sends it.
type SubscriptionState struct {
	State   string // substate: "active", "pending", "terminated", or an extension
	Reason  string // reason param: noresource, giveup, timeout, rejected, ...
	Expires int    // expires param in seconds, 0 if absent
}

// Terminated reports whether the notifier ended the subscription, meaning no
// further NOTIFY will arrive for it.
func (s SubscriptionState) Terminated() bool {
	return s.State == "terminated"
}

func (s SubscriptionState) String() string {
	if s.State == "" {
		return "<none>"
	}
	if s.Reason == "" {
		return s.State
	}
	return s.State + ";reason=" + s.Reason
}

// ParseSubscriptionState parses a Subscription-State header value. It has no
// error return on purpose: a handleNotify error becomes a non-2xx answer to the
// NOTIFY, and an odd value in this header is no reason to reject one. An
// unrecognized state yields the zero value, which is not Terminated, so the
// transfer keeps waiting.
func ParseSubscriptionState(header string) SubscriptionState {
	list := strings.Split(header, ";")
	st := SubscriptionState{State: strings.ToLower(strings.TrimSpace(list[0]))}
	for _, line := range list[1:] {
		line = strings.TrimSpace(line)
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "reason":
			st.Reason = strings.ToLower(val)
		case "expires":
			st.Expires, _ = strconv.Atoi(val)
		}
	}
	return st
}

type ReasonHeader struct {
	Type  string
	Cause int
	Text  string
}

func (r ReasonHeader) IsZero() bool {
	return r == ReasonHeader{}
}

func (r ReasonHeader) IsNormal() bool {
	if r.IsZero() {
		return true // assume there's no specific reason
	}
	switch r.Type {
	case "q.850":
		switch r.Cause {
		case 16: // Normal call clearing
			return true
		}
	case "x.int":
		switch r.Cause {
		case 0x00:
			return true
		}
	case "release_cause":
		switch r.Cause {
		case 1:
			return true
		}
	case "sip":
		switch r.Cause {
		case 0: // not set, assume success
			return true
		case 200:
			return true
		}
	}
	return false
}

func (r ReasonHeader) String() string {
	if r.IsZero() {
		return "<none>"
	}
	return fmt.Sprintf("%s-%d: %s", r.Type, r.Cause, r.Text)
}

func ParseReasonHeader(header string) (ReasonHeader, error) {
	list := strings.Split(header, ";")
	if len(list) < 2 {
		return ReasonHeader{}, errors.New("no fields in the reason")
	}
	typ := strings.TrimSpace(list[0])
	typ = strings.ToLower(typ)
	r := ReasonHeader{Type: typ}
	var reasonCode string
	for _, line := range list[1:] {
		line = strings.TrimSpace(line)
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "cause":
			r.Cause, _ = strconv.Atoi(val)
		case "text":
			r.Text, _ = strconv.Unquote(val)
		case "description":
			if r.Text == "" {
				r.Text, _ = strconv.Unquote(val)
			}
		case "reasoncode":
			reasonCode = val
		}
	}
	switch typ {
	case "x.int":
		if r.Cause == 0 {
			if reasonCode != "" {
				v, _ := strconv.ParseUint(reasonCode, 0, 64)
				r.Cause = int(v)
			} else if r.Text != "" {
				v, err := strconv.ParseUint(r.Text, 0, 64)
				r.Cause = int(v)
				if err == nil {
					r.Text = ""
				}
			}
		}
	}
	return r, nil
}
