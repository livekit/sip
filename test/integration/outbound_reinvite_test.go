package integration

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
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

	// --- Build a standalone SIP UAS using sipgo ---

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

	// Channel to receive dialog state from the OnInvite handler.
	type dialogState struct {
		req  *sip.Request
		resp *sip.Response
	}
	dialogCh := make(chan dialogState, 1)

	// Create a minimal SDP answer for the UAS.
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

	// Handle incoming INVITE (the outbound call from the SIP service).
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

	// Handle ACK (required for the dialog to complete).
	uasServer.OnAck(func(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
		t.Log("UAS: received ACK")
	})

	// Handle BYE.
	uasServer.OnBye(func(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction) {
		t.Log("UAS: received BYE")
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	// Start the UAS TCP listener.
	lis, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: uasPort})
	require.NoError(t, err)
	t.Cleanup(func() { lis.Close() })
	go uasServer.ServeTCP(lis)

	uasAddr := fmt.Sprintf("%s:%d", localIP, uasPort)
	t.Logf("UAS listening on %s", uasAddr)

	// --- Create outbound trunk and initiate the call ---

	trunkOut := srv.CreateTrunkOut(t, &livekit.SIPOutboundTrunkInfo{
		Name:      "Test Outbound ReInvite",
		Numbers:   []string{clientNumber},
		Address:   uasAddr,
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
	case dlg = <-dialogCh:
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
	// This is the key part of the test. We construct a re-INVITE within the
	// existing dialog (same Call-ID, proper From/To tags) and send it back.
	//
	// The From/To headers are swapped relative to the original INVITE:
	//   - Our From = the To from the original INVITE (the SIP service's identity) + our tag
	//     Actually for a re-INVITE from UAS:
	//     - From = original request's To (the called party = us) with our tag from the 200 OK
	//     - To = original request's From (the caller = SIP service) with their tag

	t.Log("Sending re-INVITE from UAS to outbound call")

	// Determine the Request-URI from the Contact header of the incoming INVITE.
	recipient := sip.Uri{Host: localIP.String(), Port: 5060}
	if contact := dlg.req.Contact(); contact != nil {
		recipient = contact.Address
		if recipient.Port == 0 {
			recipient.Port = 5060
		}
	}

	reinviteReq := sip.NewRequest(sip.INVITE, recipient)
	reinviteReq.SetDestination(dlg.req.Source())
	reinviteReq.SetBody(createSDP())

	reinviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	reinviteReq.AppendHeader(sip.NewHeader("Contact",
		fmt.Sprintf("<sip:%s@%s:%d>", remoteNumber, localIP, uasPort)))

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

	tx, err := uasClient.TransactionRequest(reinviteReq)
	require.NoError(t, err, "re-INVITE transaction should not fail")
	defer tx.Terminate()

	// Wait for the response.
	var resp *sip.Response
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	for {
		select {
		case <-waitCtx.Done():
			t.Fatal("Timeout waiting for re-INVITE response")
		case <-tx.Done():
			t.Fatal("re-INVITE transaction failed without response")
		case r := <-tx.Responses():
			if r.StatusCode == 100 || r.StatusCode == 180 || r.StatusCode == 183 {
				continue // skip provisional responses
			}
			resp = r
		}
		if resp != nil {
			break
		}
	}

	// This is the critical assertion:
	// - With the fix: 200 OK (re-INVITE handled within existing outbound dialog)
	// - Without the fix: error response (re-INVITE treated as new inbound call, no matching trunk)
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
	err = uasClient.WriteRequest(ackReq)
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
