// Copyright 2025 LiveKit, Inc.
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
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

const (
	testSIPWait     = 2 * time.Second
	testSIPSource   = "127.0.0.1:5060"
	testMinimalSDP  = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	testSIPTxBuffer = 16
)

// MockIOInfoClient is a no-op implementation of rpc.IOInfoClient for testing
type MockIOInfoClient struct{}

// Egress methods
func (m *MockIOInfoClient) CreateEgress(ctx context.Context, req *livekit.EgressInfo, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (m *MockIOInfoClient) UpdateEgress(ctx context.Context, req *livekit.EgressInfo, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (m *MockIOInfoClient) GetEgress(ctx context.Context, req *rpc.GetEgressRequest, opts ...psrpc.RequestOption) (*livekit.EgressInfo, error) {
	return nil, errors.New("not implemented")
}

func (m *MockIOInfoClient) ListEgress(ctx context.Context, req *livekit.ListEgressRequest, opts ...psrpc.RequestOption) (*livekit.ListEgressResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *MockIOInfoClient) UpdateMetrics(ctx context.Context, req *rpc.UpdateMetricsRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// Ingress methods
func (m *MockIOInfoClient) CreateIngress(ctx context.Context, req *livekit.IngressInfo, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (m *MockIOInfoClient) GetIngressInfo(ctx context.Context, req *rpc.GetIngressInfoRequest, opts ...psrpc.RequestOption) (*rpc.GetIngressInfoResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *MockIOInfoClient) UpdateIngressState(ctx context.Context, req *rpc.UpdateIngressStateRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// SIP methods
func (m *MockIOInfoClient) GetSIPTrunkAuthentication(ctx context.Context, req *rpc.GetSIPTrunkAuthenticationRequest, opts ...psrpc.RequestOption) (*rpc.GetSIPTrunkAuthenticationResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *MockIOInfoClient) EvaluateSIPDispatchRules(ctx context.Context, req *rpc.EvaluateSIPDispatchRulesRequest, opts ...psrpc.RequestOption) (*rpc.EvaluateSIPDispatchRulesResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *MockIOInfoClient) UpdateSIPCallState(ctx context.Context, req *rpc.UpdateSIPCallStateRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (m *MockIOInfoClient) RecordCallContext(ctx context.Context, req *rpc.RecordCallContextRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (m *MockIOInfoClient) Close() {
	// No-op for testing
}

// testRoom is a mock Room implementation that skips actual LiveKit connection
type testRoom struct {
	room *Room
}

var _ RoomInterface = (*testRoom)(nil)

type testRoomConfig struct {
	ringForever bool
}

func newTestRoomConfig(cfg *testRoomConfig) GetRoomFunc {
	return func(log logger.Logger, st *RoomStats) RoomInterface {
		return newTestRoomWithConfig(log, st, cfg)
	}
}

// newTestRoom creates a Room that skips actual LiveKit connection
func newTestRoomWithConfig(log logger.Logger, st *RoomStats, cfg *testRoomConfig) RoomInterface {
	if cfg == nil {
		cfg = &testRoomConfig{}
	}
	if st == nil {
		st = &RoomStats{}
	}
	// Create a Room with all the necessary structure but skip connection
	room := &Room{
		log:           log,
		stats:         st,
		outboundAudio: msdk.NewWriteCloserSwitch[msdk.PCM16Sample](RoomSampleRate),
		outboundDTMF:  msdk.NewWriteCloserSwitch[string](0),
		subscribe:     atomic.Bool{},
	}
	room.inboundDTMF = inboundDTMFWriter{room}

	// Create mixer
	var err error
	room.mix, err = mixer.NewMixer(room.outboundAudio, rtp.DefFrameDur, 1, mixer.WithStats(&st.Mixer), mixer.WithOutputChannel())
	if err != nil {
		panic(err)
	}

	roomLog, resolve := log.WithDeferredValues()
	room.roomLog = roomLog

	// Create a minimal lksdk.Room without connecting
	sdkRoom := lksdk.NewRoom(nil)
	room.room.Store(sdkRoom)

	// Set ready immediately (skip connection)
	room.ready.Break()
	if !cfg.ringForever {
		room.subscribed.Break()
	}
	resolve.Resolve()

	sdkRoom.OnRoomUpdate(&livekit.Room{ // Set metadata, and specifically Sid
		Name:            "test-room",
		Metadata:        "test-metadata",
		Sid:             "test-room-sid",
		NumParticipants: 1,
		NumPublishers:   1,
	})

	// Set up minimal participant info
	room.p.Store(&ParticipantInfo{
		ID:       "test-participant-id",
		RoomName: "test-room",
		Identity: "test-participant",
		Name:     "Test Participant",
	})

	return &testRoom{room: room}
}

// Connect overrides Room.Connect to skip actual LiveKit connection
func (r *testRoom) Connect(_ context.Context, conf *config.Config, rconf RoomConfig) error {
	// Update participant info from config
	partConf := rconf.Participant
	r.room.p.Store(&ParticipantInfo{
		RoomName: rconf.RoomName,
		Identity: partConf.Identity,
		Name:     partConf.Name,
	})
	// Skip actual connection - room is already set up
	return nil
}

// All other methods delegate to the embedded Room
func (r *testRoom) Closed() <-chan struct{} {
	return r.room.Closed()
}

func (r *testRoom) ClosedReason() livekit.DisconnectReason {
	return r.room.ClosedReason()
}

func (r *testRoom) Subscribed() <-chan struct{} {
	return r.room.Subscribed()
}

func (r *testRoom) Room() *lksdk.Room {
	return r.room.Room()
}

func (r *testRoom) Subscribe() {
	r.room.Subscribe()
}

func (r *testRoom) WriteOutboundAudioTo(w msdk.PCM16Writer) msdk.PCM16Writer {
	return r.room.WriteOutboundAudioTo(w)
}

func (r *testRoom) WriteOutboundDTMFTo(w msdk.WriteCloser[string]) msdk.WriteCloser[string] {
	return r.room.WriteOutboundDTMFTo(w)
}

func (r *testRoom) GetInboundAudioWriter() (msdk.PCM16Writer, error) {
	return r.NewParticipantTrack(RoomSampleRate)
}

func (r *testRoom) GetInboundDTMFWriter() msdk.WriteCloser[string] {
	return r.room.GetInboundDTMFWriter()
}

func (r *testRoom) Close() error {
	return r.room.Close()
}

func (r *testRoom) CloseWithReason(reason livekit.DisconnectReason) error {
	return r.room.CloseWithReason(reason)
}

func (r *testRoom) Participant() ParticipantInfo {
	return r.room.Participant()
}

func (r *testRoom) NewParticipantTrack(sampleRate int) (msdk.WriteCloser[msdk.PCM16Sample], error) {
	// For testing, we need to mock NewParticipantTrack since it requires a real LocalParticipant
	// which we don't have in our mock lksdk.Room. Return a no-op writer.
	return &noOpWriter{}, nil
}

// noOpWriter is a no-op implementation of msdk.WriteCloser for testing
type noOpWriter struct{}

func (w *noOpWriter) String() string {
	return "noOpWriter"
}

func (w *noOpWriter) SampleRate() int {
	return RoomSampleRate
}

func (w *noOpWriter) WriteSample(samples msdk.PCM16Sample) error {
	// No-op for testing
	return nil
}

func (w *noOpWriter) Close() error {
	return nil
}

func (r *testRoom) NewTrack() *mixer.Input {
	return r.room.NewTrack()
}

func (r *testRoom) RegisterRpcCtxMethod(method string, handler lksdk.RpcHandlerCtxFunc) error {
	return r.room.RegisterRpcCtxMethod(method, handler)
}

type testSIPClientTransaction struct {
	log       logger.Logger
	responses chan *sip.Response
	cancels   chan struct{}
	done      chan struct{}
	err       chan error
}

func (t *testSIPClientTransaction) Terminate() {
	t.log.Infow("Terminating transaction", "tx", fmt.Sprintf("%p", t))
	if t.responses != nil {
		close(t.responses)
		t.responses = nil
	}
	if t.cancels != nil {
		close(t.cancels)
		t.cancels = nil
	}
	if t.done != nil {
		close(t.done)
		t.done = nil
	}
	if t.err != nil {
		close(t.err)
		t.err = nil
	}
}

func (t *testSIPClientTransaction) Done() <-chan struct{} {
	return t.done
}

func (t *testSIPClientTransaction) Err() error {
	if t.err == nil {
		return nil
	}
	return <-t.err
}

func (t *testSIPClientTransaction) Responses() <-chan *sip.Response {
	return t.responses
}

func (t *testSIPClientTransaction) Cancel() error {
	select {
	case t.cancels <- struct{}{}:
		return nil
	default:
		return errors.New("cancel already sent")
	}
}

func (t *testSIPClientTransaction) SendResponse(resp *sip.Response) error {
	t.log.Infow("SIP Response sent on transaction", "tx", fmt.Sprintf("%p", t), "response", resp.String())
	select {
	case t.responses <- resp:
		return nil
	default:
		return errors.New("failed to add response")
	}
}

type transactionRequest struct {
	req         *sip.Request
	transaction *testSIPClientTransaction
	sequence    uint64
}

type sipRequest struct {
	req      *sip.Request
	sequence uint64
}

// Creates a utility for testing SIP correctness without going out on the network, local or otherwise.
// This is useful to isolate transport and routing tests (handled by sipgo.Client) from SIP logic.
//
// Works by mocking SIPClient interface, and providing tests with channels to listen for messages on.
// An interface mirroring sipgo.Client to be able to mock it in tests.
type testSIPClient struct {
	log      logger.Logger
	sequence atomic.Uint64

	mu                     sync.Mutex
	transactionByCallID    map[string][]*transactionRequest
	transactionBySipCallID map[string][]*transactionRequest
	requestByCallID        map[string][]*sipRequest
	requestBySipCallID     map[string][]*sipRequest
	wakeup                 chan struct{}
}

func (w *testSIPClient) FillRequestBlanks(req *sip.Request) {
	if req.Via() == nil {
		via := &sip.ViaHeader{
			ProtocolName:    "SIP",
			ProtocolVersion: "2.0",
			Transport:       req.Transport(),
			Host:            "127.0.0.1",
			Port:            5060,
			Params:          sip.NewParams(),
		}
		if via.Transport == "" {
			via.Transport = "UDP"
		}
		via.Params.Add("branch", sip.GenerateBranchN(16))
		req.PrependHeader(via)
	}
	if req.From() == nil {
		req.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: "caller", Host: "example.com"}})
	}
	if req.From().Params == nil {
		req.From().Params = sip.NewParams()
	}
	if _, ok := req.From().Params.Get("tag"); !ok {
		req.From().Params.Add("tag", sip.GenerateTagN(16))
	}
	if req.To() == nil {
		req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "callee", Host: "example.com"}})
	}
	if req.To().Params == nil {
		req.To().Params = sip.NewParams()
	}
	if req.CSeq() == nil {
		req.AppendHeader(&sip.CSeqHeader{
			SeqNo:      1,
			MethodName: req.Method,
		})
	}
	if req.CallID() == nil {
		calid := sip.CallIDHeader("test-call-" + sip.GenerateTagN(16))
		req.AppendHeader(&calid)
	}
	if req.MaxForwards() == nil {
		maxfwd := sip.MaxForwardsHeader(70)
		req.AppendHeader(&maxfwd)
	}
}

func (w *testSIPClient) deliverTx(txReq *transactionRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := w.wakeup
	w.wakeup = make(chan struct{})
	defer close(ch)
	form := txReq.req.From()
	if form == nil {
		panic("from header is required")
	}
	tag, ok := form.Params.Get("tag")
	if !ok {
		panic("tag is required")
	}
	sipCallID := txReq.req.CallID().Value()
	w.transactionByCallID[tag] = append(w.transactionByCallID[tag], txReq)
	w.transactionBySipCallID[sipCallID] = append(w.transactionBySipCallID[sipCallID], txReq)
}

func (w *testSIPClient) deliverReq(req *sipRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := w.wakeup
	w.wakeup = make(chan struct{})
	defer close(ch)
	form := req.req.From()
	if form == nil {
		panic("from header is required")
	}
	tag, ok := form.Params.Get("tag")
	if !ok {
		panic("tag is required")
	}
	sipCallID := req.req.CallID().Value()
	w.requestByCallID[tag] = append(w.requestByCallID[tag], req)
	w.requestBySipCallID[sipCallID] = append(w.requestBySipCallID[sipCallID], req)

}

func (w *testSIPClient) TransactionRequest(req *sip.Request, options ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	if len(options) > 0 {
		panic("options not supported for testSIPClient")
	}
	w.log.Infow("SIP TransactionRequest sent on client", "client", fmt.Sprintf("%p", w), "request", req.String())
	w.FillRequestBlanks(req)
	sequence := w.sequence.Add(1)
	tx := &testSIPClientTransaction{
		log:       w.log,
		responses: make(chan *sip.Response, testSIPTxBuffer),
		cancels:   make(chan struct{}),
		done:      make(chan struct{}),
		err:       make(chan error, 1),
	}
	txReq := &transactionRequest{
		sequence:    sequence,
		req:         req,
		transaction: tx,
	}
	w.deliverTx(txReq)
	return tx, nil
}

func (w *testSIPClient) WriteRequest(req *sip.Request, options ...sipgo.ClientRequestOption) error {
	if len(options) > 0 {
		panic("options not supported for testSIPClient")
	}
	w.log.Infow("SIP WriteRequest sent on client", "client", fmt.Sprintf("%p", w), "request", req.String())
	w.FillRequestBlanks(req)
	sequence := w.sequence.Add(1)
	reqReq := &sipRequest{
		sequence: sequence,
		req:      req,
	}
	w.deliverReq(reqReq)
	return nil
}

func (w *testSIPClient) removeTransactionLocked(txReqs []*transactionRequest) (*transactionRequest, bool) {
	if len(txReqs) == 0 {
		return nil, false
	}
	txReq := txReqs[0]
	callID := txReq.req.From().Params.GetOr("tag", "")
	sipCallID := txReq.req.CallID().Value()
	byCallID := w.transactionByCallID[callID]
	if len(byCallID) <= 0 {
		panic("callID not found")
	} else if txReq != byCallID[0] {
		panic("unexpected transaction request")
	}
	w.transactionByCallID[callID] = byCallID[1:]

	bySipCallID := w.transactionBySipCallID[sipCallID]
	if len(bySipCallID) <= 0 {
		panic("sipCallID not found")
	} else if txReq != bySipCallID[0] {
		panic("unexpected transaction request")
	}
	w.transactionBySipCallID[sipCallID] = bySipCallID[1:]
	return txReq, true
}

func (w *testSIPClient) removeRequestLocked(reqs []*sipRequest) (*sipRequest, bool) {
	if len(reqs) == 0 {
		return nil, false
	}
	req := reqs[0]
	callID := req.req.From().Params.GetOr("tag", "")
	sipCallID := req.req.CallID().Value()
	byCallID := w.requestByCallID[callID]
	if len(byCallID) <= 0 {
		panic("callID not found")
	} else if req != byCallID[0] {
		panic("unexpected transaction request")
	}
	if len(byCallID) == 1 {
		delete(w.requestByCallID, callID)
	} else {
		w.requestByCallID[callID] = byCallID[1:]
	}

	bySipCallID := w.requestBySipCallID[sipCallID]
	if len(bySipCallID) <= 0 {
		panic("sipCallID not found")
	} else if req != bySipCallID[0] {
		panic("unexpected transaction request")
	}
	if len(bySipCallID) == 1 {
		delete(w.requestBySipCallID, sipCallID)
	} else {
		w.requestBySipCallID[sipCallID] = bySipCallID[1:]
	}
	return req, true
}

func (w *testSIPClient) WaitTransactionTimeout(d time.Duration, callID string, sipCallID string) (*transactionRequest, error) {
	if callID == "" && sipCallID == "" {
		panic("callID or sipCallID is required")
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		w.mu.Lock()
		if w.wakeup == nil {
			panic("test client not closed")
		}
		if callID != "" {
			txReq, ok := w.removeTransactionLocked(w.transactionByCallID[callID])
			if ok {
				w.mu.Unlock()
				return txReq, nil
			}
		}
		if sipCallID != "" {
			txReq, ok := w.removeTransactionLocked(w.transactionBySipCallID[sipCallID])
			if ok {
				w.mu.Unlock()
				return txReq, nil
			}
		}
		wakeup := w.wakeup
		w.mu.Unlock()

		select {
		case <-timer.C:
			return nil, errors.New("timeout waiting for TransactionRequest")
		case <-wakeup:
			continue
		}
	}
}
func (w *testSIPClient) WaitRequestTimeout(d time.Duration, callID string, sipCallID string) (*sipRequest, error) {
	if callID == "" && sipCallID == "" {
		panic("callID or sipCallID is required")
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		w.mu.Lock()
		if w.wakeup == nil {
			panic("test client not closed")
		}
		if callID != "" {
			reqs, ok := w.removeRequestLocked(w.requestByCallID[callID])
			if ok {
				w.mu.Unlock()
				return reqs, nil
			}
		}
		if sipCallID != "" {
			reqs, ok := w.removeRequestLocked(w.requestBySipCallID[sipCallID])
			if ok {
				w.mu.Unlock()
				return reqs, nil
			}
		}
		wakeup := w.wakeup
		w.mu.Unlock()

		select {
		case <-timer.C:
			return nil, errors.New("timeout waiting for SIPRequest")
		case <-wakeup:
			continue
		}
	}
}

func (w *testSIPClient) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wakeup != nil {
		close(w.wakeup)
		w.wakeup = nil
	}
	return nil
}

var (
	_ SIPClient             = (*testSIPClient)(nil)
	_ sip.ClientTransaction = (*testSIPClientTransaction)(nil)
	_ sip.ServerTransaction = (*testSIPServerTransaction)(nil)
)

// testSIPServerTransaction fakes sipgo's ServerTransaction so tests can inject
// requests into package handlers and observe Respond, without a sipgo Server.
type testSIPServerTransaction struct {
	log           logger.Logger
	req           *sip.Request
	responses     chan *sip.Response
	acks          chan *sip.Request
	cancels       chan *sip.Request
	done          chan struct{}
	err           chan error
	terminateOnce sync.Once
}

func (t *testSIPServerTransaction) Terminate() {
	t.terminateOnce.Do(func() {
		t.log.Infow("Terminating server transaction", "tx", fmt.Sprintf("%p", t))
		close(t.done)
	})
}

func (t *testSIPServerTransaction) Done() <-chan struct{} {
	return t.done
}

func (t *testSIPServerTransaction) Err() error {
	if t.err == nil {
		return nil
	}
	return <-t.err
}

func (t *testSIPServerTransaction) Respond(res *sip.Response) error {
	t.log.Infow("SIP Respond on server transaction", "response", res.String())
	select {
	case <-t.done:
		return errors.New("transaction terminated")
	case t.responses <- res:
		return nil
	}
}

func (t *testSIPServerTransaction) Acks() <-chan *sip.Request {
	return t.acks
}

func (t *testSIPServerTransaction) Cancels() <-chan *sip.Request {
	return t.cancels
}

func (t *testSIPServerTransaction) SendAck(req *sip.Request) {
	if req == nil {
		req = sip.NewRequest(sip.ACK, sip.Uri{})
	}
	select {
	case <-t.done:
	case t.acks <- req:
	}
}

func (t *testSIPServerTransaction) SendCancel(req *sip.Request) {
	if req == nil {
		req = sip.NewRequest(sip.CANCEL, sip.Uri{})
	}
	select {
	case <-t.done:
	case t.cancels <- req:
	}
}

func (t *testSIPServerTransaction) WaitResponseTimeout(tb testing.TB, d time.Duration) *sip.Response {
	tb.Helper()
	select {
	case res, ok := <-t.responses:
		if !ok {
			tb.Fatal("server transaction closed while waiting for response")
			return nil
		}
		return res
	case <-time.After(d):
		tb.Fatalf("timeout waiting for SIP response after %s", d)
		return nil
	}
}

// TestSIPConfig holds configuration for creating a testSIPHarness fixture.
type TestSIPConfig struct {
	Region      string          // Defaults to "test"
	Config      *config.Config  // Creates minimal config if nil
	Monitor     *stats.Monitor  // Minimal monitor if nil
	GetIOClient GetStateHandler // MockIOInfoClient if nil
	GetRoom     GetRoomFunc     // newTestRoom if nil
	Handler     Handler         // empty TestHandler if nil
}

// testSIPHarness is a sipgo-less test fixture
// It allows testing orchestration logic without sipgo, and specifically
// without the need to work around certain peculiarities of the real thing
// Inbound requests are managed via testSIPServerTransaction
// Outbound requests are managed via testSIPClient
type testSIPHarness struct {
	log           logger.Logger
	Client        *Client
	Server        *Server
	client        *testSIPClient
	clientCreated atomic.Bool
}

// Wait for the package to send a SIP request to a remote endpoint
func (h *testSIPHarness) WaitTransaction(tb testing.TB, timeout time.Duration, callID string, sipCallID string) *transactionRequest {
	tb.Helper()
	res, err := h.client.WaitTransactionTimeout(timeout, callID, sipCallID)
	if err != nil {
		tb.Fatalf("error waiting for TransactionRequest: %v", err)
		return nil
	}
	return res
}

// Wait for the package to send a non-transaction SIP request to a remote endpoint
func (h *testSIPHarness) WaitRequest(tb testing.TB, timeout time.Duration, callID string, sipCallID string) *sipRequest {
	tb.Helper()
	res, err := h.client.WaitRequestTimeout(timeout, callID, sipCallID)
	if err != nil {
		tb.Fatalf("error waiting for Request: %v", err)
		return nil
	}
	return res
}

// Handle delivers req to the package as sipgo would: INVITE/ACK/BYE/NOTIFY/OPTIONS
// hit Server handlers, anything else falls through to Client.OnRequest then OnNoRoute.
// Dispatch runs in a goroutine because inbound Accept blocks until ACK.
func (h *testSIPHarness) Handle(req *sip.Request) *testSIPServerTransaction {
	if req.Source() == "" {
		req.SetSource(testSIPSource)
	}
	if req.Destination() == "" {
		req.SetDestination(testSIPSource)
	}
	tx := &testSIPServerTransaction{
		log:       h.log,
		req:       req,
		responses: make(chan *sip.Response, testSIPTxBuffer),
		acks:      make(chan *sip.Request, testSIPTxBuffer),
		cancels:   make(chan *sip.Request, testSIPTxBuffer),
		done:      make(chan struct{}),
		err:       make(chan error, 1),
	}
	go h.dispatch(req, tx)
	return tx
}

func (h *testSIPHarness) dispatch(req *sip.Request, tx sip.ServerTransaction) {
	log := slog.New(logger.ToSlogHandler(h.log))
	switch req.Method {
	case sip.INVITE:
		h.Server.onInvite(log, req, tx)
	case sip.ACK:
		h.Server.onAck(log, req, tx)
	case sip.BYE:
		h.Server.onBye(log, req, tx)
	case sip.NOTIFY:
		h.Server.onNotify(log, req, tx)
	case sip.OPTIONS:
		h.Server.onOptions(log, req, tx)
	default:
		if h.Client != nil && h.Client.OnRequest(req, tx) {
			return
		}
		h.Server.OnNoRoute(log, req, tx)
	}
}

func (h *testSIPHarness) newClient(ua *sipgo.UserAgent, options ...sipgo.ClientOption) (SIPClient, error) {
	if h.clientCreated.Swap(true) {
		panic("client must only be created once")
	}
	return h.client, nil
}

// NewTestSIP builds a test harness that replaces sipgo's client, server, and
// transport layers. sipgo's message and transaction types are still used.
//
// When package needs to be tested as the server, use Handle().
// When testing package client behavior, use WaitTransaction() or WaitRequest().
//
// NOTE: Most tests should use NewServiceTest.
// This utility and driver is only here for two edge cases:
// 1. Next-hop routing. If a message would be sent to a destination we cannot intercept.
// 2. Noncompliant messages & behavior sipgo will not send or accept.
func NewTestSIP(t testing.TB, cfg TestSIPConfig) *testSIPHarness {
	t.Helper()
	if cfg.Region == "" {
		cfg.Region = "test"
	}
	log := logger.NewTestLogger(t)
	if cfg.Config == nil {
		localIP, err := config.GetLocalIP()
		if err != nil {
			t.Fatalf("failed to get local IP: %v", err)
		}
		cfg.Config = &config.Config{
			NodeID:            "test-node",
			SIPPort:           5060,
			SIPPortListen:     5060,
			ListenIP:          localIP.String(),
			LocalNet:          localIP.String() + "/24",
			RTPPort:           rtcconfig.PortRange{Start: 20000, End: 30000},
			MaxCpuUtilization: 0.99, // Higher threshold for tests to avoid false positives
			WsUrl:             "ws://localhost:7880",
			ApiKey:            "test-api-key",
			ApiSecret:         "test-api-secret-extend-to-32-bytes-minimum",
		}
	}
	if cfg.Monitor == nil {
		var err error
		cfg.Monitor, err = stats.NewMonitor(cfg.Config)
		if err != nil {
			t.Fatalf("failed to create monitor: %v", err)
		}
		// Start the monitor so it reports healthy status
		if err := cfg.Monitor.Start(cfg.Config); err != nil {
			t.Fatalf("failed to start monitor: %v", err)
		}
		// Wait for CPU stats to initialize and health check to pass
		// The monitor samples CPU asynchronously, so we need to wait for the first sample
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if cfg.Monitor.Health() == stats.HealthOK {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Cleanup(func() {
			cfg.Monitor.Stop()
		})
	}
	if cfg.GetIOClient == nil {
		cfg.GetIOClient = func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) StateHandler {
			return NewRPCStateHandler(&MockIOInfoClient{})
		}
	}
	if cfg.GetRoom == nil {
		cfg.GetRoom = newTestRoomConfig(nil)
	}
	if cfg.Handler == nil {
		cfg.Handler = &TestHandler{}
	}

	h := &testSIPHarness{
		log: log,
		client: &testSIPClient{
			log:                    log,
			requestByCallID:        make(map[string][]*sipRequest),
			requestBySipCallID:     make(map[string][]*sipRequest),
			transactionByCallID:    make(map[string][]*transactionRequest),
			transactionBySipCallID: make(map[string][]*transactionRequest),
			wakeup:                 make(chan struct{}),
		},
	}

	client := NewClient(cfg.Region, cfg.Config, log, cfg.Monitor, cfg.GetIOClient, WithGetSipClient(h.newClient), WithGetRoomClient(cfg.GetRoom))
	client.SetHandler(cfg.Handler)

	// Set up service config with minimal values
	localIP, err := config.GetLocalIP()
	if err != nil {
		t.Fatalf("failed to get local IP: %v", err)
	}
	sconf := &ServiceConfig{
		SignalingIP:      localIP,
		SignalingIPLocal: localIP,
		MediaIP:          localIP,
	}

	err = client.Start(nil, sconf) // needed to set sconf
	if err != nil {
		t.Fatalf("failed to start client: %v", err)
	}
	t.Cleanup(func() {
		client.Stop()
	})

	srv := NewServer(cfg.Region, cfg.Config, log, cfg.Monitor, cfg.GetIOClient, WithGetRoomServer(cfg.GetRoom), WithClient(client))
	srv.SetHandler(cfg.Handler)
	srv.sconf = sconf
	srv.sipUnhandled = client.OnRequest
	t.Cleanup(srv.Stop)

	h.Client = client
	h.Server = srv
	return h
}

// NewOutboundTestClient starts a Client with the sipgo-less mock. Prefer NewTestSIP
// when the test needs to wait on transactions or inject inbound requests.
func NewOutboundTestClient(t testing.TB, cfg TestSIPConfig) *Client {
	return NewTestSIP(t, cfg).Client
}

// MinimalCreateSIPParticipantRequest creates a minimal valid request for testing.
// All required fields are set to test values.
func MinimalCreateSIPParticipantRequest() *rpc.InternalCreateSIPParticipantRequest {
	localIP, _ := config.GetLocalIP()
	return &rpc.InternalCreateSIPParticipantRequest{
		CallTo:              "+1234567890",
		Address:             "sip.example.com",
		Number:              "+0987654321",
		Hostname:            localIP.String(),
		RoomName:            guid.New(guid.RoomPrefix + "TEST_"),
		ParticipantIdentity: "test-participant",
		ParticipantName:     "Test Participant",
		SipCallId:           guid.New(guid.SIPCallPrefix + "TEST_"),
		Transport:           livekit.SIPTransport_SIP_TRANSPORT_UDP,
		WsUrl:               "ws://localhost:7880",
		Token:               "test-token",
	}
}

// MinimalInviteRequest builds a UDP INVITE the inbound handlers will accept.
func MinimalInviteRequest() *sip.Request {
	to := sip.Uri{User: "+1234567890", Host: "sip.example.com", Port: 5060}
	from := sip.Uri{User: "+0987654321", Host: "127.0.0.1", Port: 5060}
	req := sip.NewRequest(sip.INVITE, to)
	fromH := &sip.FromHeader{Address: from, Params: sip.NewParams()}
	fromH.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(fromH)
	req.AppendHeader(&sip.ToHeader{Address: to})
	req.AppendHeader(&sip.ContactHeader{Address: from})
	cid := sip.CallIDHeader("test-call-" + sip.GenerateTagN(16))
	req.AppendHeader(&cid)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.INVITE})
	via := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "UDP",
		Host:            "127.0.0.1",
		Port:            5060,
		Params:          sip.NewParams(),
	}
	via.Params.Add("branch", "z9hG4bK"+sip.GenerateTagN(16))
	req.AppendHeader(via)
	maxfwd := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxfwd)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody([]byte(testMinimalSDP))
	req.SetSource(testSIPSource)
	req.SetDestination(testSIPSource)
	return req
}
