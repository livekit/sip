package sip

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"errors"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type sipUATestOption func(*sipUATest)

func withUATestBuffer(buffer uint) sipUATestOption {
	return func(s *sipUATest) {
		s.bufferSize = buffer
	}
}

func withUATestPort(port uint16) sipUATestOption {
	return func(s *sipUATest) {
		s.localAddr = netip.AddrPortFrom(s.localAddr.Addr(), port)
	}
}

func newUATest(t *testing.T, log logger.Logger, remote netip.AddrPort, options ...sipUATestOption) *sipUATest {
	t.Helper()

	ua := &sipUATest{
		log:        log,
		remoteAddr: remote,
		localAddr:  netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0),
	}
	for _, option := range options {
		option(ua)
	}
	ua.sinks = make(map[string]map[string]chan *sipUARequest)

	var err error
	ua.UA, err = sipgo.NewUA(
		sipgo.WithUserAgent("from@test"),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(log))),
	)
	require.NoError(t, err)
	t.Cleanup(func() { ua.UA.Close() })

	ua.Server, err = sipgo.NewServer(ua.UA)
	require.NoError(t, err)
	ua.Server.OnNoRoute(ua.onRequest)
	t.Cleanup(func() { ua.Server.Close() })

	err = ua.serveTransport(t, "tcp")
	require.NoError(t, err)
	err = ua.serveTransport(t, "udp")
	require.NoError(t, err)

	// Should be after serveTransport to know the local port for sure
	ua.Client, err = sipgo.NewClient(ua.UA)
	require.NoError(t, err)
	t.Cleanup(func() { ua.Client.Close() })

	t.Cleanup(ua.CloseSinks)
	return ua
}

type sipUARequest struct {
	req *sip.Request
	tx  sip.ServerTransaction
}

type sipUATest struct {
	bufferSize uint
	log        logger.Logger

	mu    sync.Mutex
	sinks map[string]map[string]chan *sipUARequest

	UA     *sipgo.UserAgent
	Server *sipgo.Server
	Client *sipgo.Client

	remoteAddr netip.AddrPort // Address of the server under test (INVITE Request-URI / destination)
	localAddr  netip.AddrPort // Address where this UA's sipgo.Server listens (TCP/UDP)
}

func (s *sipUATest) onRequest(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
	method := req.Method.String()
	var callID string
	if hdr := req.CallID(); hdr != nil {
		callID = hdr.Value()
	}
	var cseq uint32
	if hdr := req.CSeq(); hdr != nil {
		cseq = hdr.SeqNo
	}
	var toTag string
	if hdr := req.To(); hdr != nil {
		toTag = hdr.Params.GetOr("tag", "")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dialogMethods, ok := s.sinks[toTag]
	if !ok {
		dialogMethods, ok = s.sinks[""]
	}
	if !ok {
		s.log.Infow("No listeners found, ignoring", "callID", callID, "cseq", cseq, "method", method, "toTag", toTag)
		return
	}

	ch, ok := dialogMethods[method]
	if !ok {
		ch, ok = dialogMethods[""]
	}
	if !ok {
		s.log.Infow("No listeners found, ignoring", "callID", callID, "cseq", cseq, "method", method, "toTag", toTag)
		return
	}
	if ch == nil { // Closed, just stop
		return
	}
	msg := &sipUARequest{req: req, tx: tx}
	if s.bufferSize > 0 {
		select {
		case ch <- msg:
			// OK
		default:
			s.log.Infow("Request buffer full, dropping", "callID", callID, "cseq", cseq, "method", method)
		}
	} else {
		go func(msg *sipUARequest) {
			ch <- msg
		}(msg)
	}
}

func (s *sipUATest) serveTransport(t *testing.T, transport string) error {
	addr := s.localAddr.String()
	switch transport {
	case "tcp":
		tcpL, err := net.Listen("tcp", addr)
		require.NoError(t, err)
		if s.localAddr.Port() == 0 {
			tcpAddr, ok := tcpL.Addr().(*net.TCPAddr)
			if !ok {
				return errors.New("address is not a TCP address")
			}
			s.localAddr = netip.AddrPortFrom(netip.MustParseAddr(tcpAddr.IP.String()), uint16(tcpAddr.Port))
		}
		t.Cleanup(func() { tcpL.Close() })
		go func() {
			err := s.Server.ServeTCP(tcpL)
			if err != nil && !errors.Is(err, net.ErrClosed) {
				s.log.Errorw("sip TCP server failed", err, "transport", transport, "addr", addr)
			}
		}()
	case "udp":
		udpAddr := &net.UDPAddr{IP: s.localAddr.Addr().AsSlice(), Port: int(s.localAddr.Port())}
		udpConn, err := net.ListenUDP("udp", udpAddr)
		require.NoError(t, err)
		if s.localAddr.Port() == 0 {
			localAddr, ok := udpConn.LocalAddr().(*net.UDPAddr)
			if !ok {
				return errors.New("address is not a UDP address")
			}
			s.localAddr = netip.AddrPortFrom(netip.MustParseAddr(localAddr.IP.String()), uint16(localAddr.Port))
		}
		t.Cleanup(func() { udpConn.Close() })
		go func() {
			err := s.Server.ServeUDP(udpConn)
			if err != nil && !errors.Is(err, net.ErrClosed) {
				s.log.Errorw("sip UDP server failed", err, "transport", transport, "addr", addr)
			}
		}()
	}
	return nil
}

func (s *sipUATest) RegisterSink(localTag string, method string) <-chan *sipUARequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	dialogMethods, ok := s.sinks[localTag]
	if !ok {
		dialogMethods = make(map[string]chan *sipUARequest)
		s.sinks[localTag] = dialogMethods
	}
	sink, ok := dialogMethods[method]
	if !ok {
		sink = make(chan *sipUARequest, s.bufferSize)
		dialogMethods[method] = sink
	}
	return sink
}

func (s *sipUATest) UnregisterSink(localTag string, method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dialogMethods, ok := s.sinks[localTag]
	if !ok {
		return false
	}
	sink, ok := dialogMethods[method]
	if !ok {
		return false
	}
	close(sink)
	delete(dialogMethods, method)
	return true
}

func (s *sipUATest) CloseSinks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for localTag, dialogMethods := range s.sinks {
		for method, sink := range dialogMethods {
			close(sink)
			delete(dialogMethods, method)
		}
		delete(s.sinks, localTag)
	}
}

func (s *sipUATest) Address() netip.AddrPort {
	return s.remoteAddr
}

func (s *sipUATest) LocalURI() *sip.Uri {
	return &sip.Uri{
		Scheme: "sip",
		Host:   s.localAddr.Addr().String(),
		Port:   int(s.localAddr.Port()),
	}
}

func (s *sipUATest) NewRequest(method sip.RequestMethod, fromUser string, toUser string, callID string, cseq uint32) *sip.Request {
	remoteURI := sip.Uri{Scheme: "sip", User: toUser, Host: s.remoteAddr.String()}
	localURI := sip.Uri{Scheme: "sip", User: fromUser, Host: s.localAddr.Addr().String(), Port: int(s.localAddr.Port())}

	req := sip.NewRequest(method, remoteURI)
	fromTag := sip.GenerateTagN(16)
	req.AppendHeader(&sip.FromHeader{
		Address: localURI,
		Params:  sip.HeaderParams{{K: "tag", V: fromTag}},
	})
	req.AppendHeader(&sip.ToHeader{
		Address: remoteURI,
	})
	req.AppendHeader(&sip.ContactHeader{
		Address: localURI,
	})
	callIDHdr := sip.CallIDHeader(callID)
	req.AppendHeader(&callIDHdr)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	req.SetDestination(s.remoteAddr.String())
	return req
}

func (s *sipUATest) TransactionRequest(t *testing.T, req *sip.Request, isFromUAC bool) *sip.Response {
	t.Helper()
	tx, err := s.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	resp := getFinalResponseOrFail(t, tx, req)
	if req.Method == sip.INVITE && resp.StatusCode < 300 {
		// Need to send ACK for 2xx INVITE, sipgo already sends ACK for 3xx+
		ack := sip.NewAckRequest(req, resp, nil)
		err = s.Client.WriteRequest(ack)
		require.NoError(t, err)
	}
	return resp
}

var index atomic.Int32

type sipUADialogTest struct {
	TestUA     *sipUATest
	isUAS      bool
	callID     string
	localUser  string
	remoteUser string
	localTag   string
	remoteTag  LocalTag
	localCseq  uint32
	remoteCseq uint32
	remoteSDP  []byte
	localSDP   []byte
	routeSet   []sip.Uri
}

func newTestCall(testUA *sipUATest, isUAS bool) *sipUADialogTest {
	i := index.Add(1)
	d := &sipUADialogTest{
		TestUA:     testUA,
		isUAS:      isUAS,
		callID:     fmt.Sprintf("callID-%d", i),
		localUser:  fmt.Sprintf("localTestUser-%d", i),
		remoteUser: fmt.Sprintf("remoteTestUser-%d", i),
		localTag:   fmt.Sprintf("localTestTag-%d", i),
		remoteTag:  "", // Needs to be set by remote
		localCseq:  1,
		remoteCseq: 0, // Needs to be set by remote
	}
	if isUAS {
		// sipUADialogTest generates CreateSipParticipantRequest, which needs the tag as input.
		d.remoteTag = LocalTag(fmt.Sprintf("remoteTestTag-%d", i))
	}
	return d
}

func (d *sipUADialogTest) SetRemoteTag(tag LocalTag) {
	d.remoteTag = tag
}

func (d *sipUADialogTest) SetRemoteSDP(sdp []byte) {
	d.remoteSDP = sdp
}

func (d *sipUADialogTest) SetLocalSDP(sdp []byte) {
	d.localSDP = sdp
}

func (d *sipUADialogTest) SetRouteSet(resp *sip.Response, isUAC bool) {
	rrHeaders := resp.GetHeaders("Record-Route")
	routeSet := make([]sip.Uri, 0, len(rrHeaders))
	for _, hdr := range rrHeaders {
		rr, ok := hdr.(*sip.RecordRouteHeader)
		if ok {
			routeSet = append(routeSet, rr.Address)
		}

	}
	if isUAC {
		slices.Reverse(routeSet)
	}
	d.routeSet = routeSet
}

func (d *sipUADialogTest) NewRequest(method sip.RequestMethod) *sip.Request {
	req := d.TestUA.NewRequest(method, d.localUser, d.remoteUser, d.callID, d.localCseq)
	d.localCseq++
	req.From().Params.Add("tag", string(d.localTag))
	if d.remoteTag != "" {
		req.To().Params.Add("tag", string(d.remoteTag))
	}
	if len(d.routeSet) > 0 {
		for _, uri := range d.routeSet {
			req.AppendHeader(&sip.RouteHeader{Address: uri})
		}
	}
	return req
}

func (d *sipUADialogTest) Invite(offer []byte) (*sip.Request, []byte, error) {
	if offer == nil {
		sdpOffer, err := sdp.NewOfferWith(defaultCodecs, d.TestUA.localAddr.Addr(), 0xB0B, sdp.EncryptionNone)
		if err != nil {
			return nil, nil, err
		}
		offer, err = sdpOffer.SDP.Marshal()
		if err != nil {
			return nil, nil, err
		}
	}
	req := d.NewRequest(sip.INVITE)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(offer)
	return req, offer, nil
}

func (d *sipUADialogTest) CreateSipParticipantRequest() *rpc.InternalCreateSIPParticipantRequest {
	req := MinimalCreateSIPParticipantRequest()
	req.CallTo = d.localUser
	req.Address = d.TestUA.localAddr.String()
	req.Number = d.remoteUser
	req.Hostname = d.TestUA.remoteAddr.Addr().String()
	req.SipCallId = string(d.remoteTag)
	return req
}

func (d *sipUADialogTest) TransactionRequest(t *testing.T, req *sip.Request) *sip.Response {
	return d.TestUA.TransactionRequest(t, req, !d.isUAS)
}

func (d *sipUADialogTest) RegisterRequestChannel(method string) <-chan *sipUARequest {
	return d.TestUA.RegisterSink(d.localTag, method)
}

func (d *sipUADialogTest) UnregisterRequestChannel(method string) {
	d.TestUA.UnregisterSink(d.localTag, method)
}

type serviceTest struct {
	TestUA  *sipUATest
	Server  *Server // Processing inbound calls and SIP
	Client  *Client // Processing outbound calls
	Handler Handler
	Service *Service
}

type serviceTestConfig struct {
	GetRoom GetRoomFunc
}

func NewServiceTest(t *testing.T, options *serviceTestConfig) *serviceTest {
	t.Helper()

	if options == nil {
		options = &serviceTestConfig{}
	}
	if options.GetRoom == nil {
		options.GetRoom = newTestRoomConfig(nil)
	}

	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	loopback := netip.MustParseAddr("127.0.0.1")

	conf := &config.Config{
		NodeID:             "test-node",
		MaxCpuUtilization:  0.9,
		SIPPort:            sipPort,
		SIPPortListen:      sipPort,
		RTPPort:            rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
		SIPRingingInterval: time.Second,
		// wsUrl:              "ws://localhost:7880",
		// ApiKey:             "test",
		// ApiSecret:          strings.Repeat("k", 32),
	}
	mon, err := stats.NewMonitor(conf)
	require.NoError(t, err)
	require.NoError(t, mon.Start(conf), "start monitor so metrics (e.g. inviteReqRaw) are registered")
	t.Cleanup(func() { mon.Stop() })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mon.Health() == stats.HealthOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, stats.HealthOK, mon.Health(), "monitor should be healthy")
	log := logger.NewTestLogger(t)

	cli := NewClient(
		"",
		conf,
		log,
		mon,
		func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) StateHandler {
			return NewRPCStateHandler(&MockIOInfoClient{})
		},
		WithGetRoomClient(options.GetRoom),
	)
	srv := NewServer(
		"",
		conf,
		log,
		mon,
		func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) StateHandler {
			return NewRPCStateHandler(&MockIOInfoClient{})
		},
		WithGetRoomServer(options.GetRoom),
		WithClient(cli),
	)
	require.NotNil(t, srv)

	sconf := &ServiceConfig{
		SignalingIP:      loopback,
		SignalingIPLocal: loopback,
		MediaIP:          loopback,
	}

	handler := &TestHandler{}

	err = srv.Start(nil, sconf, nil, cli.OnRequest)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)
	srv.SetHandler(handler)

	err = cli.Start(nil, sconf)
	require.NoError(t, err)
	t.Cleanup(cli.Stop)
	cli.SetHandler(handler)

	addr := netip.AddrPortFrom(loopback, uint16(sipPort))

	service := &Service{
		conf:             conf,
		log:              log,
		mon:              mon,
		cli:              cli,
		srv:              srv,
		pendingTransfers: make(map[LocalTag]*PendingTransfer),
	}
	return &serviceTest{
		TestUA:  newUATest(t, srv.log, addr, withUATestBuffer(3)),
		Server:  srv,
		Client:  cli,
		Handler: handler,
		Service: service,
	}
}

func (st *serviceTest) Address() string {
	return st.TestUA.Address().String()
}

type createCallTestOption func(req *sip.Request, resp *sip.Response)

func withTestHeaders(headers ...sip.Header) createCallTestOption {
	return func(req *sip.Request, resp *sip.Response) {
		if req != nil {
			for _, header := range headers {
				req.AppendHeader(sip.HeaderClone(header))
			}
		}
	}
}

func (st *serviceTest) CreateInboundCall(t *testing.T, opts ...createCallTestOption) (*sipUADialogTest, *inboundCall) {
	t.Helper()

	call := newTestCall(st.TestUA, false)
	req, localSDP, err := call.Invite(nil)
	require.NoError(t, err)
	for _, opt := range opts {
		opt(req, nil)
	}
	call.SetLocalSDP(localSDP)
	resp := st.TestUA.TransactionRequest(t, req, true)
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")
	remoteTag, ok := resp.To().Params.Get("tag")
	require.True(t, ok, "remote tag should be present")
	call.SetRemoteTag(LocalTag(remoteTag))
	call.SetRemoteSDP(resp.Body())
	call.SetRouteSet(resp, true)

	st.Server.cmu.Lock()
	defer st.Server.cmu.Unlock()
	ic, ok := st.Server.byLocalTag[call.remoteTag]
	require.True(t, ok, "call should be registered")

	t.Logf("inbound call: %+v", call)
	return call, ic
}

// CreateOutboundCall registers a fake outbound call so that a re-INVITE with the given localTag (To tag)
// is accepted as outbound reinvite and answered with sdpOffer.
func (st *serviceTest) CreateOutboundCall(t *testing.T, opts ...createCallTestOption) (*sipUADialogTest, *outboundCall, *sip.Request) {
	t.Helper()

	newInviteSink := st.TestUA.RegisterSink("", "INVITE")
	var reqSink <-chan *sipUARequest
	defer st.TestUA.UnregisterSink("", "INVITE")

	call := newTestCall(st.TestUA, true)
	req := call.CreateSipParticipantRequest()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := st.Client.CreateSIPParticipant(ctx, req)
	require.NoError(t, err)

	select {
	case msg := <-newInviteSink:
		require.NotNil(t, msg, "unexpected nil message")
		st.TestUA.UnregisterSink("", "INVITE")

		require.Equal(t, string(call.remoteTag), msg.req.From().Params.GetOr("tag", ""), "remote tag should be the same")
		require.Equal(t, call.remoteUser, msg.req.From().Address.User, "remote user should be the same")
		require.Equal(t, call.localUser, msg.req.To().Address.User, "local user should be the same")

		for _, opt := range opts {
			opt(msg.req, nil) // Simulate added headers
		}

		offer, err := sdp.ParseOfferWith(defaultCodecs, msg.req.Body())
		require.NoError(t, err)
		sdpAnswer, _, err := offer.Answer(netip.MustParseAddr("4.3.2.1"), 0xB00, sdp.EncryptionNone)
		require.NoError(t, err)
		answerBytes, err := sdpAnswer.SDP.Marshal()
		require.NoError(t, err)
		resp := sip.NewResponseFromRequest(msg.req, sip.StatusOK, "OK", answerBytes)
		resp.To().Params.Add("tag", call.localTag)

		call.callID = msg.req.CallID().Value()
		call.remoteCseq = msg.req.CSeq().SeqNo
		call.SetRemoteSDP(msg.req.Body())
		call.SetLocalSDP(resp.Body())
		call.SetRouteSet(resp, false)
		reqSink = st.TestUA.RegisterSink(call.localTag, "")
		err = msg.tx.Respond(resp)
		require.NoError(t, err)
	case <-ctx.Done():
		require.Fail(t, "timeout waiting for invite")
	}

	var ackReq *sip.Request
	select {
	case msg := <-reqSink:
		require.NotNil(t, msg, "unexpected nil message")
		require.Equal(t, string(sip.ACK), string(msg.req.Method), "Expecting ACK")
		require.Equal(t, call.remoteCseq, msg.req.CSeq().SeqNo, "remote cseq should be the same")
		ackReq = msg.req
	case <-ctx.Done():
		require.Fail(t, "timeout waiting for ACK")
	}

	st.Client.cmu.Lock()
	defer st.Client.cmu.Unlock()
	oc, ok := st.Client.activeCalls[call.remoteTag]
	require.True(t, ok, "call should be registered")

	t.Logf("outbound call: %+v", call)
	return call, oc, ackReq
}

func TestReinvite(t *testing.T) {
	t.Run("inbound", func(t *testing.T) {
		t.Run("normal", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			call, _ := st.CreateInboundCall(t)
			serverLocalSDP := call.remoteSDP

			// Re-INVITE
			req, _, err := call.Invite(call.localSDP)
			require.NoError(t, err)
			resp := st.TestUA.TransactionRequest(t, req, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")

			// Re-INVITE with new offer
			newOffer, err := sdp.NewOfferWith(defaultCodecs, netip.MustParseAddr("9.8.7.6"), 12345, sdp.EncryptionNone)
			require.NoError(t, err)
			newOfferBytes, err := newOffer.SDP.Marshal()
			require.NoError(t, err)
			req, _, err = call.Invite(newOfferBytes)
			require.NoError(t, err)
			resp = st.TestUA.TransactionRequest(t, req, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")
		})

		t.Run("miss", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			call, _ := st.CreateInboundCall(t)
			serverLocalSDP := call.remoteSDP

			// Re-INVITE
			req, _, err := call.Invite(call.localSDP)
			require.NoError(t, err)
			resp := st.TestUA.TransactionRequest(t, req, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")

			// re-INVITE with different tag
			call.remoteTag = "something-else"
			req, _, err = call.Invite(call.localSDP)
			require.NoError(t, err)
			resp = st.TestUA.TransactionRequest(t, req, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.NotEqual(t, serverLocalSDP, resp.Body(), "reinvite for new call should return new server local SDP")
		})
	})
	t.Run("outbound", func(t *testing.T) {
		t.Run("normal", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			call, oc, _ := st.CreateOutboundCall(t)
			serverLocalSDP := oc.cc.LocalSDP()
			require.NotEqual(t, call.localSDP, serverLocalSDP, "local and remote SDP should be different")

			// Re-INVITE
			req, _, err := call.Invite(call.localSDP)
			require.NoError(t, err)
			resp := st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")

			// Re-INVITE with new offer
			newOffer, err := sdp.NewOfferWith(defaultCodecs, netip.MustParseAddr("9.8.7.6"), 12345, sdp.EncryptionNone)
			require.NoError(t, err)
			newOfferBytes, err := newOffer.SDP.Marshal()
			require.NoError(t, err)
			req, _, err = call.Invite(newOfferBytes)
			require.NoError(t, err)
			resp = st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")
		})

		t.Run("miss", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			call, oc, _ := st.CreateOutboundCall(t)
			serverLocalSDP := oc.cc.LocalSDP()

			// Re-INVITE
			req, _, err := call.Invite(call.localSDP)
			require.NoError(t, err)
			resp := st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")

			// re-INVITE with different tag
			call.remoteTag = "something-else"
			req, _, err = call.Invite(call.localSDP)
			require.NoError(t, err)
			resp = st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
			require.NotEqual(t, serverLocalSDP, resp.Body(), "reinvite for new call should return new server local SDP")
		})
	})
}

func TestTransfer(t *testing.T) {
	const referTo = "tel:+15551234567"

	expectHeaders := func(referTo string, headers map[string]string) []sip.Header {
		expected := []sip.Header{sip.NewHeader("Refer-To", fmt.Sprintf("<%s>", referTo))}
		for name, value := range headers {
			expected = append(expected, sip.NewHeader(name, value))
		}
		return expected
	}

	handleRefer := func(t *testing.T, ctx context.Context, refChan <-chan *sipUARequest, call *sipUADialogTest, referStatus int, validateHeaders []sip.Header) error {
		t.Helper()
		select {
		case <-ctx.Done():
			return fmt.Errorf("test aborted without receiving a REFER request: %v", ctx.Err())
		case msg := <-refChan:
			require.NotNil(t, msg)
			require.Equal(t, sip.REFER, msg.req.Method)
			require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
			for _, expectedHeader := range validateHeaders {
				reqHeaders := msg.req.GetHeaders(expectedHeader.Name())
				found := false
				for _, reqHeader := range reqHeaders {
					if reqHeader.Value() == expectedHeader.Value() {
						found = true
						break
					}
				}
				require.True(t, found, "REFER request should contain the expected header %s: %s, instead got: %v", expectedHeader.Name(), expectedHeader.Value(), reqHeaders)
			}
			t.Logf("Received REFER request, responding REFER-%d %s", referStatus, sipStatus(referStatus))
			resp := sip.NewResponseFromRequest(msg.req, referStatus, sipStatus(referStatus), nil)
			return msg.tx.Respond(resp)
		}
	}

	sendNotify := func(t *testing.T, ctx context.Context, call *sipUADialogTest, notifyStatuses []int) error {
		t.Helper()
		require.NotEmpty(t, notifyStatuses)
		for i, status := range notifyStatuses {
			select {
			case <-ctx.Done():
				return fmt.Errorf("test aborted without ending remaining %d NOTIFY requests: %v", len(notifyStatuses)-i, ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
			t.Logf("Sending NOTIFY request carrying INVITE-%d response", status)
			notifyReq := call.NewRequest(sip.NOTIFY)
			notifyReq.AppendHeader(sip.NewHeader("Event", "refer"))
			notifyReq.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag"))
			notifyReq.SetBody([]byte(sip.NewResponse(status, sipStatus(status)).String()))
			notifyResp := call.TransactionRequest(t, notifyReq)
			t.Logf("Received NOTIFY-%d %s response", notifyResp.StatusCode, notifyResp.Reason)
			require.Equal(t, 200, notifyResp.StatusCode, "Expecting 200 OK response to NOTIFY")
		}
		t.Log("All NOTIFY requests sent")
		return nil
	}

	handleBye := func(t *testing.T, ctx context.Context, reqChan <-chan *sipUARequest, call *sipUADialogTest) error {
		// This will receive the BYE request, respond with 200 OK.
		t.Helper()
		select {
		case <-ctx.Done():
			return fmt.Errorf("test aborted without receiving a BYE request: %v", ctx.Err())
		case msg := <-reqChan:
			require.NotNil(t, msg)
			require.Equal(t, sip.BYE, msg.req.Method)
			require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
			t.Logf("Received BYE")
			resp := sip.NewResponseFromRequest(msg.req, 200, sipStatus(200), nil)
			return msg.tx.Respond(resp)
		}
	}

	sendBye := func(t *testing.T, call *sipUADialogTest) error {
		t.Helper()
		t.Logf("Sending BYE")
		resp := call.TransactionRequest(t, call.NewRequest(sip.BYE))
		t.Logf("Received BYE-%d %s response", resp.StatusCode, resp.Reason)
		require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting BYE-200 OK")
		return nil
	}

	startTransfer := func(t *testing.T, ctx context.Context, st *serviceTest, call *sipUADialogTest, to string, headers map[string]string, dialtone bool) <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			defer close(done)
			transferReq := &rpc.InternalTransferSIPParticipantRequest{
				SipCallId:    string(call.remoteTag),
				TransferTo:   to,
				Headers:      headers,
				PlayDialtone: dialtone,
			}
			if deadline, ok := ctx.Deadline(); ok {
				transferReq.RingingTimeout = durationpb.New(time.Until(deadline) + (5 * time.Millisecond))
			}
			_, err := st.Service.TransferSIPParticipant(ctx, transferReq)
			if err != nil {
				done <- err
			}
		}()
		return done
	}

	directions := map[string]func(t *testing.T, st *serviceTest) *sipUADialogTest{
		"inbound": func(t *testing.T, st *serviceTest) *sipUADialogTest {
			call, _ := st.CreateInboundCall(t)
			return call
		},
		"outbound": func(t *testing.T, st *serviceTest) *sipUADialogTest {
			call, _, _ := st.CreateOutboundCall(t)
			return call
		},
	}

	for direction, setupCall := range directions {
		t.Run(direction, func(t *testing.T) {
			t.Run("normal", func(t *testing.T) {
				st := NewServiceTest(t, nil)
				call := setupCall(t, st)

				reqChan := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")

				ctx, cancel := context.WithTimeout(t.Context(), time.Second*3)
				defer cancel()

				transferRes := startTransfer(t, ctx, st, call, referTo, nil, false)

				err := handleRefer(t, ctx, reqChan, call, 202, expectHeaders(referTo, nil))
				require.NoError(t, err, "Failed to process REFER request")
				err = sendNotify(t, ctx, call, []int{100, 180, 200})
				require.NoError(t, err, "Failed to send NOTIFY requests")
				err = handleBye(t, ctx, reqChan, call) // Expexting BYE after successful transfer
				require.NoError(t, err, "Failed to process BYE request")
				select {
				case err := <-transferRes:
					require.NoError(t, err, "error transferring call, unexpected transfer API response")
				case <-ctx.Done():
					require.NoError(t, ctx.Err(), "timeout waiting for BYE to arrive")
				}
			})

			t.Run("noraml_concurrent", func(t *testing.T) {
				// Same as normal, but with multiple concurrent transfers API requests.
				st := NewServiceTest(t, nil)
				call := setupCall(t, st)

				reqChan := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")

				ctx, cancel := context.WithTimeout(t.Context(), time.Second*3)
				defer cancel()

				transferResults := []<-chan error{
					startTransfer(t, ctx, st, call, referTo, nil, false),
					startTransfer(t, ctx, st, call, referTo, nil, false),
					startTransfer(t, ctx, st, call, referTo, nil, false),
				}

				err := handleRefer(t, ctx, reqChan, call, 202, expectHeaders(referTo, nil))
				require.NoError(t, err, "Failed to process REFER request")
				err = sendNotify(t, ctx, call, []int{100, 180, 200})
				require.NoError(t, err, "Failed to send NOTIFY requests")
				err = handleBye(t, ctx, reqChan, call) // Expexting BYE after successful transfer
				require.NoError(t, err, "Failed to process BYE request")
				for _, transferRes := range transferResults {
					select {
					case err := <-transferRes:
						require.NoError(t, err, "error transferring call, unexpected transfer API response")
					case <-ctx.Done():
						require.NoError(t, ctx.Err(), "timeout waiting for BYE to arrive")
					}
				}
			})

			t.Run("with headers", func(t *testing.T) {
				st := NewServiceTest(t, nil)
				call := setupCall(t, st)

				reqChan := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")

				ctx, cancel := context.WithTimeout(t.Context(), time.Second*3)
				defer cancel()

				headers := map[string]string{
					"X-Custom-Header": "custom-value",
				}
				transferRes := startTransfer(t, ctx, st, call, referTo, headers, false)

				err := handleRefer(t, ctx, reqChan, call, 202, expectHeaders(referTo, headers))
				require.NoError(t, err, "Failed to process REFER request")
				err = sendNotify(t, ctx, call, []int{100, 180, 200})
				require.NoError(t, err, "Failed to send NOTIFY requests")
				err = handleBye(t, ctx, reqChan, call) // Expexting BYE after successful transfer
				require.NoError(t, err, "Failed to process BYE request")
				select {
				case err := <-transferRes:
					require.NoError(t, err, "error transferring call, unexpected transfer API response")
				case <-ctx.Done():
					require.NoError(t, ctx.Err(), "timeout waiting for BYE to arrive")
				}
			})

			t.Run("failed", func(t *testing.T) {
				// Transfer fails, we don't receive a BYE
				const finalNotifyStatus = 480

				st := NewServiceTest(t, nil)
				call := setupCall(t, st)

				reqChan := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")

				ctx, cancel := context.WithTimeout(t.Context(), time.Second*3)
				defer cancel()

				transferRes := startTransfer(t, ctx, st, call, referTo, nil, false)

				err := handleRefer(t, ctx, reqChan, call, 202, expectHeaders(referTo, nil))
				require.NoError(t, err, "Failed to process REFER request")
				err = sendNotify(t, ctx, call, []int{100, 180, finalNotifyStatus})
				require.NoError(t, err, "Failed to send NOTIFY requests")
				select {
				case <-time.After(time.Millisecond * 250):
					t.Logf("No BYE received, as expected")
				case msg := <-reqChan:
					t.Fatalf("Received unexpected request: %+v", msg)
				}
				err = sendBye(t, call)
				require.NoError(t, err, "Failed to send BYE request")
				select {
				case err := <-transferRes:
					t.Logf("Received error: %v", err)
					require.Error(t, err)
					var psErr psrpc.Error
					require.ErrorAs(t, err, &psErr)
					var sipErr *livekit.SIPStatus
					require.ErrorAs(t, err, &sipErr)
					require.Equal(t, livekit.SIPStatusCode(finalNotifyStatus), sipErr.Code)
					require.Equal(t, sipStatus(finalNotifyStatus), sipErr.Status)
				case <-ctx.Done():
					require.NoError(t, ctx.Err(), "timeout waiting for BYE to arrive")
				}
			})

			t.Run("failed_concurrent", func(t *testing.T) {
				// Transfer fails, we don't receive a BYE
				const finalNotifyStatus = 480

				st := NewServiceTest(t, nil)
				call := setupCall(t, st)

				reqChan := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")

				ctx, cancel := context.WithTimeout(t.Context(), time.Second*3)
				defer cancel()

				transferResults := []<-chan error{
					startTransfer(t, ctx, st, call, referTo, nil, false),
					startTransfer(t, ctx, st, call, referTo, nil, false),
					startTransfer(t, ctx, st, call, referTo, nil, false),
				}

				err := handleRefer(t, ctx, reqChan, call, 202, expectHeaders(referTo, nil))
				require.NoError(t, err, "Failed to process REFER request")
				err = sendNotify(t, ctx, call, []int{100, 180, finalNotifyStatus})
				require.NoError(t, err, "Failed to send NOTIFY requests")
				select {
				case <-time.After(time.Millisecond * 250):
					t.Logf("No BYE received, as expected")
				case msg := <-reqChan:
					t.Fatalf("Received unexpected request: %+v", msg)
				}
				err = sendBye(t, call)
				require.NoError(t, err, "Failed to send BYE request")
				for i, transferRes := range transferResults {
					select {
					case err := <-transferRes:
						t.Logf("Received error: %v for transfer %d", err, i)
						require.Error(t, err)
						var psErr psrpc.Error
						require.ErrorAs(t, err, &psErr)
						var sipErr *livekit.SIPStatus
						require.ErrorAs(t, err, &sipErr)
						require.Equal(t, livekit.SIPStatusCode(finalNotifyStatus), sipErr.Code)
						require.Equal(t, sipStatus(finalNotifyStatus), sipErr.Status)
					case <-ctx.Done():
						require.NoError(t, ctx.Err(), "timeout waiting for BYE to arrive")
					}
				}
			})

			t.Run("bye", func(t *testing.T) {
				// After REFER gets 202 we wait on NOTIFY; remote hangs up with BYE instead.
				st := NewServiceTest(t, nil)
				call := setupCall(t, st)

				reqChan := call.RegisterRequestChannel("")
				defer call.UnregisterRequestChannel("")

				ctx, cancel := context.WithTimeout(t.Context(), time.Second*3)
				defer cancel()

				headers := map[string]string{
					"X-Custom-Header": "custom-value",
				}
				transferRes := startTransfer(t, ctx, st, call, referTo, headers, false)

				err := handleRefer(t, ctx, reqChan, call, 202, expectHeaders(referTo, headers))
				require.NoError(t, err, "Failed to process REFER request")
				err = sendBye(t, call)
				require.NoError(t, err, "Failed to send BYE request")

				select {
				case err := <-transferRes:
					require.NoError(t, err, "error transferring call, unexpected transfer API response")
				case <-ctx.Done():
					require.NoError(t, ctx.Err(), "timeout waiting for BYE to arrive")
				}
			})
		})
	}
}

func TestRouteSet(t *testing.T) {
	// makeRouteSetHeaders creates two Record-Route headers simulating two proxies
	// in the signaling path. Returns the headers and the expected Route order for
	// both UAC and UAS sides.
	makeRouteSetHeaders := func(t *testing.T, st *serviceTest) (rrHeaders []sip.Header, expectUAS, expectUAC []string) {
		t.Helper()
		uri := st.TestUA.LocalURI()
		uri1 := *uri
		uri1.User = "proxy-user1"
		uri1.UriParams = sip.NewParams()
		uri1.UriParams.Add("lr", "")
		uri1.UriParams.Add("check", "first")
		uri2 := *uri
		uri2.User = "proxy-user2"
		uri2.UriParams = sip.NewParams()
		uri2.UriParams.Add("lr", "")
		uri2.UriParams.Add("check", "second")
		rr1 := &sip.RecordRouteHeader{Address: uri1}
		rr2 := &sip.RecordRouteHeader{Address: uri2}
		// Record-Route order as seen on the wire: rr1 (topmost), rr2
		rrHeaders = []sip.Header{rr1, rr2}
		// UAS route set: in order (RFC 3261 §12.1.1)
		expectUAS = []string{rr1.Value(), rr2.Value()}
		// UAC route set: reversed (RFC 3261 §12.1.2)
		expectUAC = []string{rr2.Value(), rr1.Value()}
		return
	}

	// assertRouteHeaders verifies that a request carries the expected Route headers.
	assertRouteHeaders := func(t *testing.T, req *sip.Request, expected []string) {
		t.Helper()
		routeHeaders := req.GetHeaders("Route")
		require.Equal(t, len(expected), len(routeHeaders), "wrong number of Route headers")
		for i, exp := range expected {
			require.Equal(t, exp, routeHeaders[i].Value(), "Route header %d mismatch", i)
		}
	}

	t.Run("inbound", func(t *testing.T) {
		// Server is UAS for inbound calls. Route set should be in order.

		t.Run("BYE", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			rrHeaders, expectUAS, _ := makeRouteSetHeaders(t, st)
			call, ic := st.CreateInboundCall(t, withTestHeaders(rrHeaders...))

			byeSink := st.TestUA.RegisterSink(call.localTag, "BYE")
			defer st.TestUA.UnregisterSink(call.localTag, "BYE")

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			// Close triggers server-side BYE. Run in goroutine since Close
			// blocks until the BYE transaction completes.
			closed := make(chan error, 1)
			go func() {
				defer close(closed)
				closed <- ic.Close()
			}()

			select {
			case msg := <-byeSink:
				require.NotNil(t, msg)
				require.Equal(t, sip.BYE, msg.req.Method)
				assertRouteHeaders(t, msg.req, expectUAS)
				_ = msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 200, "OK", nil))
			case <-ctx.Done():
				require.Fail(t, "timeout waiting for BYE")
			}
			err := <-closed
			require.NoError(t, err)
		})

		t.Run("REFER", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			rrHeaders, expectUAS, _ := makeRouteSetHeaders(t, st)
			call, ic := st.CreateInboundCall(t, withTestHeaders(rrHeaders...))
			t.Cleanup(func() { ic.Close() })

			referSink := st.TestUA.RegisterSink(call.localTag, "REFER")
			defer st.TestUA.UnregisterSink(call.localTag, "REFER")

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			transferRes := make(chan error, 1)
			go func() {
				defer close(transferRes)
				err := ic.transferCall(ctx, "tel:+15551234567", nil, false)
				if err != nil {
					transferRes <- err
				}
			}()

			select {
			case msg := <-referSink:
				require.NotNil(t, msg)
				require.Equal(t, sip.REFER, msg.req.Method)
				assertRouteHeaders(t, msg.req, expectUAS)
				_ = msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 202, "Accepted", nil))
			case err := <-transferRes:
				require.Fail(t, "unexpected transfer result", err)
			case <-ctx.Done():
				require.Fail(t, "timeout waiting for REFER")
			}

			req := call.NewRequest(sip.BYE)
			resp := st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")

			err := <-transferRes
			require.NoError(t, err)
		})
	})

	t.Run("outbound", func(t *testing.T) {
		// Server is UAC for outbound calls. Route set should be reversed.

		t.Run("ACK", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			rrHeaders, _, expectUAC := makeRouteSetHeaders(t, st)
			call, _, ackReq := st.CreateOutboundCall(t, withTestHeaders(rrHeaders...))
			assertRouteHeaders(t, ackReq, expectUAC)

			req := call.NewRequest(sip.BYE)
			resp := st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")
		})

		t.Run("BYE", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			rrHeaders, _, expectUAC := makeRouteSetHeaders(t, st)
			call, oc, _ := st.CreateOutboundCall(t, withTestHeaders(rrHeaders...))

			byeSink := st.TestUA.RegisterSink(call.localTag, "BYE")
			defer st.TestUA.UnregisterSink(call.localTag, "BYE")

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			closed := make(chan error, 1)
			go func() {
				defer close(closed)
				closed <- oc.Close(ctx)
			}()

			select {
			case msg := <-byeSink:
				require.NotNil(t, msg)
				require.Equal(t, sip.BYE, msg.req.Method)
				assertRouteHeaders(t, msg.req, expectUAC)
				_ = msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 200, "OK", nil))
			case <-ctx.Done():
				require.Fail(t, "timeout waiting for BYE")
			}
			err := <-closed
			require.NoError(t, err)
		})

		t.Run("REFER", func(t *testing.T) {
			st := NewServiceTest(t, nil)
			rrHeaders, _, expectUAC := makeRouteSetHeaders(t, st)
			call, oc, _ := st.CreateOutboundCall(t, withTestHeaders(rrHeaders...))

			referSink := st.TestUA.RegisterSink(call.localTag, "REFER")
			defer st.TestUA.UnregisterSink(call.localTag, "REFER")

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			transferRes := make(chan error, 1)
			go func() {
				defer close(transferRes)
				transferRes <- oc.transferCall(ctx, "tel:+15551234567", nil, false)
			}()

			select {
			case msg := <-referSink:
				require.NotNil(t, msg)
				require.Equal(t, sip.REFER, msg.req.Method)
				assertRouteHeaders(t, msg.req, expectUAC)
				_ = msg.tx.Respond(sip.NewResponseFromRequest(msg.req, 202, "Accepted", nil))
			case err := <-transferRes:
				require.Fail(t, "unexpected transfer result", err)
			case <-ctx.Done():
				require.Fail(t, "timeout waiting for REFER")
			}

			req := call.NewRequest(sip.BYE)
			resp := st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")

			err := <-transferRes
			require.NoError(t, err)
		})
	})
}
