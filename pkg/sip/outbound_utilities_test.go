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
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
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

// newTestRoom creates a Room that skips actual LiveKit connection
func newTestRoom(log logger.Logger, st *RoomStats) RoomInterface {
	if st == nil {
		st = &RoomStats{}
	}
	// Create a Room with all the necessary structure but skip connection
	room := &Room{
		log:       log,
		stats:     st,
		out:       msdk.NewSwitchWriter(RoomSampleRate),
		subscribe: atomic.Bool{},
	}

	// Create mixer
	var err error
	room.mix, err = mixer.NewMixer(room.out, rtp.DefFrameDur, 1, mixer.WithStats(&st.Mixer), mixer.WithOutputChannel())
	if err != nil {
		panic(err)
	}

	roomLog, resolve := log.WithDeferredValues()
	room.roomLog = roomLog

	// Create a minimal lksdk.Room without connecting
	room.room = lksdk.NewRoom(nil)

	// Set ready immediately (skip connection)
	room.ready.Break()
	resolve.Resolve()

	room.room.OnRoomUpdate(&livekit.Room{ // Set metadata, and specifically Sid
		Name:            "test-room",
		Metadata:        "test-metadata",
		Sid:             "test-room-sid",
		NumParticipants: 1,
		NumPublishers:   1,
	})

	// Set up minimal participant info
	room.p = ParticipantInfo{
		ID:       "test-participant-id",
		RoomName: "test-room",
		Identity: "test-participant",
		Name:     "Test Participant",
	}

	return &testRoom{room: room}
}

// Connect overrides Room.Connect to skip actual LiveKit connection
func (r *testRoom) Connect(conf *config.Config, rconf RoomConfig) error {
	// Update participant info from config
	partConf := rconf.Participant
	r.room.p = ParticipantInfo{
		RoomName: rconf.RoomName,
		Identity: partConf.Identity,
		Name:     partConf.Name,
	}
	// Skip actual connection - room is already set up
	return nil
}

// All other methods delegate to the embedded Room
func (r *testRoom) Closed() <-chan struct{} {
	return r.room.Closed()
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

func (r *testRoom) Output() msdk.Writer[msdk.PCM16Sample] {
	return r.room.Output()
}

func (r *testRoom) SwapOutput(out msdk.PCM16Writer) msdk.PCM16Writer {
	return r.room.SwapOutput(out)
}

func (r *testRoom) CloseOutput() error {
	return r.room.CloseOutput()
}

func (r *testRoom) SetDTMFOutput(w dtmf.Writer) {
	r.room.SetDTMFOutput(w)
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

func (r *testRoom) SendData(data lksdk.DataPacket, opts ...lksdk.DataPublishOption) error {
	return r.room.SendData(data, opts...)
}

func (r *testRoom) NewTrack() *mixer.Input {
	return r.room.NewTrack()
}

type testSIPClientTransaction struct {
	responses chan *sip.Response
	cancels   chan struct{}
	done      chan struct{}
	err       chan error
}

func (t *testSIPClientTransaction) Terminate() {
	fmt.Println("Terminating transaction!!")
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
	fmt.Printf("SIP Response sent on transaction %v:\n%s\n", t, resp.String())
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
	client       *sipgo.Client
	requests     chan *sipRequest
	transactions chan *transactionRequest
	sequence     uint64
}

func (w *testSIPClient) FillRequestBlanks(req *sip.Request) {
	sipgo.ClientRequestAddVia(w.client, req)
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

func (w *testSIPClient) TransactionRequest(req *sip.Request, options ...sipgo.ClientRequestOption) (sip.ClientTransaction, error) {
	if len(options) > 0 {
		panic("options not supported for testSIPClient")
	}
	fmt.Printf("SIP TransactionRequest sent on client %v:\n%s\n", w, req.String())
	w.FillRequestBlanks(req)
	w.sequence++
	tx := &testSIPClientTransaction{
		responses: make(chan *sip.Response),
		cancels:   make(chan struct{}),
		done:      make(chan struct{}),
		err:       make(chan error),
	}
	txReq := &transactionRequest{
		sequence:    w.sequence,
		req:         req,
		transaction: tx,
	}
	select {
	case w.transactions <- txReq:
		return tx, nil
	default:
		return nil, errors.New("failed to add transaction request")
	}
}

func (w *testSIPClient) WriteRequest(req *sip.Request, options ...sipgo.ClientRequestOption) error {
	if len(options) > 0 {
		panic("options not supported for testSIPClient")
	}
	fmt.Printf("SIP WriteRequest sent on client %v:\n%s\n", w, req.String())
	w.FillRequestBlanks(req)
	w.sequence++
	reqReq := &sipRequest{
		sequence: w.sequence,
		req:      req,
	}
	select {
	case w.requests <- reqReq:
		return nil
	default:
		return errors.New("failed to add request")
	}
}

func (w *testSIPClient) Close() error {
	if w.requests != nil {
		close(w.requests)
		w.requests = nil
	}
	if w.transactions != nil {
		close(w.transactions)
		w.transactions = nil
	}
	return w.client.Close()
}

var createdClients = make(chan *testSIPClient, 10)

func NewTestClientFunc(ua *sipgo.UserAgent, options ...sipgo.ClientOption) (SIPClient, error) {
	client, err := sipgo.NewClient(ua, options...)
	if err != nil {
		return nil, err
	}
	testClient := &testSIPClient{
		client:       client,
		requests:     make(chan *sipRequest, 10),         // Buffered to avoid blocking
		transactions: make(chan *transactionRequest, 10), // Buffered to avoid blocking
	}

	select {
	case createdClients <- testClient:
		return testClient, nil
	default:
		return nil, errors.New("failed to add test client")
	}
}

// TestClientConfig holds configuration for creating a test Client
type TestClientConfig struct {
	Region       string           // Defaults to "test"
	Config       *config.Config   // Creates minimal config if nil
	Monitor      *stats.Monitor   // Minimal monitor if nil
	GetIOClient  GetIOInfoClient  // MockIOInfoClient if nil
	GetSipClient GetSipClientFunc // NewTestClientFunc if nil
	GetRoom      GetRoomFunc      // newTestRoom if nil
}

func NewOutboundTestClient(t testing.TB, cfg TestClientConfig) *Client {
	if cfg.Region == "" {
		cfg.Region = "test"
	}
	zlogger, err := logger.NewZapLogger(&logger.Config{
		Level: "debug",
		JSON:  false, // Use console format, not JSON
	})
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
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
			RTPPort:           rtcconfig.PortRange{Start: 20000, End: 20010},
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
		cfg.GetIOClient = func(projectID string) rpc.IOInfoClient {
			return &MockIOInfoClient{}
		}
	}
	if cfg.GetSipClient == nil {
		cfg.GetSipClient = NewTestClientFunc
	}
	if cfg.GetRoom == nil {
		cfg.GetRoom = newTestRoom
	}
	client := NewClient(cfg.Region, cfg.Config, zlogger, cfg.Monitor, cfg.GetIOClient, WithGetSipClient(cfg.GetSipClient), WithGetRoomClient(cfg.GetRoom))
	client.SetHandler(&TestHandler{})

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
	return client
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
		RoomName:            "test-room",
		ParticipantIdentity: "test-participant",
		ParticipantName:     "Test Participant",
		SipCallId:           "test-call-id",
		Transport:           livekit.SIPTransport_SIP_TRANSPORT_UDP,
		WsUrl:               "ws://localhost:7880",
		Token:               "test-token",
	}
}
