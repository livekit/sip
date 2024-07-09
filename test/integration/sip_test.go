package integration

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media/g722"
	"github.com/livekit/sip/pkg/media/ulaw"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/pkg/siptest"
	"github.com/livekit/sip/test/lktest"
)

type SIPServer struct {
	LiveKit *LiveKit
	Client  *lksdk.SIPClient
	Address string
	URI     string
}

func runSIPServer(t testing.TB, lk *LiveKit) *SIPServer {
	rc, err := redis.GetRedisClient(lk.Redis)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rc.Close()
	})

	bus := psrpc.NewRedisMessageBus(rc)
	psrpcCli, err := rpc.NewIOInfoClient(bus)
	if err != nil {
		t.Fatal(err)
	}
	sipPort := 5060 + rand.Intn(100)
	conf := &config.Config{
		ApiKey:        lk.ApiKey,
		ApiSecret:     lk.ApiSecret,
		WsUrl:         lk.WsUrl,
		Redis:         lk.Redis,
		SIPPort:       sipPort,
		RTPPort:       rtcconfig.PortRange{Start: 20000, End: 20010},
		UseExternalIP: false,
		Logging:       logger.Config{Level: "debug"},
	}
	_ = conf.InitLogger()
	log := logger.GetLogger()

	sipsrv, err := sip.NewService(conf, log)
	if err != nil {
		t.Fatal(err)
	}

	svc := service.NewService(conf, log, sipsrv.InternalServerImpl(), sipsrv.Stop, sipsrv.ActiveCalls, psrpcCli, bus)
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

	// TODO: Our local IP selection picks Docker bridge IP be default.
	//       If we try to dial localhost here, the first packet will go to 127.0.0.1, while the server will
	//       respond from Docker bridge IP. This breaks the SIP client because it uses net.DialUDP,
	//       which in turn only accepts UDP from the address used in DialUDP.
	addr := dockerBridgeIP
	return &SIPServer{
		LiveKit: lk,
		Client:  lksdk.NewSIPClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret),
		Address: fmt.Sprintf("%s:%d", addr, conf.SIPPort),
		URI:     "sip.local",
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

func (s *SIPServer) CreateTrunkOut(t testing.TB, number, addr, user, pass string) string {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPOutboundTrunk(ctx, &livekit.CreateSIPOutboundTrunkRequest{
		Trunk: &livekit.SIPOutboundTrunkInfo{
			Address:      addr,
			Numbers:      []string{number},
			AuthUsername: user,
			AuthPassword: pass,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Trunk (out):", tr.SipTrunkId)
	return tr.SipTrunkId
}

func (s *SIPServer) CreateTrunkIn(t testing.TB, number, user, pass string) string {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{
		Trunk: &livekit.SIPInboundTrunkInfo{
			Numbers:      []string{number},
			AuthUsername: user,
			AuthPassword: pass,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Trunk (in):", tr.SipTrunkId)
	return tr.SipTrunkId
}

func (s *SIPServer) CreateTrunkAndDirect(t testing.TB, number, room, pin string, meta string) *NumberConfig {
	trunkID := s.CreateTrunkIn(t, number, "", "")
	ruleID := s.CreateDirectDispatch(t, room, pin, meta)
	return &NumberConfig{
		SIP:     s,
		TrunkID: trunkID, RuleID: ruleID,
		Number: number, Pin: pin,
	}
}

func (s *SIPServer) CreateTrunkAndIndividual(t testing.TB, number, room, pin string, meta string) *NumberConfig {
	trunkID := s.CreateTrunkIn(t, number, "", "")
	ruleID := s.CreateIndividualDispatch(t, room, pin, meta)
	return &NumberConfig{
		SIP:     s,
		TrunkID: trunkID, RuleID: ruleID,
		Number: number, Pin: pin,
	}
}

func (s *SIPServer) CreateDirectDispatch(t testing.TB, room, pin string, meta string) string {
	ctx := context.Background()
	dr, err := s.Client.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
		Metadata: meta,
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
	t.Log("Dispatch (direct):", dr.SipDispatchRuleId)
	return dr.SipDispatchRuleId
}

func (s *SIPServer) CreateIndividualDispatch(t testing.TB, pref, pin string, meta string) string {
	ctx := context.Background()
	dr, err := s.Client.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
		Metadata: meta,
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

func runClient(t testing.TB, conf *NumberConfig, id string, number string, forcePin bool) *siptest.Client {
	return runClientWithCodec(t, conf, id, number, "", forcePin)
}

func runClientWithCodec(t testing.TB, conf *NumberConfig, id string, number string, codec string, forcePin bool) *siptest.Client {
	cli, err := siptest.NewClient(id, siptest.ClientConfig{
		//IP: dockerBridgeIP,
		Number:   number,
		AuthUser: conf.AuthUser,
		AuthPass: conf.AuthPass,
		Codec:    codec,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnMediaTimeout: func() {
			t.Fatal("media timeout")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.Close)

	err = cli.Dial(conf.SIP.Address, conf.SIP.URI, conf.Number)
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
	participantsJoinTimeout        = 5 * time.Second
	participantsJoinWithPinTimeout = participantsJoinTimeout + 5*time.Second
	participantsLeaveTimeout       = 3 * time.Second
	webrtcSetupDelay               = 5 * time.Second
)

func TestSIPJoinOpenRoom(t *testing.T) {
	lk := runLiveKit(t)
	var (
		dmu  sync.Mutex
		dtmf string
	)
	const (
		clientID = "test-cli"
		roomName = "test-open"
		meta     = `{"test":true}`
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

	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "", meta)

	cli := runClient(t, nc, clientID, clientNumber, false)

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
				"sip.callID":           "<test>", // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
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
		return dtmf == dtmfDigits
	}, 5*time.Second, time.Second/2)

	cli.Close()
	r.Disconnect()

	// SIP participant must disconnect from LK room on hangup.
	ctx, cancel = context.WithTimeout(context.Background(), participantsLeaveTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, nil)
}

func TestSIPJoinPinRoom(t *testing.T) {
	lk := runLiveKit(t)
	var (
		dmu  sync.Mutex
		dtmf string
	)
	const (
		clientID = "test-cli"
		roomName = "test-priv"
		meta     = `{"test":true}`
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

	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "1234", meta)

	cli := runClient(t, nc, clientID, clientNumber, false)

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
				"sip.callID":           "<test>", // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
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
		return dtmf == dtmfDigits
	}, 5*time.Second, time.Second/2)

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
		clientID = "test-cli"
		roomName = "test-open"
		meta     = `{"test":true}`
	)
	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "", meta)
	srv.CreateDirectDispatch(t, "test-priv", "1234", "")

	runClient(t, nc, clientID, clientNumber, true)

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
				"sip.callID":           "<test>", // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
			},
		},
	})
}

func TestSIPJoinRoomIndividual(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const (
		clientID = "test-cli"
		roomName = "test-open"
		meta     = `{"test":true}`
	)
	nc := srv.CreateTrunkAndIndividual(t, serverNumber, roomName, "", meta)

	runClient(t, nc, clientID, clientNumber, false)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomPrefWithParticipants(t, ctx, roomName, clientNumber, []lktest.ParticipantInfo{
		{
			Identity: "sip_" + clientNumber,
			Name:     "Phone " + clientNumber,
			Kind:     livekit.ParticipantInfo_SIP,
			Metadata: meta,
			Attributes: map[string]string{
				"sip.callID":           "<test>", // special case
				"sip.callStatus":       "active",
				"sip.trunkPhoneNumber": serverNumber,
				"sip.phoneNumber":      clientNumber,
				"sip.ruleID":           nc.RuleID,
				"sip.trunkID":          nc.TrunkID,
				"lktest.id":            clientID,
			},
		},
	})
}

func TestSIPAudio(t *testing.T) {
	for _, codec := range []string{
		ulaw.SDPName,
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
						roomName = "test-open"
						meta     = `{"test":true}`
					)
					nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "", meta)

					// Connect clients and wait for them to join.
					var (
						clients []*siptest.Client
						audios  []lktest.AudioParticipant
					)
					for i := 0; i < N; i++ {
						codec := codec
						if i == 0 {
							// Make first client always use the same codec.
							// This way we can see how different codecs interact.
							codec = ulaw.SDPName
						}
						cli := runClientWithCodec(t, nc, strconv.Itoa(i+1), fmt.Sprintf("+%d", 111111111*(i+1)), codec, false)
						clients = append(clients, cli)
						audios = append(audios, cli)
					}
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
								"sip.callID":           "<test>", // special case
								"sip.callStatus":       "active",
								"sip.trunkPhoneNumber": serverNumber,
								"sip.phoneNumber":      fmt.Sprintf("+%d", 111111111*(i+1)),
								"sip.ruleID":           nc.RuleID,
								"sip.trunkID":          nc.TrunkID,
								"lktest.id":            strconv.Itoa(i + 1),
							},
						})
					}
					lk.ExpectRoomWithParticipants(t, ctx, roomName, exp)

					ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					lktest.CheckAudioForParticipants(t, ctx, audios...)
					cancel()

					// Stop everything and ensure the room is empty afterward.
					for _, cli := range clients {
						cli.Close()
					}

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
		dtmfPin  = "ww*12w34ww#" // with added delays
		meta     = `{"test":true}`
	)

	// Configure Trunk for inbound server.
	trunkIn := srvIn.CreateTrunkIn(t, serverNumber, userName, userPass)
	ruleIn := srvIn.CreateDirectDispatch(t, roomIn, roomPin, meta)

	// Configure Trunk for outbound server and make a SIP call.
	trunkOut := srvOut.CreateTrunkOut(t, clientNumber, srvIn.Address, userName, userPass)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Run the test twice to make sure participants with the same identities can be re-created.
	for i := 0; i < 2; i++ {
		// Running sub test here is important, because TestSIPOutbound registers Cleanup funcs.
		t.Run(fmt.Sprintf("run %d", i+1), func(t *testing.T) {
			lktest.TestSIPOutbound(t, ctx, lkOut.LiveKit, lkIn.LiveKit, lktest.SIPOutboundTestParams{
				TrunkOut:  trunkOut,
				NumberOut: clientNumber,
				RoomOut:   "outbound",
				TrunkIn:   trunkIn,
				RuleIn:    ruleIn,
				NumberIn:  serverNumber,
				RoomIn:    roomIn,
				RoomPin:   dtmfPin,
				MetaIn:    meta,
			})
		})
	}
}
