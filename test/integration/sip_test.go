package integration

import (
	"context"
	"fmt"
	"io"
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

func (s *SIPServer) CreateTrunkOut(t testing.TB, number, addr, user, pass string) string {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPTrunk(ctx, &livekit.CreateSIPTrunkRequest{
		OutboundNumber:   number,
		OutboundAddress:  addr,
		OutboundUsername: user,
		OutboundPassword: pass,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Trunk (out):", tr.SipTrunkId)
	return tr.SipTrunkId
}

func (s *SIPServer) CreateTrunkIn(t testing.TB, number, user, pass string) string {
	ctx := context.Background()
	tr, err := s.Client.CreateSIPTrunk(ctx, &livekit.CreateSIPTrunkRequest{
		OutboundNumber:  number,
		InboundUsername: user,
		InboundPassword: pass,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Trunk (in):", tr.SipTrunkId)
	return tr.SipTrunkId
}

func (s *SIPServer) CreateTrunkAndDirect(t testing.TB, number, room, pin string) *NumberConfig {
	s.CreateTrunkIn(t, number, "", "")
	s.CreateDirectDispatch(t, room, pin)
	return &NumberConfig{SIP: s, Number: number, Pin: pin}
}

func (s *SIPServer) CreateTrunkAndIndividual(t testing.TB, number, room, pin string) *NumberConfig {
	s.CreateTrunkIn(t, number, "", "")
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
	participantsJoinTimeout        = 2 * time.Second
	participantsJoinWithPinTimeout = participantsJoinTimeout + 5*time.Second
	participantsLeaveTimeout       = 3 * time.Second
)

func TestSIPJoinOpenRoom(t *testing.T) {
	lk := runLiveKit(t)
	var (
		dmu  sync.Mutex
		dtmf string
	)
	const roomName = "test-open"
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

	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "")

	cli := runClient(t, nc, "", clientNumber, false)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []ParticipantInfo{
		{Identity: "test"},
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})

	// Wait for WebRTC to come online.
	time.Sleep(2 * time.Second)

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
	const roomName = "test-priv"
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

	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "1234")

	cli := runClient(t, nc, "", clientNumber, false)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	// This needs additional time for the "enter pin" message to end.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinWithPinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []ParticipantInfo{
		{Identity: "test"},
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})

	// Wait for WebRTC to come online.
	time.Sleep(2 * time.Second)

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

	const roomName = "test-open"
	nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "")
	srv.CreateDirectDispatch(t, "test-priv", "1234")

	runClient(t, nc, "", clientNumber, true)

	// This needs additional time for the "enter pin" message to end.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinWithPinTimeout)
	defer cancel()
	lk.ExpectRoomWithParticipants(t, ctx, roomName, []ParticipantInfo{
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})
}

func TestSIPJoinRoomIndividual(t *testing.T) {
	lk := runLiveKit(t)
	srv := runSIPServer(t, lk)

	const roomName = "test-open"
	nc := srv.CreateTrunkAndIndividual(t, serverNumber, roomName, "")

	runClient(t, nc, "", clientNumber, false)

	// Room should be created automatically with exact name.
	// SIP participant should be visible and have a proper kind.
	ctx, cancel := context.WithTimeout(context.Background(), participantsJoinTimeout)
	defer cancel()
	lk.ExpectRoomPrefWithParticipants(t, ctx, roomName, clientNumber, []ParticipantInfo{
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})
}

type AudioParticipant interface {
	SendSignal(ctx context.Context, n int, val int) error
	WaitSignals(ctx context.Context, vals []int, w io.WriteCloser) error
}

func CheckAudioForParticipants(t testing.TB, ctx context.Context, participants ...AudioParticipant) {
	// Participants can only subscribe to tracks that are "live", so give them the chance to do so.
	for _, p := range participants {
		err := p.SendSignal(ctx, 3, 0)
		require.NoError(t, err)
	}

	// Each client will emit its own audio signal.
	var allSig []int
	for i, p := range participants {
		p := p
		sig := i + 1
		allSig = append(allSig, sig)
		go func() {
			err := p.SendSignal(ctx, -1, sig)
			require.NoError(t, err)
		}()
	}

	// And they will wait for the other one's signal.
	errc := make(chan error, len(participants))
	for i, p := range participants {
		p := p
		// Expect all signals except its own.
		var signals []int
		signals = append(signals, allSig[:i]...)
		signals = append(signals, allSig[i+1:]...)
		go func() {
			errc <- p.WaitSignals(ctx, signals, nil)
		}()
	}

	// Wait for the signal sequence to be received by both (or the timeout).
	for i := 0; i < len(participants); i++ {
		err := <-errc
		require.NoError(t, err)
	}
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

					const roomName = "test-open"
					nc := srv.CreateTrunkAndDirect(t, serverNumber, roomName, "")

					// Connect clients and wait for them to join.
					var (
						clients []*siptest.Client
						audios  []AudioParticipant
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
					var exp []ParticipantInfo
					for i := range clients {
						exp = append(exp, ParticipantInfo{Identity: fmt.Sprintf("Phone +%d", 111111111*(i+1)), Kind: livekit.ParticipantInfo_SIP})
					}
					lk.ExpectRoomWithParticipants(t, ctx, roomName, exp)

					ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					CheckAudioForParticipants(t, ctx, audios...)
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
		roomOut  = "outbound"
		roomIn   = "inbound"
		userName = "test-user"
		userPass = "test-pass"
		roomPin  = "*1234"
		dtmfPin  = "ww*12w34ww#" // with added delays
	)

	// LK participants that will generate/listen for audio.
	pOut := lkOut.ConnectParticipant(t, roomOut, "testOut", nil)
	pIn := lkIn.ConnectParticipant(t, roomIn, "testIn", nil)

	// Configure Trunk for inbound server.
	srvIn.CreateTrunkIn(t, serverNumber, userName, userPass)
	srvIn.CreateDirectDispatch(t, roomIn, roomPin)

	// Configure Trunk for outbound server and make a SIP call.
	trunkOut := srvOut.CreateTrunkOut(t, clientNumber, srvIn.Address, userName, userPass)
	lkOut.CreateSIPParticipant(t, trunkOut, roomOut, "Outbound Call", serverNumber, dtmfPin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lkOut.ExpectRoomWithParticipants(t, ctx, roomOut, []ParticipantInfo{
		{Identity: "testOut", Kind: livekit.ParticipantInfo_STANDARD},
		{Identity: "Outbound Call", Kind: livekit.ParticipantInfo_SIP},
	})
	lkIn.ExpectRoomWithParticipants(t, ctx, roomIn, []ParticipantInfo{
		{Identity: "testIn", Kind: livekit.ParticipantInfo_STANDARD},
		{Identity: "Phone " + clientNumber, Kind: livekit.ParticipantInfo_SIP},
	})

	CheckAudioForParticipants(t, ctx, pOut, pIn)
}
