package integration

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/tracer/jaeger"
	"github.com/livekit/psrpc/pkg/middleware/otelpsrpc"
	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/g722"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	sipgo "github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/pkg/siptest"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/test/lktest"
)

type SIPServer struct {
	LiveKit *LiveKit
	Client  *lksdk.SIPClient
	IP      netip.Addr
	Address string
	Host    string
}

func runSIPServer(t testing.TB, lk *LiveKit) *SIPServer {
	rc, err := redis.GetRedisClient(lk.Redis)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rc.Close()
	})

	sipPort := 5060 + rand.Intn(1000)
	local, err := config.GetLocalIP()
	require.NoError(t, err)
	conf := &config.Config{
		ServiceName:        "sip",
		NodeID:             utils.NewGuid("NS_"),
		ApiKey:             lk.ApiKey,
		ApiSecret:          lk.ApiSecret,
		WsUrl:              lk.WsUrl,
		Redis:              lk.Redis,
		SIPPort:            sipPort,
		SIPPortListen:      sipPort,
		ListenIP:           local.String(),
		LocalNet:           local.String() + "/24",
		RTPPort:            rtcconfig.PortRange{Start: 20000, End: 20010},
		UseExternalIP:      false,
		MaxCpuUtilization:  0.9,
		Logging:            logger.Config{Level: "debug"},
		EnableJitterBuffer: true,
		JaegerURL:          os.Getenv("JAEGER_URL"),
	}
	_ = conf.InitLogger()
	log := logger.GetLogger()
	if conf.JaegerURL != "" {
		jaeger.Configure(t.Context(), conf.JaegerURL, conf.ServiceName)
	}

	bus := psrpc.NewRedisMessageBus(rc)
	psrpcCli, err := rpc.NewIOInfoClient(bus,
		otelpsrpc.ClientOptions(otelpsrpc.Config{}),
	)
	if err != nil {
		t.Fatal(err)
	}

	mon, err := stats.NewMonitor(conf)
	if err != nil {
		t.Fatal(err)
	}
	sipsrv, err := sip.NewService("", conf, mon, log, func(projectID string) rpc.IOInfoClient { return psrpcCli })
	if err != nil {
		t.Fatal(err)
	}

	svc := service.NewService(conf, log, sipsrv, sipsrv.Stop, sipsrv.ActiveCalls, psrpcCli, bus, mon)
	sipsrv.SetHandler(svc)
	t.Cleanup(func() {
		svc.Stop(true)
	})

	if err = sipsrv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sipsrv.Stop)

	go func() {
		if err := svc.Run(); err != nil {
			t.Fatal(err)
		}
	}()
	time.Sleep(time.Second * 2)

	// TODO: If we try to dial localhost here, the first packet will go to 127.0.0.1, while the server will
	//       respond from an IP that was selected above. This breaks the SIP client because it uses net.DialUDP,
	//       which in turn only accepts UDP from the address used in DialUDP.
	addr := local
	return &SIPServer{
		LiveKit: lk,
		Client:  lksdk.NewSIPClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret),
		IP:      addr,
		Address: fmt.Sprintf("%s:%d", addr, conf.SIPPort),
		Host:    "sip.local",
	}
}

type NumberConfig struct {
	SIP      *SIPServer
	TrunkID  string
	RuleID   string
	Number   string
	Pin      string
	AuthUser string
	AuthPass string
}

func (s *SIPServer) CreateTrunkOut(t testing.TB, trunk *livekit.SIPOutboundTrunkInfo) string {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPOutboundTrunk(ctx, &livekit.CreateSIPOutboundTrunkRequest{
		Trunk: trunk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("New trunk (outbound):", tr.SipTrunkId)
	return tr.SipTrunkId
}

func (s *SIPServer) CreateTrunkIn(t testing.TB, trunk *livekit.SIPInboundTrunkInfo) string {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{
		Trunk: trunk,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("New trunk (inbound):", tr.SipTrunkId)
	return tr.SipTrunkId
}

func (s *SIPServer) DeleteTrunk(t testing.TB, id string) {
	ctx := context.Background()
	_, err := s.Client.DeleteSIPTrunk(ctx, &livekit.DeleteSIPTrunkRequest{
		SipTrunkId: id,
	})
	if err != nil {
		t.Fatal(id, err)
	}
}

func (s *SIPServer) CreateTrunkAndDirect(t testing.TB, trunk *livekit.SIPInboundTrunkInfo, room, pin string, meta string, attrs map[string]string) *NumberConfig {
	trunkID := s.CreateTrunkIn(t, trunk)
	ruleID := s.CreateDirectDispatch(t, room, pin, meta, attrs)
	return &NumberConfig{
		SIP:     s,
		TrunkID: trunkID, RuleID: ruleID,
		Number: trunk.Numbers[0], Pin: pin,
	}
}

func (s *SIPServer) CreateTrunkAndIndividual(t testing.TB, trunk *livekit.SIPInboundTrunkInfo, room, pin string, meta string, attrs map[string]string) *NumberConfig {
	trunkID := s.CreateTrunkIn(t, trunk)
	ruleID := s.CreateIndividualDispatch(t, room, pin, meta, attrs)
	return &NumberConfig{
		SIP:     s,
		TrunkID: trunkID, RuleID: ruleID,
		Number: trunk.Numbers[0], Pin: pin,
	}
}

func (s *SIPServer) CreateDirectDispatch(t testing.TB, room, pin string, meta string, attrs map[string]string) string {
	ctx := context.Background()
	dr, err := s.Client.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
		Name:       room,
		Metadata:   meta,
		Attributes: attrs,
		Rule: &livekit.SIPDispatchRule{
			Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
				DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{
					RoomName: room, Pin: pin,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("New dispatch rule (direct):", dr.SipDispatchRuleId)
	return dr.SipDispatchRuleId
}

func (s *SIPServer) CreateIndividualDispatch(t testing.TB, pref, pin string, meta string, attrs map[string]string) string {
	ctx := context.Background()
	dr, err := s.Client.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
		Metadata:   meta,
		Attributes: attrs,
		Rule: &livekit.SIPDispatchRule{
			Rule: &livekit.SIPDispatchRule_DispatchRuleIndividual{
				DispatchRuleIndividual: &livekit.SIPDispatchRuleIndividual{
					RoomPrefix: pref, Pin: pin,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Dispatch (individual):", dr.SipDispatchRuleId)
	return dr.SipDispatchRuleId
}

func (s *SIPServer) DeleteDispatch(t testing.TB, id string) {
	ctx := context.Background()
	_, err := s.Client.DeleteSIPDispatchRule(ctx, &livekit.DeleteSIPDispatchRuleRequest{
		SipDispatchRuleId: id,
	})
	if err != nil {
		t.Fatal(id, err)
	}
}

func runClient(t testing.TB, conf *NumberConfig, ip netip.Addr, id string, number string, forcePin bool, headers map[string]string, onDTMF func(ev dtmf.Event), onBye func(), onRefer func(req *sipgo.Request)) *siptest.Client {
	return runClientWithCodec(t, conf, ip, id, number, "", forcePin, headers, onDTMF, onBye, onRefer)
}

func runClientWithCodec(t testing.TB, conf *NumberConfig, ip netip.Addr, id, number string, codec string, forcePin bool, headers map[string]string, onDTMF func(ev dtmf.Event), onBye func(), onRefer func(req *sipgo.Request)) *siptest.Client {
	cconf := siptest.ClientConfig{
		// IP: dockerBridgeIP,
		IP:       ip,
		Number:   number,
		AuthUser: conf.AuthUser,
		AuthPass: conf.AuthPass,
		Codec:    codec,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnMediaTimeout: func() {
			t.Fatal("media timeout from server to test client")
		},
		OnDTMF:  onDTMF,
		OnBye:   onBye,
		OnRefer: onRefer,
	}

	cli, err := siptest.NewClient(id, cconf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.Close)

	err = cli.Dial(conf.SIP.Address, conf.SIP.Host, conf.Number, headers)
	if err != nil {
		t.Fatal(err)
	}
	if conf.Pin != "" || forcePin {
		err = cli.SendDTMF(conf.Pin + "#")
		if err != nil {
			t.Fatal(err)
		}
	}
	return cli
}

const (
	serverNumber                   = "+000000000"
	clientNumber                   = "+111111111"
	transferNumber                 = "+222222222"
	participantsJoinTimeout        = 5 * time.Second
	participantsJoinWithPinTimeout = participantsJoinTimeout + 5*time.Second
	participantsLeaveTimeout       = 3 * time.Second
	webrtcSetupDelay               = 5 * time.Second
	notifyIntervalDelay            = 100 * time.Millisecond
)

func TestSIPJoinOpenRoom(t *testing.T) {
	lk := runLiveKit(t)
	var (
		dmu          sync.Mutex
		dtmfOut      string
		dtmfIn       string
		referRequest *sipgo.Request
	)
	const (
		clientID   = "test-cli"
		roomName   = "test-open"
		meta       = `{"test":true}`
		customAttr = "my.attr"
		customVal  = "custom"
	)
	r := lk.ConnectWithAudio(t, roomName, "test", &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				switch data := data.(type) {
				case *livekit.SipDTMF:
					dmu.Lock()
					dtmfOut += data.Digit
					dmu.Unlock()
				}
			},
		},
	})
	srv := runSIPServer(t, lk)

	nc := srv.CreateTrunkAndDirect(t, &livekit.SIPInboundTrunkInfo{
		Numbers: []string{serverNumber},
		Headers: map[string]string{
			"X-LK-Accepted": "1",
		},
		HeadersToAttributes: map[string]string{
			"X-LK-Inbound": "test.lk.inbound",
		},
	}, roomName, "", meta, map[string]string{
		customAttr: customVal,
	})

	transferDone := make(chan struct{})
	byeReceived := make(chan struct{})

	cli := runClient(t, nc, srv.IP, clientID, clientNumber, false, map[string]string{
		"X-LK-Inbound": "1",
	}, func(ev dtmf.Event) {
		dmu.Lock()
		defer dmu.Unlock()
		dtmfIn += string(ev.Digit)
	}, func() {
		close(byeReceived)
	}, func(req *sipgo.Request) {
		dmu.Lock()
		defer dmu.Unlock()
		referRequest = req
	})

	h := sip.Headers(cli.RemoteHeaders()).GetHeader("X-LK-Accepted")
	require.NotNil(t, h)
	require.Equal(t, "1", h.Value())

	// Send audio, so that we don't trigger media timeout.
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	go cli.SendSilence(mctx)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{Identity: "test"},
		{
			Identity: "sip_" + clientNumber,
			Name:     "Phone " + clientNumber,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: meta,
			Attributes: map[string]string{
				"sip.callID":           lktest.AttrTestAny, // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
				"test.lk.inbound":      "1", // from SIP headers
				customAttr:             customVal,
			},
		},
	})

	// Wait for WebRTC to come online.
	time.Sleep(webrtcSetupDelay)

	// Test that we can send DTMF data to LK participants.
	const dtmfDigits = "*111#"
	err := cli.SendDTMF(dtmfDigits)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return dtmfOut == dtmfDigits
	}, 5*time.Second, time.Second/2)

	err = r.LocalParticipant.PublishDataPacket(&livekit.SipDTMF{Digit: "4567"}, lksdk.WithDataPublishReliable(true))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return dtmfIn == "4567"
	}, 5*time.Second, time.Second/2)

	go func() {
		// TransferSIPParticipant is synchronous
		_, err = lk.SIP.TransferSIPParticipant(context.Background(), &livekit.TransferSIPParticipantRequest{
			RoomName:            roomName,
			ParticipantIdentity: "sip_" + clientNumber,
			TransferTo:          "tel:" + transferNumber,
		})
		require.NoError(t, err)
		close(transferDone)
	}()

	require.Eventually(t, func() bool {
		dmu.Lock()
		defer dmu.Unlock()

		return referRequest != nil

	}, 5*time.Second, time.Second/2)

	require.Equal(t, sipgo.REFER, referRequest.Method)
	transferTo := referRequest.GetHeader("Refer-To")
	require.Equal(t, "<tel:"+transferNumber+">", transferTo.Value())

	time.Sleep(notifyIntervalDelay)
	err = cli.SendNotify(referRequest, "SIP/2.0 100 Trying")
	require.NoError(t, err)

	time.Sleep(notifyIntervalDelay)
	err = cli.SendNotify(referRequest, "SIP/2.0 200 OK")
	require.NoError(t, err)

	select {
	case <-transferDone:
	case <-time.After(participantsLeaveTimeout):
		t.Fatal("participant transfer call never completed")
	}

	select {
	case <-byeReceived:
	case <-time.After(participantsLeaveTimeout):
		t.Fatal("did not receive bye after notify")
	}

	cli.Close()
	r.Disconnect()

	// SIP participant should have left
	ctx, cancel = context.WithTimeout(context.Background(), participantsLeaveTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, nil)
}

func TestSIPJoinPinRoom(t *testing.T) {
	lk := runLiveKit(t)
	var (
		dmu          sync.Mutex
		dtmf         string
		referRequest *sipgo.Request
	)
	const (
		clientID   = "test-cli"
		roomName   = "test-priv"
		meta       = `{"test":true}`
		customAttr = "my.attr"
		customVal  = "custom"
	)
	r := lk.Connect(t, roomName, "test", &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				switch data := data.(type) {
				case *livekit.SipDTMF:
					dmu.Lock()
					dtmf += data.Digit
					dmu.Unlock()
				}
			},
		},
	})
	srv := runSIPServer(t, lk)

	nc := srv.CreateTrunkAndDirect(t, &livekit.SIPInboundTrunkInfo{
		Numbers: []string{serverNumber},
		Headers: map[string]string{
			"X-LK-Accepted": "1",
		},
		HeadersToAttributes: map[string]string{
			"X-LK-Inbound": "test.lk.inbound",
		},
	}, roomName, "1234", meta, map[string]string{
		customAttr: customVal,
	})

	transferDone := make(chan struct{})

	cli := runClient(t, nc, srv.IP, clientID, clientNumber, false, map[string]string{
		"X-LK-Inbound": "1",
	}, nil, nil, func(req *sipgo.Request) {
		dmu.Lock()
		defer dmu.Unlock()
		referRequest = req
	})

	// Even though we set this header in the dispatch rule, PIN forces us to send response earlier.
	// Because of this, we can no longer attach attributes from a selected dispatch rule later.
	h := sip.Headers(cli.RemoteHeaders()).GetHeader("X-LK-Accepted")
	require.Nil(t, h)

	// Send audio, so that we don't trigger media timeout.
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	go cli.SendSilence(mctx)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	// This needs additional time for the "enter pin" message to end.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinWithPinTimeout)
	defer cancel()

	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{Identity: "test"},
		{
			Identity: "sip_" + clientNumber,
			Name:     "Phone " + clientNumber,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: meta,
			Attributes: map[string]string{
				"sip.callID":           lktest.AttrTestAny, // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
				"test.lk.inbound":      "1", // from SIP headers
				customAttr:             customVal,
			},
		},
	})

	// Wait for WebRTC to come online.
	time.Sleep(webrtcSetupDelay)

	// Stop sending audio. We need it for DTMF tones now.
	cancel()

	// Test that we can send DTMF data to LK participants.
	const dtmfDigits = "*111#"
	err := cli.SendDTMF(dtmfDigits)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		dmu.Lock()
		defer dmu.Unlock()
		return dtmf == dtmfDigits
	}, 5*time.Second, time.Second/2)

	go func() {
		// TransferSIPParticipant is synchronous
		_, err = lk.SIP.TransferSIPParticipant(context.Background(), &livekit.TransferSIPParticipantRequest{
			RoomName:            "test-priv",
			ParticipantIdentity: "sip_" + clientNumber,
			TransferTo:          "tel:" + transferNumber,
		})
		require.Error(t, err)
		close(transferDone)
	}()

	require.Eventually(t, func() bool {
		dmu.Lock()
		defer dmu.Unlock()

		return referRequest != nil

	}, 5*time.Second, time.Second/2)

	require.Equal(t, sipgo.REFER, referRequest.Method)
	transferTo := referRequest.GetHeader("Refer-To")
	require.Equal(t, "<tel:"+transferNumber+">", transferTo.Value())

	time.Sleep(notifyIntervalDelay)
	err = cli.SendNotify(referRequest, "SIP/2.0 403 Fobidden")
	require.NoError(t, err)

	select {
	case <-transferDone:
	case <-time.After(participantsLeaveTimeout):
		t.Fatal("participant transfer call never completed")
	}

	// Participants should all still be there
	time.Sleep(time.Second)
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{Identity: "test"},
		{
			Identity: "sip_" + clientNumber,
			Name:     "Phone " + clientNumber,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: meta,
			Attributes: map[string]string{
				"sip.callID":           lktest.AttrTestAny, // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
				"test.lk.inbound":      "1", // from SIP headers
				customAttr:             customVal,
			},
		},
	})

	cli.Close()
	r.Disconnect()

	// SIP participant must disconnect from LK room on hangup.
	ctx, cancel = context.WithTimeout(context.Background(), participantsLeaveTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, nil)
}

func TestSIPJoinOpenRoomWithPin(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const (
		clientID   = "test-cli"
		roomName   = "test-open"
		meta       = `{"test":true}`
		customAttr = "my.attr"
		customVal  = "custom"
	)
	nc := srv.CreateTrunkAndDirect(t, &livekit.SIPInboundTrunkInfo{
		Numbers: []string{serverNumber},
	}, roomName, "", meta, map[string]string{
		customAttr: customVal,
	})
	srv.CreateDirectDispatch(t, "test-priv", "1234", "", nil)

	cli := runClient(t, nc, srv.IP, clientID, clientNumber, true, nil, nil, nil, nil)

	// Send audio, so that we don't trigger media timeout.
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	go cli.SendSilence(mctx)

	// This needs additional time for the "enter pin" message to end.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinWithPinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []lktest.ParticipantInfo{
		{
			Identity: "sip_" + clientNumber,
			Name:     "Phone " + clientNumber,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: meta,
			Attributes: map[string]string{
				"sip.callID":           lktest.AttrTestAny, // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
				customAttr:             customVal,
			},
		},
	})
}

func TestSIPJoinRoomIndividual(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const (
		clientID   = "test-cli"
		roomPref   = "test-pref"
		meta       = `{"test":true}`
		customAttr = "my.attr"
		customVal  = "custom"
	)

	nc := srv.CreateTrunkAndIndividual(t, &livekit.SIPInboundTrunkInfo{
		Numbers: []string{serverNumber},
	}, roomPref, "", meta, map[string]string{
		customAttr: customVal,
	})

	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()

	// runClient waits for SIP to completely dial, but this won't happen until we connect
	// another participant to that room.
	// So we have to monitor rooms separately and connect participant as soon as there's a room with our prefix.
	rch := make(chan *livekit.Room, 1)
	go func() {
		defer close(rch)
		room := lk.ExpectRoomPref(t, ctx, roomPref, clientNumber, false)
		lk.ConnectWithAudio(t, room.Name, "test", nil)
		rch <- room
	}()

	cli := runClient(t, nc, srv.IP, clientID, clientNumber, false, nil, nil, nil, nil)

	// Send audio, so that we don't trigger media timeout.
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	go cli.SendSilence(mctx)

	// Room should be created automatically with exact prefix containing phone number.
	// SIP participant should be visible and have a proper kind.
	select {
	case <-ctx.Done():
		t.Fatal("cannot find the room")
	case room := <-rch:
		lk.ExpectParticipants(t, ctx, room.Name, []lktest.ParticipantInfo{
			{
				Identity: "test",
				Kind:     livekit.ParticipantInfo_STANDARD,
			},
			{
				Identity: "sip_" + clientNumber,
				Name:     "Phone " + clientNumber,
				Kind:     livekit.ParticipantInfo_SIP,
				Metadata: meta,
				Attributes: map[string]string{
					"sip.callID":           lktest.AttrTestAny, // special case
					"sip.callStatus":       "active",
					"sip.trunkPhoneNumber": serverNumber,
					"sip.phoneNumber":      clientNumber,
					"sip.ruleID":           nc.RuleID,
					"sip.trunkID":          nc.TrunkID,
					"lktest.id":            clientID,
					customAttr:             customVal,
				},
			},
		})
	}
}

func TestSIPAudio(t *testing.T) {
	for _, codec := range []string{
		g711.ULawSDPName,
		g722.SDPName,
	} {
		codec := codec
		t.Run(codec, func(t *testing.T) {
			for _, N := range []int{2, 3} {
				N := N
				t.Run(fmt.Sprintf("%d clients", N), func(t *testing.T) {
					lk := runLiveKit(t)
					srv := runSIPServer(t, lk)

					const (
						roomName   = "test-open"
						meta       = `{"test":true}`
						customAttr = "my.attr"
						customVal  = "custom"
					)
					nc := srv.CreateTrunkAndDirect(t, &livekit.SIPInboundTrunkInfo{
						Numbers: []string{serverNumber},
					}, roomName, "", meta, map[string]string{
						customAttr: customVal,
					})

					// Connect clients and wait for them to join.
					var (
						wg      sync.WaitGroup
						mu      sync.Mutex
						clients = make([]*siptest.Client, N)
						audios  = make([]lktest.AudioParticipant, N)
					)
					for i := 0; i < N; i++ {
						codec := codec
						if i == 0 {
							// Make first client always use the same codec.
							// This way we can see how different codecs interact.
							codec = g711.ULawSDPName
						}
						wg.Add(1)
						go func() {
							defer wg.Done()
							cli := runClientWithCodec(t, nc, srv.IP, strconv.Itoa(i+1), fmt.Sprintf("+%d", 111111111*(i+1)), codec, false, nil, nil, nil, nil)
							mu.Lock()
							clients[i] = cli
							audios[i] = cli
							mu.Unlock()
						}()
					}
					wg.Wait()
					t.Log("Participants dialed")
					ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout*time.Duration(N))
					defer cancel()
					var exp []lktest.ParticipantInfo
					for i := range clients {
						exp = append(exp, lktest.ParticipantInfo{
							Identity: fmt.Sprintf("sip_+%d", 111111111*(i+1)),
							Name:     fmt.Sprintf("Phone +%d", 111111111*(i+1)),
							Kind:     livekit.ParticipantInfo_SIP,
							Metadata: meta,
							Attributes: map[string]string{
								"sip.callID":           lktest.AttrTestAny, // special case
								"sip.callStatus":       "active",
								"sip.trunkPhoneNumber": serverNumber,
								"sip.phoneNumber":      fmt.Sprintf("+%d", 111111111*(i+1)),
								"sip.ruleID":           nc.RuleID,
								"sip.trunkID":          nc.TrunkID,
								"lktest.id":            strconv.Itoa(i + 1),
								customAttr:             customVal,
							},
						})
					}
					lk.ExpectRoomWithParticipants(t, ctx, roomName, exp)
					t.Log("Participants join confirmed, testing audio")

					ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					lktest.CheckAudioForParticipants(t, ctx, audios...)
					cancel()

					t.Log("Success, cleaning up")

					// Stop everything and ensure the room is empty afterward.
					for _, cli := range clients {
						wg.Add(1)
						go func() {
							defer wg.Done()
							cli.Close()
						}()
					}
					wg.Wait()

					ctx, cancel = context.WithTimeout(context.Background(), participantsLeaveTimeout)
					defer cancel()
					lk.ExpectRoomWithParticipants(t, ctx, roomName, nil)
				})
			}
		})
	}
}

func TestSIPOutbound(t *testing.T) {
	// Run two LK and SIP servers and make a SIP call from one to the other.
	lkOut := runLiveKit(t)
	lkIn := runLiveKit(t)
	srvOut := runSIPServer(t, lkOut)
	srvIn := runSIPServer(t, lkIn)

	const (
		roomIn   = "inbound"
		userName = "test-user"
		userPass = "test-pass"
		roomPin  = "*1234"
		meta     = `{"test":true}`
	)

	for _, tr := range []livekit.SIPTransport{
		livekit.SIPTransport_SIP_TRANSPORT_UDP,
		livekit.SIPTransport_SIP_TRANSPORT_TCP,
	} {
		t.Run(tr.String(), func(t *testing.T) {
			for _, withPin := range []bool{true, false} {
				name := "pin"
				if !withPin {
					name = "open"
				}
				t.Run(name, func(t *testing.T) {
					headersIn := map[string]string{
						"X-LK-From-1": "inbound",
					}
					roomPin := roomPin
					if withPin {
						// We cannot set headers because of the PIN. See TestSIPJoinPinRoom for details.
						delete(headersIn, "X-LK-From-1")
					} else {
						roomPin = ""
					}
					// Configure Trunk for inbound server.
					trunkIn := srvIn.CreateTrunkIn(t, &livekit.SIPInboundTrunkInfo{
						Name:         "Test In",
						Numbers:      []string{serverNumber},
						AuthUsername: userName,
						AuthPassword: userPass,
						Headers:      headersIn,
						HeadersToAttributes: map[string]string{
							"X-LK-From-2": "test.lk.from",
						},
					})
					t.Cleanup(func() {
						srvIn.DeleteTrunk(t, trunkIn)
					})
					ruleIn := srvIn.CreateDirectDispatch(t, roomIn, roomPin, meta, nil)
					t.Cleanup(func() {
						srvIn.DeleteDispatch(t, ruleIn)
					})

					// Configure Trunk for outbound server and make a SIP call.
					trunkOut := srvOut.CreateTrunkOut(t, &livekit.SIPOutboundTrunkInfo{
						Name:         "Test Out",
						Numbers:      []string{clientNumber},
						Address:      srvIn.Address,
						Transport:    tr,
						AuthUsername: userName,
						AuthPassword: userPass,
						Headers: map[string]string{
							"X-LK-From-2": "outbound",
						},
						HeadersToAttributes: map[string]string{
							"X-LK-From-1": "test.lk.from",
						},
					})
					t.Cleanup(func() {
						srvOut.DeleteTrunk(t, trunkOut)
					})

					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()

					expAttrsIn := map[string]string{
						"test.lk.from": "outbound",
					}
					expAttrsOut := map[string]string{
						"test.lk.from": "inbound",
					}
					if withPin {
						delete(expAttrsOut, "test.lk.from")
					}
					// Run the test twice to make sure participants with the same identities can be re-created.
					for i := 0; i < 2; i++ {
						// Running sub test here is important, because TestSIPOutbound registers Cleanup funcs.
						t.Run(fmt.Sprintf("run %d", i+1), func(t *testing.T) {
							lktest.TestSIPOutbound(t, ctx, lkOut.LiveKit, lkIn.LiveKit, lktest.SIPOutboundTestParams{
								TrunkOut: trunkOut,
								RoomOut:  "outbound",
								TrunkIn:  trunkIn,
								RuleIn:   ruleIn,
								AttrsIn:  expAttrsIn,
								AttrsOut: expAttrsOut,
							})
						})
					}
				})
			}
		})
	}
}

func TestSIPOutboundRouteHeader(t *testing.T) {
	// Test that when a Route header is specified in CreateSIPParticipant request,
	// the SIP message is sent to the route header target instead of the request URI.

	// Set up two LiveKit servers and SIP servers
	lkOut := runLiveKit(t)
	lkIn := runLiveKit(t)
	srvOut := runSIPServer(t, lkOut)
	srvIn := runSIPServer(t, lkIn)

	const (
		roomIn   = "inbound"
		userName = "test-user"
		userPass = "test-pass"
		meta     = `{"test":true}`
	)

	// Configure Trunk for inbound server
	trunkIn := srvIn.CreateTrunkIn(t, &livekit.SIPInboundTrunkInfo{
		Name:         "Test In",
		Numbers:      []string{serverNumber},
		AuthUsername: userName,
		AuthPassword: userPass,
	})
	t.Cleanup(func() {
		srvIn.DeleteTrunk(t, trunkIn)
	})

	ruleIn := srvIn.CreateDirectDispatch(t, roomIn, "", meta, nil)
	t.Cleanup(func() {
		srvIn.DeleteDispatch(t, ruleIn)
	})

	// Create a mock SIP server that will receive the route header target
	// This server should be different from the request URI
	routeTarget := "127.0.0.1:5061" // Different from srvIn.Address

	// Set up a mock SIP server to capture the route header target
	// We'll create a simple UDP server that can receive and log the SIP message
	routeServer, err := net.ListenPacket("udp", routeTarget)
	require.NoError(t, err)
	defer routeServer.Close()

	// Channel to capture received messages
	receivedMessages := make(chan string, 10)

	// Start a goroutine to listen for messages on the route target
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, addr, err := routeServer.ReadFrom(buffer)
			if err != nil {
				return
			}
			message := string(buffer[:n])
			receivedMessages <- message
			t.Logf("Route server received message from %s: %s", addr, message)
		}
	}()

	// Configure Trunk for outbound server with the request URI (different from route target)
	trunkOut := srvOut.CreateTrunkOut(t, &livekit.SIPOutboundTrunkInfo{
		Name:         "Test Out",
		Numbers:      []string{clientNumber},
		Address:      srvIn.Address, // This will be the request URI
		Transport:    livekit.SIPTransport_SIP_TRANSPORT_UDP,
		AuthUsername: userName,
		AuthPassword: userPass,
	})
	t.Cleanup(func() {
		srvOut.DeleteTrunk(t, trunkOut)
	})

	// Create the outbound SIP participant with a Route header
	// The Route header should point to a different destination than the request URI
	routeHeader := fmt.Sprintf("<sip:%s;lr>", routeTarget)

	// Create the SIP participant with the Route header
	// We need to pass the Route header in the headers map
	headers := map[string]string{
		"Route": routeHeader,
	}

	// Create the outbound SIP participant with the Route header
	t.Logf("Testing Route header: %s", routeHeader)
	t.Logf("Request URI target: %s", srvIn.Address)
	t.Logf("Route header target: %s", routeTarget)

	// Create the outbound SIP participant
	r := lkOut.CreateSIPParticipant(t, &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          trunkOut,
		SipCallTo:           serverNumber,
		RoomName:            "outbound",
		ParticipantIdentity: "siptest_outbound",
		ParticipantName:     "Outbound Call",
		ParticipantMetadata: `{"test":true, "dir": "out"}`,
		Headers:             headers, // This is the key - passing the Route header
	})
	t.Logf("outbound call ID: %s", r.SipCallId)

	// Wait a bit to see if any messages are received on the route target
	select {
	case msg := <-receivedMessages:
		t.Logf("Received message on route target: %s", msg)
		// If we receive a message, it means the Route header is working
		require.Contains(t, msg, "INVITE", "Should receive INVITE message on route target")
		t.Log("SUCCESS: Route header is working - message was sent to route target instead of request URI")
	case <-time.After(10 * time.Second):
		t.Fatal("No message received on route target within timeout - Route header processing is not working correctly")
	}
}
