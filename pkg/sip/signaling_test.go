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

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
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

func (s *sipUATest) RegisterSink(localTag string, method string) chan *sipUARequest {
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
		ack := sip.NewAckRequest(req, resp, nil, isFromUAC)
		err = s.Client.WriteRequest(ack)
		require.NoError(t, err)
	}
	return resp
}

var index atomic.Int32

type sipUADialogTest struct {
	TestUA     *sipUATest
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

func newTestCall(testUA *sipUATest, forceRemoteTag bool) *sipUADialogTest {
	i := index.Add(1)
	d := &sipUADialogTest{
		TestUA:     testUA,
		callID:     fmt.Sprintf("callID-%d", i),
		localUser:  fmt.Sprintf("localTestUser-%d", i),
		remoteUser: fmt.Sprintf("remoteTestUser-%d", i),
		localTag:   fmt.Sprintf("localTestTag-%d", i),
		remoteTag:  "", // Needs to be set by remote
		localCseq:  1,
		remoteCseq: 0, // Needs to be set by remote
	}
	if forceRemoteTag {
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
		sdpOffer, err := sdp.NewOffer(d.TestUA.localAddr.Addr(), 0xB0B, sdp.EncryptionNone)
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

type serviceTest struct {
	TestUA  *sipUATest
	Server  *Server // Processing inbound calls and SIP
	Client  *Client // Processing outbound calls
	Handler Handler
}

func NewServiceTest(t *testing.T) *serviceTest {
	t.Helper()

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
		func(projectID string) rpc.IOInfoClient { return &MockIOInfoClient{} },
		WithGetRoomClient(newTestRoom),
	)
	srv := NewServer(
		"",
		conf,
		log,
		mon,
		func(projectID string) rpc.IOInfoClient { return &MockIOInfoClient{} },
		WithGetRoomServer(newTestRoom),
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

	return &serviceTest{
		TestUA:  newUATest(t, srv.log, addr, withUATestBuffer(3)),
		Server:  srv,
		Client:  cli,
		Handler: handler,
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
	var reqSink chan *sipUARequest
	defer st.TestUA.UnregisterSink("", "INVITE")

	call := newTestCall(st.TestUA, true)
	req := call.CreateSipParticipantRequest()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := st.Client.CreateSIPParticipant(ctx, req)
	require.NoError(t, err)

	select {
	case msg := <-newInviteSink:
		if msg == nil {
			require.Fail(t, "unexpected nil message")
		}
		st.TestUA.UnregisterSink("", "INVITE")

		require.Equal(t, string(call.remoteTag), msg.req.From().Params.GetOr("tag", ""), "remote tag should be the same")
		require.Equal(t, call.remoteUser, msg.req.From().Address.User, "remote user should be the same")
		require.Equal(t, call.localUser, msg.req.To().Address.User, "local user should be the same")

		for _, opt := range opts {
			opt(msg.req, nil) // Simulate added headers
		}

		offer, err := sdp.ParseOffer(msg.req.Body())
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
		if msg == nil {
			require.Fail(t, "unexpected nil message")
		}
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
			st := NewServiceTest(t)
			call, _ := st.CreateInboundCall(t)
			serverLocalSDP := call.remoteSDP

			// Re-INVITE
			req, _, err := call.Invite(call.localSDP)
			require.NoError(t, err)
			resp := st.TestUA.TransactionRequest(t, req, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")
			require.Equal(t, serverLocalSDP, resp.Body(), "reinvite 200 OK should return server local SDP")

			// Re-INVITE with new offer
			newOffer, err := sdp.NewOffer(netip.MustParseAddr("9.8.7.6"), 12345, sdp.EncryptionNone)
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
			st := NewServiceTest(t)
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
			st := NewServiceTest(t)
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
			newOffer, err := sdp.NewOffer(netip.MustParseAddr("9.8.7.6"), 12345, sdp.EncryptionNone)
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
			st := NewServiceTest(t)
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
	t.Run("inbound", func(t *testing.T) {
		prepFunc := func(t *testing.T) (*serviceTest, *sipUADialogTest, *inboundCall) {
			t.Helper()
			st := NewServiceTest(t)
			call, ic := st.CreateInboundCall(t)
			return st, call, ic
		}

		startTransfer := func(ctx context.Context, ic *inboundCall, to string, headers map[string]string, dialtone bool) <-chan error {
			transferRes := make(chan error, 1)
			go func() {
				defer close(transferRes)
				err := ic.transferCall(ctx, to, headers, dialtone)
				if err != nil {
					transferRes <- err
				}
			}()
			return transferRes
		}

		t.Run("normal", func(t *testing.T) {
			st, call, ic := prepFunc(t)
			reqChan := st.TestUA.RegisterSink(call.localTag, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
			defer cancel()

			transferRes := startTransfer(ctx, ic, "tel:+15551234567", nil, false)

			select {
			case msg := <-reqChan:
				require.Equal(t, sip.REFER, msg.req.Method)
				require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
				hdrs := msg.req.GetHeaders("Refer-To")
				require.Equal(t, 1, len(hdrs), "should be 1 Refer-To header")
				require.Equal(t, "<tel:+15551234567>", hdrs[0].Value())
				resp := sip.NewResponseFromRequest(msg.req, 202, "Accepted", nil)
				msg.tx.Respond(resp)
				// OK, proceed
			case err := <-transferRes:
				t.Fatal("error transferring call, unexpected transfer API response", err)
			case <-ctx.Done():
				t.Fatal("timeout waiting for REFER to arrive")
			}

			notifyReq := call.NewRequest(sip.NOTIFY)
			notifyReq.AppendHeader(sip.NewHeader("Event", "refer"))
			notifyReq.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag"))
			notifyReq.SetBody([]byte(sip.NewResponse(200, "OK").String()))

			resp := st.TestUA.TransactionRequest(t, notifyReq, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")

			select {
			case err := <-transferRes:
				if err != nil {
					t.Fatal("error transferring call, unexpected transfer API response", err)
				}
			case <-ctx.Done():
				t.Fatal("timeout waiting for BYE to arrive")
			}
		})

		t.Run("with headers", func(t *testing.T) {
			st, call, ic := prepFunc(t)
			reqChan := st.TestUA.RegisterSink(call.localTag, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
			defer cancel()

			headers := map[string]string{
				"X-Custom-Header": "custom-value",
			}
			transferRes := startTransfer(ctx, ic, "tel:+15551234567", headers, false)

			select {
			case msg := <-reqChan:
				require.Equal(t, sip.REFER, msg.req.Method)
				require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
				hdrs := msg.req.GetHeaders("Refer-To")
				require.Equal(t, 1, len(hdrs), "should be 1 Refer-To header")
				require.Equal(t, "<tel:+15551234567>", hdrs[0].Value())
				hdr := msg.req.GetHeaders("X-Custom-Header")
				require.Equal(t, 1, len(hdr), "should be 1 X-Custom-Header")
				require.Equal(t, "custom-value", hdr[0].Value())
				resp := sip.NewResponseFromRequest(msg.req, 480, "Temporarily Unavailable", nil)
				msg.tx.Respond(resp)
			case err := <-transferRes:
				t.Fatal("error transferring call, unexpected transfer API response", err)
			case <-ctx.Done():
				t.Fatal("timeout waiting for REFER to arrive")
			}
		})

		t.Run("bye", func(t *testing.T) {
			// After REFER gets 202 we wait on NOTIFY; remote hangs up with BYE instead.
			st, call, ic := prepFunc(t)
			reqChan := st.TestUA.RegisterSink(call.localTag, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
			defer cancel()

			transferRes := startTransfer(ctx, ic, "tel:+15551234567", nil, false)

			select {
			case msg := <-reqChan:
				require.Equal(t, sip.REFER, msg.req.Method)
				require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
				hdrs := msg.req.GetHeaders("Refer-To")
				require.Equal(t, 1, len(hdrs), "should be 1 Refer-To header")
				require.Equal(t, "<tel:+15551234567>", hdrs[0].Value())
				resp := sip.NewResponseFromRequest(msg.req, 202, "Accepted", nil)
				msg.tx.Respond(resp)
				// OK, proceed
			case err := <-transferRes:
				t.Fatal("error transferring call, unexpected transfer API response", err)
			case <-ctx.Done():
				t.Fatal("timeout waiting for REFER to arrive")
			}

			byeReq := call.NewRequest(sip.BYE)
			resp := st.TestUA.TransactionRequest(t, byeReq, true)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")

			select {
			case err := <-transferRes:
				if err != nil {
					t.Fatal("error transferring call, unexpected transfer API response", err)
				}
			case <-ctx.Done():
				t.Fatal("timeout waiting for BYE to arrive")
			}
		})
	})

	t.Run("outbound", func(t *testing.T) {
		prepFunc := func(t *testing.T) (*serviceTest, *sipUADialogTest, *outboundCall) {
			t.Helper()
			st := NewServiceTest(t)
			call, oc, _ := st.CreateOutboundCall(t)
			return st, call, oc
		}

		startTransfer := func(t *testing.T, ctx context.Context, oc *outboundCall, to string, headers map[string]string, dialtone bool) <-chan error {
			transferRes := make(chan error, 1)
			go func() {
				defer close(transferRes)
				t.Logf("startTransfer")
				err := oc.transferCall(ctx, to, headers, dialtone)
				t.Logf("startTransfer done")
				if err != nil {
					t.Logf("startTransfer error")
					transferRes <- err
				}
			}()
			return transferRes
		}

		t.Run("normal", func(t *testing.T) {
			st, call, oc := prepFunc(t)
			reqChan := st.TestUA.RegisterSink(call.localTag, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
			defer cancel()

			transferRes := startTransfer(t, ctx, oc, "tel:+15551234567", nil, false)

			select {
			case msg := <-reqChan:
				require.Equal(t, sip.REFER, msg.req.Method)
				require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
				hdrs := msg.req.GetHeaders("Refer-To")
				require.Equal(t, 1, len(hdrs), "should be 1 Refer-To header")
				require.Equal(t, "<tel:+15551234567>", hdrs[0].Value())
				resp := sip.NewResponseFromRequest(msg.req, 202, "Accepted", nil)
				msg.tx.Respond(resp)
				// OK, proceed
			case err := <-transferRes:
				t.Fatal("error transferring call, unexpected transfer API response", err)
			case <-ctx.Done():
				t.Fatal("timeout waiting for REFER to arrive")
			}

			notifyReq := call.NewRequest(sip.NOTIFY)
			notifyReq.AppendHeader(sip.NewHeader("Event", "refer"))
			notifyReq.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag"))
			notifyReq.SetBody([]byte(sip.NewResponse(200, "OK").String()))

			resp := st.TestUA.TransactionRequest(t, notifyReq, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")

			select {
			case err := <-transferRes:
				if err != nil {
					t.Fatal("error transferring call, unexpected transfer API response", err)
				}
			case <-ctx.Done():
				t.Fatal("timeout waiting for BYE to arrive")
			}
		})

		t.Run("with headers", func(t *testing.T) {
			st, call, oc := prepFunc(t)
			reqChan := st.TestUA.RegisterSink(call.localTag, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
			defer cancel()

			headers := map[string]string{
				"X-Custom-Header": "custom-value",
			}
			transferRes := startTransfer(t, ctx, oc, "tel:+15551234567", headers, false)

			select {
			case msg := <-reqChan:
				require.Equal(t, sip.REFER, msg.req.Method)
				require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
				hdrs := msg.req.GetHeaders("Refer-To")
				require.Equal(t, 1, len(hdrs), "should be 1 Refer-To header")
				require.Equal(t, "<tel:+15551234567>", hdrs[0].Value())
				hdr := msg.req.GetHeaders("X-Custom-Header")
				require.Equal(t, 1, len(hdr), "should be 1 X-Custom-Header")
				require.Equal(t, "custom-value", hdr[0].Value())
				resp := sip.NewResponseFromRequest(msg.req, 480, "Temporarily Unavailable", nil)
				msg.tx.Respond(resp)
			case err := <-transferRes:
				t.Fatal("error transferring call, unexpected transfer API response", err)
			case <-ctx.Done():
				t.Fatal("timeout waiting for REFER to arrive")
			}
		})

		t.Run("bye", func(t *testing.T) {
			// After REFER gets 202 we wait on NOTIFY; remote hangs up with BYE instead.
			st, call, oc := prepFunc(t)
			reqChan := st.TestUA.RegisterSink(call.localTag, "")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
			defer cancel()

			transferRes := startTransfer(t, ctx, oc, "tel:+15551234567", nil, false)

			select {
			case msg := <-reqChan:
				require.Equal(t, sip.REFER, msg.req.Method)
				require.Equal(t, call.localTag, msg.req.To().Params.GetOr("tag", ""))
				hdrs := msg.req.GetHeaders("Refer-To")
				require.Equal(t, 1, len(hdrs), "should be 1 Refer-To header")
				require.Equal(t, "<tel:+15551234567>", hdrs[0].Value())
				resp := sip.NewResponseFromRequest(msg.req, 202, "Accepted", nil)
				msg.tx.Respond(resp)
				// OK, proceed
			case err := <-transferRes:
				t.Fatal("error transferring call, unexpected transfer API response", err)
			case <-ctx.Done():
				t.Fatal("timeout waiting for REFER to arrive")
			}

			byeReq := call.NewRequest(sip.BYE)
			resp := st.TestUA.TransactionRequest(t, byeReq, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")

			select {
			case err := <-transferRes:
				if err != nil {
					t.Fatal("error transferring call, unexpected transfer API response", err)
				}
			case <-ctx.Done():
				t.Fatal("timeout waiting for API to time return")
			}
		})
	})
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
			st := NewServiceTest(t)
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
			st := NewServiceTest(t)
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
			st := NewServiceTest(t)
			rrHeaders, _, expectUAC := makeRouteSetHeaders(t, st)
			call, _, ackReq := st.CreateOutboundCall(t, withTestHeaders(rrHeaders...))
			assertRouteHeaders(t, ackReq, expectUAC)

			req := call.NewRequest(sip.BYE)
			resp := st.TestUA.TransactionRequest(t, req, false)
			require.Equal(t, sip.StatusCode(200), resp.StatusCode, "Expecting 200 OK")
		})

		t.Run("BYE", func(t *testing.T) {
			st := NewServiceTest(t)
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
			st := NewServiceTest(t)
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
