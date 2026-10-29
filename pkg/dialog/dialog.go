// Package dialog provides a unified Dialog type for both UAC and UAS, with early-dialog
// support and handling for all in-dialog methods (INVITE, ACK, BYE, OPTIONS, UPDATE,
// PRACK, REFER, INFO, SUBSCRIBE, NOTIFY) plus CANCEL in early state.
package dialog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	gsip "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/livekit/protocol/logger"
)

var (
	ErrNotInDialog    = errors.New("request not in dialog")
	ErrBadRequest     = errors.New("bad request")
	ErrDialogNotFound = errors.New("dialog not found")
	ErrDialogRejected = errors.New("dialog rejected")

	ErrInvalidDialogState = errors.New("invalid dialog state")
	ErrDialogInvalidCseq  = errors.New("invalid CSEQ for dialog")
	ErrDialogMismatch     = errors.New("request does not match dialog")
	ErrDialogEnded        = errors.New("dialog already ended")

	ErrDialogNotImplemented = errors.New("dialog method not implemented")

	ErrSkip = errors.New("skip")

	DialogMinRequiredHeaders = []string{"From", "To", "Call-ID", "Contact"}

	supportedInDialogMethods = []gsip.RequestMethod{
		gsip.INVITE,
		gsip.ACK,
		gsip.OPTIONS,
		gsip.BYE,
		gsip.UPDATE,
		gsip.PRACK,
		gsip.REFER,
		gsip.INFO,
		gsip.NOTIFY,
		gsip.CANCEL,
	}
)

var dialogStateStrings = [...]string{
	"DialogStateAllocated",
	"DialogStateEarly",
	"DialogStateAckPending",
	"DialogStateConfirmed",
	"DialogStateEnded",
}

type DialogState int

func (s DialogState) String() string {
	if int(s) >= len(dialogStateStrings) {
		return "DialogStateUnknown"
	}
	return dialogStateStrings[s]
}

const (
	DialogStateAllocated  DialogState = iota // Allocating dialog ID
	DialogStateEarly                         // No ID yet
	DialogStateAckPending                    // 2xx sent, waiting for ACK. ID set, dialog mapped.
	DialogStateConfirmed                     // Fully established
	DialogStateEnded                         // Ended by BYE or CANCEL
)

type DialogRole int // Initial Dialog Role, not applicable once dialog is established

const (
	DialogRoleUAC DialogRole = iota // Sending the initial INVITE
	DialogRoleUAS                   // Receiving the initial INVITE
)

type DialogRequestHandler func(req *gsip.Request, tx gsip.ServerTransaction, d *Dialog) error
type DialogStateChangeHandler func(d *Dialog, from DialogState, to DialogState)
type dialogResponseInterceptor func(res *gsip.Response)

var dialogTransitions = map[DialogState][]DialogState{
	DialogStateAllocated:  {DialogStateEarly, DialogStateEnded},
	DialogStateEarly:      {DialogStateAckPending, DialogStateConfirmed, DialogStateEnded},
	DialogStateAckPending: {DialogStateConfirmed, DialogStateEnded},
	DialogStateConfirmed:  {DialogStateEnded},
	DialogStateEnded:      {},
}

func cseqExtract(m gsip.Message) (string, int) {
	cseq := m.CSeq()
	if cseq == nil {
		return "", 0
	}
	return cseq.MethodName.String(), int(cseq.SeqNo)
}

type dialogTransactionUAS struct {
	gsip.ServerTransaction
	id string
	d  *Dialog
}

func NewDialogTransactionUAS(d *Dialog, req *gsip.Request, tx gsip.ServerTransaction) gsip.ServerTransaction {
	via := req.Via()
	if via == nil {
		return tx
	}
	dt := &dialogTransactionUAS{
		ServerTransaction: tx,
		d:                 d,
		id:                via.Params.GetOr("branch", ""),
	}
	method, seqNo := cseqExtract(req)
	dt.d.log.Debugw("processing request", "method", method, "cseq", seqNo, "tx", dt.id)
	return dt
}

func (dt *dialogTransactionUAS) Respond(res *gsip.Response) error {
	cseq := res.CSeq()
	if cseq != nil && cseq.MethodName == gsip.INVITE && !dt.d.inviteResponse {
		err := dt.d.SetInviteResponse(res)
		if err != nil {
			return err
		}
	} else if cseq != nil && cseq.MethodName == gsip.BYE {
		dt.d.setState(DialogStateEnded)
	}
	method, seqNo := cseqExtract(res)
	dt.d.log.Debugw("sending response", "status", res.StatusCode, "method", method, "cseq", seqNo, "tx", dt.id)
	err := dt.ServerTransaction.Respond(res)
	if errors.Is(err, gsip.ErrTransactionTerminated) {
		return nil // Reliable transport, transaction already deleted and that's OK
	}
	return err
}

type dialogTransactionUAC struct {
	gsip.ClientTransaction
	id                   string
	d                    *Dialog
	interceptedResponses chan *gsip.Response
	cb                   dialogResponseInterceptor
	stop                 chan struct{}
}

func NewDialogTransactionUAC(d *Dialog, req *gsip.Request, tx gsip.ClientTransaction, cb dialogResponseInterceptor) gsip.ClientTransaction {
	branch := ""
	if via := req.Via(); via != nil {
		branch = via.Params.GetOr("branch", "")
	}
	dt := &dialogTransactionUAC{
		ClientTransaction:    tx,
		id:                   branch,
		d:                    d,
		interceptedResponses: make(chan *gsip.Response),
		cb:                   cb,
		stop:                 make(chan struct{}),
	}
	tx.OnTerminate(func(_ string, _ error) {
		close(dt.stop)
	})
	go dt.InterceptResponses()
	method, seqNo := cseqExtract(req)
	dt.d.log.Debugw("sending request", "method", method, "cseq", seqNo, "tx", dt.id)
	return dt
}

func (dt *dialogTransactionUAC) Responses() <-chan *gsip.Response {
	return dt.interceptedResponses
}

func (dt *dialogTransactionUAC) InterceptResponses() {
	defer close(dt.interceptedResponses)
	for {
		select {
		case <-dt.stop:
			return
		case res := <-dt.ClientTransaction.Responses():
			if res == nil {
				return
			}
			method, seqNo := cseqExtract(res)
			dt.d.log.Debugw("received response", "status", res.StatusCode, "method", method, "cseq", seqNo, "tx", dt.id)
			if dt.cb != nil {
				dt.cb(res)
			}
			dt.interceptedResponses <- res
		}
	}
}

// Dialog is a unified SIP dialog that can be created as UAC or UAS, supports early and confirmed state,
// and can receive any in-dialog method (including CANCEL in early state).
type Dialog struct {
	log    logger.Logger
	ctx    context.Context
	cancel context.CancelCauseFunc
	client *sipgo.Client

	role DialogRole
	id   string

	// Original tx & request, used to auto-respond to INVITE requests per spec
	inviteReq *gsip.Request
	inviteTx  gsip.ServerTransaction

	// Dialog related definitions
	inviteRequest  bool
	inviteResponse bool
	localCSeqNo    atomic.Uint32
	localFrom      *gsip.FromHeader
	localTag       string
	localContact   *gsip.ContactHeader
	localUri       gsip.Uri
	transport      string
	callID         *gsip.CallIDHeader
	routeSet       []gsip.Header
	remoteCSeqNo   atomic.Uint32
	remoteTo       *gsip.ToHeader
	remoteTag      string
	remoteUri      gsip.Uri
	remoteSource   string

	lastTransport atomic.Pointer[string]
	lastSource    atomic.Pointer[string]

	mu           sync.RWMutex
	handler      DialogRequestHandler
	state        DialogState
	stateOnEnter map[DialogState][]DialogStateChangeHandler
}

func NewDialog(ctx context.Context, log logger.Logger, client *sipgo.Client, role DialogRole) *Dialog {
	ctx, cancel := context.WithCancelCause(ctx)
	d := &Dialog{
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
		client:   client,
		role:     role,
		state:    DialogStateAllocated,
		routeSet: make([]gsip.Header, 0, 10),
	}
	d.OnEnter(DialogStateAckPending, d.onAckPending)
	d.OnEnter(DialogStateConfirmed, d.onConfirmed)
	d.OnEnter(DialogStateEnded, func(d *Dialog, _, _ DialogState) {
		d.cancel(nil)
	})
	go d.cleanup()
	return d
}

func (d *Dialog) Close() {
	if d != nil { // QoL: allow calling ForceEnd on nil dialog
		d.cancel(nil)
	}
}

func (d *Dialog) onAckPending(_ *Dialog, _, _ DialogState) {
	// Final response is in, never respond to INVITE requests beyond this point
	d.inviteReq = nil
	d.inviteTx = nil
}

func (d *Dialog) onConfirmed(_ *Dialog, _, _ DialogState) {
	d.log.Debugw("dialog confirmed")
}

func (d *Dialog) cleanup() {
	<-d.ctx.Done()
	d.log.Debugw("dialog shutting down")
	defer d.setState(DialogStateEnded) // May be a no-op if already ended
	// Terminate SIP dialog depending on state
	var err error
	switch d.State() {
	case DialogStateEarly:
		if d.role == DialogRoleUAC {
			_, err = d.Cancel()
		} else if d.inviteReq != nil && d.inviteTx != nil {
			resp := d.NewResponse(d.inviteReq, 487, "Request Terminated", nil)
			d.inviteTx.Respond(resp) // tx probably closed; best effort
		}
	case DialogStateAckPending, DialogStateConfirmed:
		_, err = d.Bye()
	case DialogStateEnded:
		// Already terminated SIP
	default: // Invalid state
		d.log.Errorw("invalid dialog state", nil, "state", d.State())
	}
	if err != nil {
		d.log.Errorw("failed to cleanup dialog", err)
	}
}

func (d *Dialog) checkTransport(transport string) {
	old := d.lastTransport.Swap(&transport)
	if old != nil && *old != transport {
		d.log.Errorw("transport mismatch", nil, "old", *old, "new", transport)
	}
}

func (d *Dialog) checkSource(source string) {
	old := d.lastSource.Swap(&source)
	if old != nil && *old != source {
		d.log.Errorw("source mismatch", nil, "old", *old, "new", source)
	}
}

func (d *Dialog) SetInviteRequest(req *gsip.Request, tx gsip.ServerTransaction) error {
	if err := validateMessageHeaders(req, DialogMinRequiredHeaders); d.inviteRequest || req == nil || err != nil {
		return ErrBadRequest
	}
	if req.Method != gsip.INVITE || req.CSeq().MethodName != gsip.INVITE || req.From().Params.GetOr("tag", "") == "" {
		return ErrBadRequest
	}
	d.inviteRequest = true
	d.inviteTx = tx
	d.inviteReq = req
	d.callID = req.CallID()
	d.log = d.log.WithValues("sipCallID", d.callID.Value())
	if d.role == DialogRoleUAC {
		d.localFrom = req.From()
		d.localTag = req.From().Params.GetOr("tag", "")
		d.localContact = req.Contact()
		d.localCSeqNo.Store(req.CSeq().SeqNo)
		if d.localContact != nil {
			d.localUri = d.localContact.Address
		}
	} else {
		rrs := req.GetHeaders("Record-Route")
		for _, recordRoute := range rrs {
			d.routeSet = append(d.routeSet, gsip.NewHeader("Route", recordRoute.Value()))
		}
		d.transport = req.Transport()
		d.remoteCSeqNo.Store(req.CSeq().SeqNo)
		remoteTo := req.From().AsTo()
		d.remoteTo = &remoteTo
		d.remoteTag = req.From().Params.GetOr("tag", "")
		d.log = d.log.WithValues("callID", d.remoteTag)
		d.remoteUri = req.Contact().Address
		d.remoteSource = req.Source()
		d.localUri = req.Recipient
		d.localCSeqNo.Store(1)
	}
	d.setState(DialogStateEarly)
	return nil
}

func (d *Dialog) SetInviteResponse(rsp *gsip.Response) error {
	if rsp.StatusCode < 200 {
		return nil
	}
	if err := validateMessageHeaders(rsp, DialogMinRequiredHeaders); !d.inviteRequest || d.inviteResponse || rsp == nil || err != nil {
		return ErrBadRequest
	}
	if rsp.CSeq().MethodName != gsip.INVITE || rsp.To().Params.GetOr("tag", "") == "" {
		return ErrBadRequest
	}
	if rsp.StatusCode >= 300 {
		d.setState(DialogStateEnded)
		return nil
	}
	// 2xx response
	d.inviteResponse = true
	d.transport = rsp.Transport()
	targetState := DialogStateAckPending
	switch d.role {
	case DialogRoleUAC:
		rrs := rsp.GetHeaders("record-route")
		for i := range rrs {
			recordRoute := rrs[len(rrs)-1-i]
			d.routeSet = append(d.routeSet, gsip.NewHeader("Route", recordRoute.Value()))
		}
		d.remoteTo = rsp.To()
		d.remoteTag = rsp.To().Params.GetOr("tag", "")
		d.log = d.log.WithValues("callID", d.remoteTag)
		d.remoteUri = rsp.Contact().Address
		d.remoteSource = rsp.Source()
	case DialogRoleUAS:
		localFrom := rsp.To().AsFrom()
		d.localFrom = &localFrom
		d.localTag = rsp.To().Params.GetOr("tag", "")
		d.localContact = rsp.Contact()
		if d.localContact != nil {
			d.localUri = d.localContact.Address
		}
	}
	d.id = gsip.DialogIDMake(d.callID.Value(), d.localTag, d.remoteTag)
	if err := d.validate(); err != nil {
		d.log.Errorw("invalid invite response", err, "state", d.State())
		return ErrInvalidDialogState
	}
	d.setState(targetState)
	return nil
}

func (d *Dialog) SetLogger(log logger.Logger) {
	if d.callID != nil {
		log = log.WithValues("sipCallID", d.callID.Value())
	}
	if d.remoteTag != "" {
		log = log.WithValues("callID", d.remoteTag)
	}
	d.log = log
}

// OnEnter registers a callback for when the dialog enters a specific state.
func (d *Dialog) OnEnter(state DialogState, h DialogStateChangeHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	handlers := append(d.stateOnEnter[state], h)
	d.stateOnEnter[state] = handlers
}

func (d *Dialog) setState(state DialogState) {
	d.mu.Lock()
	if state == d.state {
		d.mu.Unlock()
		return
	}
	oldState := d.state
	d.state = state
	handlers := slices.Clone(d.stateOnEnter[state])
	d.mu.Unlock()

	for _, h := range handlers {
		h(d, oldState, state)
	}
}

func (d *Dialog) ID() string               { return d.id }
func (d *Dialog) Context() context.Context { return d.ctx }

func (d *Dialog) CallID() string {
	if d.callID != nil {
		return d.callID.Value()
	}
	return ""
}

func (d *Dialog) RemoteTag() string {
	return d.remoteTag
}

func (d *Dialog) State() DialogState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

func (d *Dialog) getHandler() DialogRequestHandler {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.handler
}

func (d *Dialog) SetHandler(h DialogRequestHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handler = h
}

func (d *Dialog) validateCSeq(req *gsip.Request) error {
	cseq := req.CSeq()
	if cseq == nil {
		return ErrDialogInvalidCseq
	}
	newSeq := cseq.SeqNo
	for {
		old := d.remoteCSeqNo.Load()
		if newSeq < old {
			return ErrDialogInvalidCseq
		}
		if d.remoteCSeqNo.CompareAndSwap(old, newSeq) {
			return nil
		}
	}
}

func (d *Dialog) validate() error {
	if d.id == "" {
		return fmt.Errorf("dialog ID is not set")
	}
	if d.inviteRequest == false {
		return fmt.Errorf("invite request is not set")
	}
	if d.inviteResponse == false {
		return fmt.Errorf("invite response is not set")
	}
	if d.localCSeqNo.Load() == 0 {
		return fmt.Errorf("local CSeq is not set")
	}
	if d.localFrom == nil {
		return fmt.Errorf("local From is not set")
	}
	if d.localContact == nil {
		return fmt.Errorf("local Contact is not set")
	}
	if d.transport == "" {
		return fmt.Errorf("transport is not set")
	}
	if d.callID == nil {
		return fmt.Errorf("call ID is not set")
	}
	if d.remoteTo == nil {
		return fmt.Errorf("remote To is not set")
	}
	if d.remoteUri.Host == "" {
		return fmt.Errorf("remote URI host is not set")
	}
	if d.remoteSource == "" {
		return fmt.Errorf("remote source is not set")
	}
	return nil
}

func (d *Dialog) Bye() (gsip.ClientTransaction, error) { // TODO: Add reason?
	state := d.State()
	if state != DialogStateAckPending && state != DialogStateConfirmed {
		return nil, ErrInvalidDialogState
	}
	defer d.setState(DialogStateEnded)

	req, err := d.NewRequest(gsip.BYE, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build BYE request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second) // At this point, dialog might have been ended via context
	defer cancel()
	tx, err := d.TransactionRequest(ctx, req) // Handle retrnasmissions internally
	if err != nil {
		return nil, fmt.Errorf("failed to send BYE request: %w", err)
	}
	return tx, nil
}

func (d *Dialog) Cancel() (gsip.ClientTransaction, error) { // TODO: Add reason?
	if d.State() != DialogStateEarly {
		return nil, ErrInvalidDialogState
	}
	defer d.setState(DialogStateEnded)
	req, err := d.NewRequest(gsip.CANCEL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build CANCEL request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second) // At this point, dialog might have been ended via context
	defer cancel()
	tx, err := d.TransactionRequest(ctx, req) // Handle retrnasmissions internally
	if err != nil {
		return nil, fmt.Errorf("failed to send CANCEL request: %w", err)
	}
	return tx, nil
}

// Ack sends an ACK message for the given response. Skip sending an ACK if the
// response is not a 2xx
func (d *Dialog) Ack(res *gsip.Response, sdpBody []byte) error {
	cseq := res.CSeq()
	if cseq == nil || cseq.MethodName != gsip.INVITE {
		return fmt.Errorf("invalid Cseq header: %+v", cseq)
	}

	if !res.IsSuccess() {
		// sipgo automatically sends ACK for non-2xx responses
		return nil
	}

	// sipgo doesn't ACK 2xx responses automatically, so we need to do it manually
	ackCSeq := &gsip.CSeqHeader{SeqNo: cseq.SeqNo, MethodName: gsip.ACK} // Don't mutate, races readers
	ackReq, err := d.NewRequest(gsip.ACK, []gsip.Header{ackCSeq})        // Per RFC, ACK reuses INVITE CSeq number
	if err != nil {
		d.log.Errorw("new ACK request failed", err, "status", res.StatusCode)
		return err
	}

	if len(sdpBody) > 0 {
		ackReq.SetBody(sdpBody)
		contentType := gsip.ContentTypeHeader("application/sdp")
		ackReq.AppendHeader(&contentType)
	}

	err = d.WriteRequest(ackReq)
	if err != nil {
		d.log.Errorw("write ACK request failed", err, "status", res.StatusCode)
		return err
	}
	d.setState(DialogStateConfirmed)
	return nil
}

func (d *Dialog) ProcessRequest(req *gsip.Request, tx gsip.ServerTransaction) error {
	d.checkTransport(req.Transport())
	d.checkSource(req.Source())
	if err := d.validateCSeq(req); err != nil {
		return err
	}
	tx = NewDialogTransactionUAS(d, req, tx)
	state := d.State()
	var resp *gsip.Response
	switch req.Method {
	case gsip.ACK:
		switch state {
		case DialogStateAckPending:
			defer d.setState(DialogStateConfirmed)
		case DialogStateConfirmed:
		default:
			d.log.Warnw("invalid ACK request", nil, "state", state)
		}
	case gsip.CANCEL:
		switch state {
		case DialogStateEarly:
			defer d.setState(DialogStateEnded)
			resp = d.NewResponse(req, 200, "OK", nil)
		default:
			// Technically and invalid state, but per spec we still need to response with 200,
			// with the cancel being a no-op. See RFC 3261 Section 9.2.
			resp = d.NewResponse(req, 200, "OK", nil)
		}
	case gsip.BYE:
		defer d.setState(DialogStateEnded)
		resp = d.NewResponse(req, 200, "OK", nil)
	}
	if resp != nil {
		err := tx.Respond(resp)
		if err != nil {
			return fmt.Errorf("failed to respond to request: %w", err)
		}
	}

	h := d.getHandler()
	if h != nil {
		err := h(req, tx, d)
		if err != nil {
			return fmt.Errorf("error processing request: %w", err)
		}
	}
	return nil
}

func (d *Dialog) BuildRequest(req *gsip.Request) error {
	if req.Method == gsip.UPDATE {
		return ErrDialogNotImplemented
	}
	state := d.State()
	if !(state == DialogStateAckPending && req.Method == gsip.ACK) &&
		(state != DialogStateConfirmed && req.Method != gsip.CANCEL && req.Method != gsip.BYE) {
		return ErrInvalidDialogState
	}
	if !slices.Contains(supportedInDialogMethods, req.Method) {
		return ErrBadRequest
	}

	mustHaveHeaders := make([]gsip.Header, 0, 6)
	if h := req.From(); h == nil {
		mustHaveHeaders = append(mustHaveHeaders, gsip.HeaderClone(d.localFrom))
	}
	if h := req.To(); h == nil {
		remoteTo := d.remoteTo
		if remoteTo == nil { // UAC, unset yet, in Early state
			remoteTo = d.inviteReq.To()
		}
		mustHaveHeaders = append(mustHaveHeaders, gsip.HeaderClone(remoteTo))
	}
	if h := req.Contact(); h == nil {
		mustHaveHeaders = append(mustHaveHeaders, gsip.HeaderClone(d.localContact))
	}
	if h := req.CallID(); h == nil {
		mustHaveHeaders = append(mustHaveHeaders, gsip.HeaderClone(d.callID))
	}
	cseq := req.CSeq()
	if cseq == nil {
		cseq = &gsip.CSeqHeader{MethodName: req.Method}
		mustHaveHeaders = append(mustHaveHeaders, cseq)
	}
	if h := req.MaxForwards(); h == nil {
		maxFwd := gsip.MaxForwardsHeader(70)
		mustHaveHeaders = append(mustHaveHeaders, &maxFwd)
	}
	if len(mustHaveHeaders) > 0 {
		req.PrependHeader(mustHaveHeaders...)
	}

	// For safety make sure we are starting with our last dialog cseq num
	cseq.SeqNo = d.localCSeqNo.Load()
	if !req.IsAck() && !req.IsCancel() {
		cseq.SeqNo++
	}

	for _, rh := range d.routeSet {
		req.AppendHeader(gsip.HeaderClone(rh))
	}

	if rh := req.Route(); rh != nil && !rh.Address.UriParams.Has("lr") {
		req.Recipient = rh.Address
		// TODO: Might need to pop that first header
	}

	d.localCSeqNo.Store(cseq.SeqNo)
	req.SetTransport(d.transport)
	return nil
}

func (d *Dialog) NewRequest(method gsip.RequestMethod, headers []gsip.Header) (*gsip.Request, error) {
	remoteUri := d.remoteUri
	if remoteUri.Host == "" { // UAC, unset yet, in Early state
		remoteUri = d.inviteReq.Recipient
	}
	req := gsip.NewRequest(method, remoteUri)
	for _, h := range headers {
		req.AppendHeader(h)
	}
	err := d.BuildRequest(req)
	if err != nil {
		return nil, err
	}
	return req, nil
}

func (d *Dialog) NewResponse(req *gsip.Request, status int, reason string, body []byte, headers ...gsip.Header) *gsip.Response {
	resp := gsip.NewResponseFromRequest(req, status, reason, body)
	for _, h := range headers {
		resp.AppendHeader(h)
	}
	if h := resp.Contact(); h == nil {
		if d.localContact != nil {
			resp.AppendHeader(gsip.HeaderClone(d.localContact))
		} else {
			resp.AppendHeader(&gsip.ContactHeader{Address: d.localUri, Params: gsip.HeaderParams{{K: "transport", V: d.transport}}})
		}
	}
	return resp
}

func (d *Dialog) interceptTransactionReponse(res *gsip.Response) {
	d.checkTransport(res.Transport())
	d.checkSource(res.Source())
	if d.State() == DialogStateEarly {
		// Should only be the initial INVITE response
		err := d.SetInviteResponse(res)
		if err != nil {
			d.log.Errorw("set invite response failed", err, "status", res.StatusCode)
		}
	}
}

func (d *Dialog) TransactionRequest(ctx context.Context, req *gsip.Request, options ...sipgo.ClientRequestOption) (gsip.ClientTransaction, error) {
	state := d.State()
	if state == DialogStateEnded || (state == DialogStateAllocated && req.Method != gsip.INVITE) {
		return nil, ErrInvalidDialogState
	}
	tx, err := d.client.TransactionRequest(ctx, req, options...)
	if req.Method == gsip.BYE {
		defer d.setState(DialogStateEnded)
	}
	if err != nil {
		return tx, err
	}
	if req.Method == gsip.INVITE && !d.inviteRequest {
		if err = d.SetInviteRequest(req, nil); err != nil { // Now that the request is fully fleshed out, set it on the dialog
			tx.Terminate()
			return tx, err
		}
		tx = NewDialogTransactionUAC(d, req, tx, d.interceptTransactionReponse) // intercept responses
	}
	return tx, nil
}

func (d *Dialog) WriteRequest(req *gsip.Request, options ...sipgo.ClientRequestOption) error {
	return d.client.WriteRequest(req, options...)
}

func BuildDialogInviteRequest(from gsip.Uri, to gsip.Uri, transport string) (*gsip.Request, error) {
	if from.Host == "" || to.Host == "" {
		return nil, ErrBadRequest
	}
	switch transport {
	case "udp":
	case "tcp":
	case "tls":
	case "":
		transport = "udp"
	default:
		return nil, ErrBadRequest
	}
	from.UriParams.Remove("transport")
	source := from.Clone()
	source.UriParams.Add("transport", transport)
	to.UriParams.Remove("transport")
	dest := to.Clone()
	dest.UriParams.Add("transport", transport)
	callID := gsip.CallIDHeader(uuid.New().String())

	// Basic request headers
	req := gsip.NewRequest(gsip.INVITE, *dest)
	req.SetTransport(transport)
	req.AppendHeader(&gsip.FromHeader{Address: from, Params: gsip.HeaderParams{{K: "tag", V: gsip.GenerateTagN(16)}}})
	req.AppendHeader(&gsip.ToHeader{Address: to})
	req.AppendHeader(&gsip.ContactHeader{Address: *source})
	req.AppendHeader(&gsip.CSeqHeader{SeqNo: 1, MethodName: gsip.INVITE}) // Don't override local CSeq stored
	req.AppendHeader(&callID)
	return req, nil
}

func validateMessageHeaders(r gsip.Message, requiredHeaders []string) error {
	missingHeaders := []string{}

	for _, header := range requiredHeaders {
		h := r.GetHeaders(header)
		if len(h) == 0 || h[0].Value() == "" {
			missingHeaders = append(missingHeaders, header)
		}
	}
	if len(missingHeaders) > 0 {
		return fmt.Errorf("missing required headers: %s", strings.Join(missingHeaders, ", "))
	}
	return nil
}
