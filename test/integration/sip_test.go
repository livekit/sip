package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/pkg/siptest"
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
	conf := &config.Config{
		ApiKey:        lk.ApiKey,
		ApiSecret:     lk.ApiSecret,
		WsUrl:         lk.WsUrl,
		Redis:         lk.Redis,
		SIPPort:       5060, // TODO: randomize port
		RTPPort:       rtcconfig.PortRange{Start: 20000, End: 20010},
		UseExternalIP: false,
		Logging:       logger.Config{Level: "debug"},
	}

	sipsrv, err := sip.NewService(conf)
	if err != nil {
		t.Fatal(err)
	}

	svc := service.NewService(conf, sipsrv.InternalServerImpl(), sipsrv.Stop, sipsrv.ActiveCalls, psrpcCli, bus)
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
	Number   string
	Pin      string
	AuthUser string
	AuthPass string
}

func (s *SIPServer) CreateTrunkAndDirect(t testing.TB, number, room, pin string) *NumberConfig {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPTrunk(ctx, &livekit.CreateSIPTrunkRequest{
		OutboundNumber: number,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Trunk:", tr.SipTrunkId)
	s.CreateDirectDispatch(t, room, pin)
	return &NumberConfig{SIP: s, Number: number, Pin: pin}
}

func (s *SIPServer) CreateTrunkAndIndividual(t testing.TB, number, room, pin string) *NumberConfig {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPTrunk(ctx, &livekit.CreateSIPTrunkRequest{
		OutboundNumber: number,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Trunk:", tr.SipTrunkId)
	s.CreateIndividualDispatch(t, room, pin)
	return &NumberConfig{SIP: s, Number: number, Pin: pin}
}

func (s *SIPServer) CreateDirectDispatch(t testing.TB, room, pin string) {
	ctx := context.Background()
	dr, err := s.Client.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
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
}

func (s *SIPServer) CreateIndividualDispatch(t testing.TB, pref, pin string) {
	ctx := context.Background()
	dr, err := s.Client.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
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
}

func runClient(t testing.TB, conf *NumberConfig, number string, forcePin bool) func() {
	cli, err := siptest.NewClient(siptest.ClientConfig{
		//IP: dockerBridgeIP,
		Number:   number,
		AuthUser: conf.AuthUser,
		AuthPass: conf.AuthPass,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
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
	return cli.Close
}

const (
	serverNumber = "+111111111"
	clientNumber = "+222222222"
)

func TestSIPJoinOpenRoom(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const roomName = "test-open"
	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "")

	hangup := runClient(t, nc, clientNumber, false)
	time.Sleep(2 * time.Second)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	lk.ExpectRoomWithParticipants(t, roomName, []Participant{
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})

	hangup()
	time.Sleep(1 * time.Second)

	// SIP participant must disconnect from LK room on hangup.
	lk.ExpectRoomWithParticipants(t, roomName, nil)
}

func TestSIPJoinPinRoom(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const roomName = "test-priv"
	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "1234")

	hangup := runClient(t, nc, clientNumber, false)

	// Need additional time for the "enter pin" message to end.
	time.Sleep(7 * time.Second)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	lk.ExpectRoomWithParticipants(t, roomName, []Participant{
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})

	hangup()
	time.Sleep(1 * time.Second)

	// SIP participant must disconnect from LK room on hangup.
	lk.ExpectRoomWithParticipants(t, roomName, nil)
}

func TestSIPJoinOpenRoomWithPin(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const roomName = "test-open"
	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "")
	srv.CreateDirectDispatch(t, "test-priv", "1234")

	runClient(t, nc, clientNumber, true)

	// Need additional time for the "enter pin" message to end.
	time.Sleep(7 * time.Second)

	lk.ExpectRoomWithParticipants(t, roomName, []Participant{
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})
}

func TestSIPJoinRoomIndividual(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const roomName = "test-open"
	nc := srv.CreateTrunkAndIndividual(t, serverNumber, roomName, "")

	runClient(t, nc, clientNumber, false)
	time.Sleep(2 * time.Second)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	lk.ExpectRoomPrefWithParticipants(t, roomName, clientNumber, []Participant{
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})
}
