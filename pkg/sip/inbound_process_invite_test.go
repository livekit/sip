package sip

import (
	"log/slog"
	"math/rand"
	"net/netip"
	"testing"
	"time"

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

type InboundTest struct {
	Server        *Server
	Handler       Handler
	Client        *sipgo.Client
	addr          netip.AddrPort
	LiveKitClient *Client
}

func (it *InboundTest) NewInvite(t *testing.T, callID string, cseq uint32, offer []byte) (*sip.Request, []byte) {
	if offer == nil {
		sdpOffer, err := sdp.NewOffer(it.addr.Addr(), 0xB0B, sdp.EncryptionNone)
		require.NoError(t, err)
		offer, err = sdpOffer.SDP.Marshal()
		require.NoError(t, err)
	}

	inviteReq := sip.NewRequest(sip.INVITE, sip.Uri{User: "to", Host: it.addr.String()})
	fromTag := sip.GenerateTagN(16)
	inviteReq.AppendHeader(&sip.FromHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   "caller",
			Host:   it.addr.String(),
		},
		Params: sip.HeaderParams{
			"tag": fromTag,
		},
	})
	inviteReq.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   "callee",
			Host:   it.addr.String(),
		},
	})
	inviteReq.SetDestination(it.addr.String())
	inviteReq.SetBody(offer)
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	callIDHdr := sip.CallIDHeader(callID)
	inviteReq.AppendHeader(&callIDHdr)
	inviteReq.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.INVITE})
	return inviteReq, offer
}

func (it *InboundTest) NewInviteWithToTag(t *testing.T, callID string, cseq uint32, toTag string, offer []byte) (*sip.Request, []byte) {
	inviteReq, offer := it.NewInvite(t, callID, cseq, offer)
	to := inviteReq.To()
	if to.Params == nil {
		to.Params = sip.NewParams()
	}
	to.Params.Remove("tag")
	to.Params.Add("tag", toTag)
	return inviteReq, offer
}

func (it *InboundTest) TransactionRequest(t *testing.T, req *sip.Request) *sip.Response {
	tx, err := it.Client.TransactionRequest(req)
	require.NoError(t, err)
	defer tx.Terminate()

	resp := getFinalResponseOrFail(t, tx, req)
	if resp.StatusCode < 300 { // Need to send ACK for 2xx, sipgo sends ACK for 3xx+
		ack := sip.NewAckRequest(req, resp, nil)
		err = it.Client.WriteRequest(ack)
		require.NoError(t, err)
	}
	return resp
}

func (it *InboundTest) Address() netip.AddrPort {
	return it.addr
}

func NewInboundTest(t *testing.T) *InboundTest {
	t.Helper()

	sipPort := rand.Intn(testPortSIPMax-testPortSIPMin) + testPortSIPMin
	loopback := netip.MustParseAddr("127.0.0.1")

	conf := &config.Config{
		MaxCpuUtilization:  0.9,
		SIPPort:            sipPort,
		SIPPortListen:      sipPort,
		RTPPort:            rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
		SIPRingingInterval: time.Second,
	}
	mon, err := stats.NewMonitor(conf)
	require.NoError(t, err)
	require.NoError(t, mon.Start(conf), "start monitor so metrics (e.g. inviteReqRaw) are registered")

	log := logger.NewTestLogger(t)

	cli := NewClient("", conf, log, mon, func(projectID string) rpc.IOInfoClient { return &MockIOInfoClient{} })
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

	err = srv.Start(nil, sconf, nil, nil)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	handler := &TestHandler{}
	srv.SetHandler(handler)

	addr := netip.AddrPortFrom(loopback, uint16(sipPort))

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("from@test"),
		sipgo.WithUserAgentLogger(slog.New(logger.ToSlogHandler(srv.log))),
	)
	require.NoError(t, err)

	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	t.Cleanup(func() { ua.Close() })

	return &InboundTest{Server: srv, Handler: handler, Client: client, addr: addr, LiveKitClient: cli}
}

// RegisterOutboundCallForReinvite registers a fake outbound call so that a re-INVITE
// with the given localTag (To tag) is accepted as outbound reinvite and answered with sdpOffer.
// Requires it.LiveKitClient != nil (use NewInboundTestWithClient).
func (it *InboundTest) RegisterOutboundCallForReinvite(t *testing.T, localTag LocalTag, sdpOffer []byte) {
	t.Helper()
	log := logger.NewTestLogger(t).WithValues("callID", localTag)
	from := CreateURIFromUserAndAddress("out", it.addr.String(), TransportUDP)
	contact := CreateURIFromUserAndAddress("out", it.addr.String(), TransportUDP)
	so := it.LiveKitClient.newOutbound(log, localTag, from, contact, nil, nil)
	inviteWithBody := sip.NewRequest(sip.INVITE, sip.Uri{User: "to", Host: it.addr.String()})
	inviteWithBody.SetBody(sdpOffer)
	so.mu.Lock()
	so.invite = inviteWithBody
	so.mu.Unlock()
	oc := &outboundCall{cc: so, log: log}
	it.LiveKitClient.cmu.Lock()
	it.LiveKitClient.activeCalls[localTag] = oc
	it.LiveKitClient.cmu.Unlock()
}

func TestProcessInvite_Reinvite(t *testing.T) {
	it := NewInboundTest(t)

	cseq := uint32(2)
	callID := "reinvite-new@test"
	origInviteReq, _ := it.NewInvite(t, callID, cseq, nil)
	firstResp := it.TransactionRequest(t, origInviteReq.Clone())
	require.Equal(t, sip.StatusCode(200), firstResp.StatusCode, "200 OK")
	answer := string(firstResp.Body())

	// Test prev CSeq
	req2 := origInviteReq.Clone()
	req2.CSeq().SeqNo = cseq - 1
	resp2 := it.TransactionRequest(t, req2)
	require.Equal(t, sip.StatusCode(200), resp2.StatusCode, "200 OK")
	require.NotEqual(t, answer, string(resp2.Body()), "answer should not be the same as original answer")

	// Test next CSeq
	req3 := origInviteReq.Clone()
	req3.CSeq().SeqNo = cseq + 1
	req3.ReplaceHeader(sip.HeaderClone(firstResp.To()))
	resp3 := it.TransactionRequest(t, req3)
	require.Equal(t, sip.StatusCode(200), resp3.StatusCode, "200 OK")
	require.Equal(t, answer, string(resp3.Body()), "answer should be the same")
	require.NotEqual(t, resp2.To().Params["tag"], resp3.To().Params["tag"], "to tag should not be the same")
}

func TestProcessInvite_ReinviteOutbound(t *testing.T) {
	it := NewInboundTest(t)

	localTag := LocalTag("out-reinvite-1")
	sdpOffer, err := sdp.NewOffer(it.addr.Addr(), 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerBytes, err := sdpOffer.SDP.Marshal()
	require.NoError(t, err)

	it.RegisterOutboundCallForReinvite(t, localTag, offerBytes)

	// Re-INVITE for the outbound call: To tag = our local tag, CSeq > 0 (InviteCSeq)
	req, _ := it.NewInviteWithToTag(t, "reinvite-outbound@test", 1, string(localTag), offerBytes)
	resp := it.TransactionRequest(t, req)
	require.Equal(t, sip.StatusCode(200), resp.StatusCode, "reinvite for outbound call should get 200 OK")
	require.Equal(t, offerBytes, resp.Body(), "reinvite 200 OK should return our offer (keepalive)")
}

func TestProcessInvite_ReinviteOutbound_HigherCSeq(t *testing.T) {
	it := NewInboundTest(t)

	localTag := LocalTag("out-reinvite-2")
	sdpOffer, err := sdp.NewOffer(it.addr.Addr(), 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerBytes, err := sdpOffer.SDP.Marshal()
	require.NoError(t, err)

	it.RegisterOutboundCallForReinvite(t, localTag, offerBytes)

	// First reinvite: CSeq 1
	req1, _ := it.NewInviteWithToTag(t, "reinvite-outbound-2@test", 1, string(localTag), offerBytes)
	resp1 := it.TransactionRequest(t, req1)
	require.Equal(t, sip.StatusCode(200), resp1.StatusCode)
	require.Equal(t, offerBytes, resp1.Body())

	// Second reinvite: CSeq 2 (still accepted as reinvite)
	req2, _ := it.NewInviteWithToTag(t, "reinvite-outbound-2@test", 2, string(localTag), offerBytes)
	resp2 := it.TransactionRequest(t, req2)
	require.Equal(t, sip.StatusCode(200), resp2.StatusCode)
	require.Equal(t, offerBytes, resp2.Body())
}

func TestProcessInvite_ReinviteOutbound_UnknownTagNotTreatedAsReinvite(t *testing.T) {
	// Server has a Client but no active call for tag "unknown-tag".
	// INVITE with To tag "unknown-tag" should not be accepted as outbound reinvite;
	// it goes through normal inbound flow (and will need auth/dispatch etc.).
	it := NewInboundTest(t)

	localTag := "unknown-tag"
	sdpOffer, err := sdp.NewOffer(it.addr.Addr(), 0xB0B, sdp.EncryptionNone)
	require.NoError(t, err)
	offerBytes, err := sdpOffer.SDP.Marshal()
	require.NoError(t, err)

	// Do NOT register an outbound call for localTag. Send re-INVITE with that To tag.
	req, _ := it.NewInviteWithToTag(t, "reinvite-unknown@test", 1, localTag, offerBytes)
	resp := it.TransactionRequest(t, req)
	// Should still get 200 (normal inbound flow accepts the call), but body is the
	// answer from the handler, not our offer (we didn't register an outbound call).
	require.Equal(t, sip.StatusCode(200), resp.StatusCode)
	// Answer from room is different from our offer (we sent offer, handler returns answer)
	require.NotEmpty(t, resp.Body())
	require.NotEqual(t, offerBytes, resp.Body(), "unknown tag should not get outbound reinvite keepalive (our offer)")
}
