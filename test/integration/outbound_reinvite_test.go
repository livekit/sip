package integration

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/sipgo"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/test/lktest"
)

// dialogState captures the SIP dialog established by the UAS OnInvite handler.
type dialogState struct {
	req  *sip.Request
	resp *sip.Response
}

// uasConfig holds the standalone SIP UAS components used by the re-INVITE tests.
type uasConfig struct {
	ua        *sipgo.UserAgent
	server    *sipgo.Server
	client    *sipgo.Client
	localIP   netip.Addr
	port      int
	addr      string
	createSDP func() []byte
	dialogCh  chan dialogState
	byeCh     chan struct{}
}

// setupUAS creates a standalone SIP UAS using sipgo directly that can accept
// outbound calls from the SIP service and send re-INVITEs back.
func setupUAS(t *testing.T, remoteNumber string) *uasConfig {
	t.Helper()

	localIP, err := config.GetLocalIP()
	require.NoError(t, err)

	uasPort := 5060 + rand.Intn(1000)
	uasLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(remoteNumber),
		sipgo.WithUserAgentLogger(uasLog),
	)
	require.NoError(t, err)

	uasClient, err := sipgo.NewClient(ua, sipgo.WithClientHostname(localIP.String()))
	require.NoError(t, err)
	t.Cleanup(func() { uasClient.Close() })

	uasServer, err := sipgo.NewServer(ua)
	require.NoError(t, err)
	t.Cleanup(func() { uasServer.Close() })

	dialogCh := make(chan dialogState, 1)
	byeCh := make(chan struct{}, 1)

	createSDP := func() []byte {
		offer := sdp.SessionDescription{
			Version: 0,
			Origin: sdp.Origin{
				Username:       "-",
				SessionID:      rand.Uint64(),
				SessionVersion: 1,
				NetworkType:    "IN",
				AddressType:    "IP4",
				UnicastAddress: localIP.String(),
			},
			SessionName: "TestUAS",
			ConnectionInformation: &sdp.ConnectionInformation{
				NetworkType: "IN",
				AddressType: "IP4",
				Address:     &sdp.Address{Address: localIP.String()},
			},
			TimeDescriptions: []sdp.TimeDescription{
				{Timing: sdp.Timing{StartTime: 0, StopTime: 0}},
			},
			MediaDescriptions: []*sdp.MediaDescription{
				{
					MediaName: sdp.MediaName{
						Media:   "audio",
						Port:    sdp.RangedPort{Value: 30000 + rand.Intn(1000)},
						Protos:  []string{"RTP", "AVP"},
						Formats: []string{"0"},
					},
					Attributes: []sdp.Attribute{
						{Key: "rtpmap", Value: "0 PCMU/8000"},
					},
				},
			},
		}
		b, _ := offer.Marshal()
		return b
	}

	uasServer.OnInvite(func(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
		t.Log("UAS: received INVITE from SIP service")

		sdpBody := createSDP()
		resp := sip.NewResponseFromRequest(req, 200, "OK", sdpBody)
		resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		resp.AppendHeader(sip.NewHeader("Contact",
			fmt.Sprintf("<sip:%s@%s:%d>", remoteNumber, localIP, uasPort)))

		if err := tx.Respond(resp); err != nil {
			t.Errorf("UAS: failed to respond to INVITE: %v", err)
			return
		}

		t.Log("UAS: sent 200 OK for INVITE")
		dialogCh <- dialogState{req: req, resp: resp}
	})

	uasServer.OnAck(func(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
		t.Log("UAS: received ACK")
	})

	uasServer.OnBye(func(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
		t.Log("UAS: received BYE")
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		select {
		case byeCh <- struct{}{}:
		default:
		}
	})

	lis, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: uasPort})
	require.NoError(t, err)
	t.Cleanup(func() { lis.Close() })
	go uasServer.ServeTCP(lis)

	uasAddr := fmt.Sprintf("%s:%d", localIP, uasPort)
	t.Logf("UAS listening on %s", uasAddr)

	return &uasConfig{
		ua:        ua,
		server:    uasServer,
		client:    uasClient,
		localIP:   localIP,
		port:      uasPort,
		addr:      uasAddr,
		createSDP: createSDP,
		dialogCh:  dialogCh,
		byeCh:     byeCh,
	}
}

// sendReInvite constructs and sends a mid-dialog re-INVITE from the UAS back
// to the SIP service, returning the client transaction and the request.
func (u *uasConfig) sendReInvite(t *testing.T, dlg dialogState, remoteNumber string) (sip.ClientTransaction, *sip.Request, error) {
	t.Helper()

	recipient := sip.Uri{Host: u.localIP.String(), Port: 5060}
	if contact := dlg.req.Contact(); contact != nil {
		recipient = contact.Address
		if recipient.Port == 0 {
			recipient.Port = 5060
		}
	}

	reinviteReq := sip.NewRequest(sip.INVITE, recipient)
	reinviteReq.SetDestination(dlg.req.Source())
	reinviteReq.SetBody(u.createSDP())

	reinviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	reinviteReq.AppendHeader(sip.NewHeader("Contact",
		fmt.Sprintf("<sip:%s@%s:%d>", remoteNumber, u.localIP, u.port)))

	// From = our identity (To from 200 OK, which has our tag)
	ourToHdr := dlg.resp.To()
	reinviteReq.AppendHeader(&sip.FromHeader{
		DisplayName: ourToHdr.DisplayName,
		Address:     ourToHdr.Address,
		Params:      ourToHdr.Params,
	})

	// To = remote identity (From from original INVITE, which has their tag)
	remoteFromHdr := dlg.req.From()
	reinviteReq.AppendHeader(&sip.ToHeader{
		DisplayName: remoteFromHdr.DisplayName,
		Address:     remoteFromHdr.Address,
		Params:      remoteFromHdr.Params,
	})

	// Same Call-ID as the original dialog
	callIDHdr := sip.CallIDHeader(dlg.req.CallID().Value())
	reinviteReq.AppendHeader(&callIDHdr)

	// CSeq incremented
	reinviteReq.AppendHeader(&sip.CSeqHeader{
		SeqNo:      2,
		MethodName: sip.INVITE,
	})

	tx, err := u.client.TransactionRequest(reinviteReq)
	return tx, reinviteReq, err
}

// waitForFinalResponse waits for a non-provisional SIP response on the
// transaction, returning it. Returns nil if the context expires or the
// transaction terminates without a final response.
func waitForFinalResponse(t *testing.T, tx sip.ClientTransaction, timeout time.Duration) *sip.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tx.Done():
			return nil
		case r := <-tx.Responses():
			if r.StatusCode == 100 || r.StatusCode == 180 || r.StatusCode == 183 {
				continue
			}
			return r
		}
	}
}

// TestSIPOutboundReInvite reproduces the bug where mid-dialog re-INVITEs on
// outbound calls are misidentified as new inbound calls.
//
// The test:
//  1. Creates a standalone SIP UAS (using sipgo directly) that can accept calls
//  2. Makes the SIP service initiate an outbound call to this UAS
//  3. After the call establishes, the UAS sends a re-INVITE back
//  4. Asserts the re-INVITE gets 200 OK (handled within existing dialog)
//
// Without the outbound re-INVITE fix, the SIP server's Server.onInvite() will
// not find the To tag in its inbound dialog map (byLocalTag), and will not
// delegate to the Client. It will instead try to process the re-INVITE as a
// new inbound call, which will fail (no matching trunk/rule for that direction).
func TestSIPOutboundReInvite(t *testing.T) {
	lk := runLiveKit(t)

	const (
		roomName     = "test-outbound-reinvite"
		outIdentity  = "siptest_outbound"
		outName      = "Outbound Call"
		outMeta      = `{"test":true}`
		remoteNumber = "+222222222"
	)

	srv := runSIPServer(t, lk)
	uas := setupUAS(t, remoteNumber)

	// --- Create outbound trunk and initiate the call ---

	trunkOut := srv.CreateTrunkOut(t, &livekit.SIPOutboundTrunkInfo{
		Name:      "Test Outbound ReInvite",
		Numbers:   []string{clientNumber},
		Address:   uas.addr,
		Transport: livekit.SIPTransport_SIP_TRANSPORT_TCP,
	})
	t.Cleanup(func() {
		srv.DeleteTrunk(t, trunkOut)
	})

	// Initiate the outbound call.
	lk.LiveKit.CreateSIPParticipant(t, &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          trunkOut,
		SipCallTo:           remoteNumber,
		RoomName:            roomName,
		ParticipantIdentity: outIdentity,
		ParticipantName:     outName,
		ParticipantMetadata: outMeta,
	})

	// Wait for the UAS to receive and accept the INVITE.
	var dlg dialogState
	select {
	case dlg = <-uas.dialogCh:
		t.Log("Dialog established")
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for INVITE from SIP service")
	}

	// Wait for the outbound SIP participant to appear in the room.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{
			Identity: outIdentity,
			Name:     outName,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: outMeta,
			Attributes: map[string]string{
				"sip.callID":     lktest.AttrTestAny,
				"sip.callStatus": "active",
			},
		},
	})

	// Wait for WebRTC to stabilize.
	time.Sleep(webrtcSetupDelay)

	// --- Send a mid-dialog re-INVITE from UAS back to the SIP service ---

	t.Log("Sending re-INVITE from UAS to outbound call")

	tx, reinviteReq, err := uas.sendReInvite(t, dlg, remoteNumber)
	require.NoError(t, err, "re-INVITE transaction should not fail")
	defer tx.Terminate()

	resp := waitForFinalResponse(t, tx, 10*time.Second)
	require.NotNil(t, resp, "expected a final response to re-INVITE, got timeout")

	// This is the critical assertion:
	// - With the fix: 200 OK (re-INVITE handled within existing outbound dialog)
	// - Without the fix: timeout or error (re-INVITE treated as new inbound call)
	t.Logf("re-INVITE response: %d %s", resp.StatusCode, resp.Reason)
	require.Equal(t, 200, int(resp.StatusCode),
		"re-INVITE should be accepted with 200 OK, but got %d — "+
			"this means the re-INVITE was NOT routed to the existing outbound dialog",
		resp.StatusCode)

	// Verify the response contains SDP.
	require.NotEmpty(t, resp.Body(), "re-INVITE response should contain SDP")
	ctHeader := resp.GetHeader("Content-Type")
	require.NotNil(t, ctHeader, "re-INVITE response should have Content-Type")
	require.Equal(t, "application/sdp", ctHeader.Value())

	// ACK the 200 OK.
	ackReq := sip.NewAckRequest(reinviteReq, resp, nil)
	err = uas.client.WriteRequest(ackReq)
	require.NoError(t, err, "ACK for re-INVITE should not fail")

	t.Log("re-INVITE accepted, verifying call is still active")

	// Verify the outbound call is still active (not replaced by a new call).
	ctx, cancel = context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{
			Identity: outIdentity,
			Name:     outName,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: outMeta,
			Attributes: map[string]string{
				"sip.callID":     lktest.AttrTestAny,
				"sip.callStatus": "active",
			},
		},
	})

	t.Log("PASS: outbound re-INVITE handled correctly within existing dialog")
}

// TestSIPOutboundReInviteCallDrop reproduces the production scenario where a
// mid-dialog re-INVITE on an outbound call gets a 486 Rejected response and
// the active call is torn down with a BYE.
//
// This happens when an inbound trunk exists that matches the reversed call
// direction (as is common in production with SBC session refresh). The SIP
// service misidentifies the re-INVITE as a new inbound call, processes it
// through the dispatch pipeline, and rejects it — killing the original call.
//
// Production SIP trace:
//
//	INVITE (re-INVITE from SBC) →
//	  ← 100 Processing
//	  ← 180 Ringing
//	  ← 486 Rejected
//	ACK →
//	  ← BYE (original dialog torn down)
//	200 OK (BYE) →
func TestSIPOutboundReInviteCallDrop(t *testing.T) {
	lk := runLiveKit(t)

	const (
		roomName     = "test-outbound-reinvite-drop"
		outIdentity  = "siptest_outbound_drop"
		outName      = "Outbound Call Drop"
		outMeta      = `{"test":true}`
		remoteNumber = "+333333333"
		inboundRoom  = "inbound-reinvite"
	)

	srv := runSIPServer(t, lk)
	uas := setupUAS(t, remoteNumber)

	// --- Create an inbound trunk that matches the reversed direction ---
	//
	// In production, the SBC that sends the re-INVITE is also configured as
	// an inbound trunk. When the re-INVITE arrives with From: remoteNumber
	// and To: clientNumber, it matches this inbound trunk and gets processed
	// as a new inbound call (with dispatch, ringing, etc.) instead of being
	// recognized as a mid-dialog re-INVITE for the existing outbound call.
	trunkIn := srv.CreateTrunkIn(t, &livekit.SIPInboundTrunkInfo{
		Name:    "Test Inbound (triggers 486 on re-INVITE)",
		Numbers: []string{clientNumber},
	})
	t.Cleanup(func() {
		srv.DeleteTrunk(t, trunkIn)
	})

	ruleIn := srv.CreateDirectDispatch(t, inboundRoom, "", "", nil)
	t.Cleanup(func() {
		srv.DeleteDispatch(t, ruleIn)
	})

	// --- Create outbound trunk and initiate the call ---

	trunkOut := srv.CreateTrunkOut(t, &livekit.SIPOutboundTrunkInfo{
		Name:      "Test Outbound ReInvite Drop",
		Numbers:   []string{clientNumber},
		Address:   uas.addr,
		Transport: livekit.SIPTransport_SIP_TRANSPORT_TCP,
	})
	t.Cleanup(func() {
		srv.DeleteTrunk(t, trunkOut)
	})

	// Initiate the outbound call.
	lk.LiveKit.CreateSIPParticipant(t, &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          trunkOut,
		SipCallTo:           remoteNumber,
		RoomName:            roomName,
		ParticipantIdentity: outIdentity,
		ParticipantName:     outName,
		ParticipantMetadata: outMeta,
	})

	// Wait for the UAS to receive and accept the INVITE.
	var dlg dialogState
	select {
	case dlg = <-uas.dialogCh:
		t.Log("Dialog established")
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for INVITE from SIP service")
	}

	// Wait for the outbound SIP participant to appear in the room.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{
			Identity: outIdentity,
			Name:     outName,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: outMeta,
			Attributes: map[string]string{
				"sip.callID":     lktest.AttrTestAny,
				"sip.callStatus": "active",
			},
		},
	})

	// Wait for WebRTC to stabilize.
	time.Sleep(webrtcSetupDelay)

	// --- Send a mid-dialog re-INVITE from UAS back to the SIP service ---
	//
	// With a matching inbound trunk, the SIP service will:
	//   1. Accept the re-INVITE as a new inbound call (100 Processing)
	//   2. Start ringing (180 Ringing)
	//   3. Reject it with 486 Busy Here (because the identity conflicts
	//      with the existing outbound participant in the room)
	//   4. Send BYE on the original dialog, killing the outbound call

	t.Log("Sending re-INVITE from UAS to outbound call (with matching inbound trunk)")

	tx, _, err := uas.sendReInvite(t, dlg, remoteNumber)
	require.NoError(t, err, "re-INVITE transaction should not fail")
	defer tx.Terminate()

	resp := waitForFinalResponse(t, tx, 15*time.Second)

	// With the fix: 200 OK (re-INVITE handled within existing outbound dialog)
	// Without the fix: 486 Rejected (re-INVITE misidentified as new inbound call)
	if resp != nil {
		t.Logf("re-INVITE response: %d %s", resp.StatusCode, resp.Reason)
		require.Equal(t, 200, int(resp.StatusCode),
			"re-INVITE should be accepted with 200 OK, but got %d %s — "+
				"this means the re-INVITE was processed as a new inbound call "+
				"and rejected, which will kill the outbound call",
			resp.StatusCode, resp.Reason)
	} else {
		t.Fatal("Timeout waiting for re-INVITE response — " +
			"the re-INVITE was silently dropped (no matching inbound trunk path)")
	}

	// If we got here with a 200 OK (fix is present), verify the call is still active.
	ctx, cancel = context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{
			Identity: outIdentity,
			Name:     outName,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: outMeta,
			Attributes: map[string]string{
				"sip.callID":     lktest.AttrTestAny,
				"sip.callStatus": "active",
			},
		},
	})

	t.Log("PASS: outbound re-INVITE handled correctly, call still active")
}
