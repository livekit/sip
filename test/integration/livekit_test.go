package integration

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/redis"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/ory/dockertest/v3"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/audiotest"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/media/rtp"
	webmm "github.com/livekit/sip/pkg/media/webm"
	"github.com/livekit/sip/pkg/mixer"
)

const (
	channels = 1
)

var redisLast uint32

func runRedis(t testing.TB) *redis.RedisConfig {
	c, err := Docker.RunWithOptions(&dockertest.RunOptions{
		Name:       fmt.Sprintf("siptest-redis-%d", atomic.AddUint32(&redisLast, 1)),
		Repository: "redis", Tag: "latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = Docker.Purge(c)
	})
	addr := c.GetHostPort("6379/tcp")
	waitTCPPort(t, addr)

	t.Log("Redis running on", addr)
	return &redis.RedisConfig{Address: addr}
}

type LiveKit struct {
	Redis     *redis.RedisConfig
	Rooms     *lksdk.RoomServiceClient
	SIP       *lksdk.SIPClient
	ApiKey    string
	ApiSecret string
	WsUrl     string
}

func (lk *LiveKit) ListRooms(t testing.TB) []*livekit.Room {
	resp, err := lk.Rooms.ListRooms(context.Background(), &livekit.ListRoomsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Rooms
}

func (lk *LiveKit) RoomParticipants(t testing.TB, room string) []*livekit.ParticipantInfo {
	resp, err := lk.Rooms.ListParticipants(context.Background(), &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Participants
}

func (lk *LiveKit) CreateSIPParticipant(t testing.TB, trunk, room, identity, number, dtmf string) {
	_, err := lk.SIP.CreateSIPParticipant(context.Background(), &livekit.CreateSIPParticipantRequest{
		SipTrunkId:          trunk,
		SipCallTo:           number,
		RoomName:            room,
		ParticipantIdentity: identity,
		Dtmf:                dtmf,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (lk *LiveKit) Connect(t testing.TB, room, identity string, cb *lksdk.RoomCallback) *lksdk.Room {
	r := lksdk.NewRoom(cb)
	err := r.Join(lk.WsUrl, lksdk.ConnectInfo{
		APIKey:              lk.ApiKey,
		APISecret:           lk.ApiSecret,
		RoomName:            room,
		ParticipantIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Disconnect)
	return r
}

func (lk *LiveKit) ConnectParticipant(t testing.TB, room, identity string, cb *lksdk.RoomCallback) *Participant {
	if cb == nil {
		cb = new(lksdk.RoomCallback)
	}
	p := &Participant{t: t}
	pr, pw := media.Pipe[media.PCM16Sample]()
	t.Cleanup(func() {
		pw.Close()
		pr.Close()
	})
	p.AudioIn = pr
	p.mix = mixer.NewMixer(pw, rtp.DefSampleDur, rtp.DefSampleRate)
	cb.ParticipantCallback.OnTrackPublished = func(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if pub.Kind() == lksdk.TrackKindAudio {
			if err := pub.SetSubscribed(true); err != nil {
				t.Error("cannot subscribe to the track", pub.SID(), err)
			}
		}
	}
	cb.ParticipantCallback.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		inp := p.mix.NewInput()
		defer p.mix.RemoveInput(inp)

		odec, err := opus.Decode(inp, rtp.DefSampleRate, channels)
		if err != nil {
			return
		}
		h := rtp.NewMediaStreamIn[opus.Sample](odec)
		_ = rtp.HandleLoop(track, h)
	}
	p.Room = lk.Connect(t, room, identity, cb)
	track, err := p.newAudioTrack()
	if err != nil {
		t.Fatal(err)
	}
	p.AudioOut = track
	return p
}

type Participant struct {
	t   testing.TB
	mix *mixer.Mixer

	Room     *lksdk.Room
	AudioOut media.Writer[media.PCM16Sample]
	AudioIn  media.Reader[media.PCM16Sample]
}

func (p *Participant) newAudioTrack() (media.Writer[media.PCM16Sample], error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, err
	}
	pt := p.Room.LocalParticipant
	if _, err = pt.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: pt.Identity(),
	}); err != nil {
		return nil, err
	}
	ow := media.FromSampleWriter[opus.Sample](track, rtp.DefSampleDur)
	pw, err := opus.Encode(ow, rtp.DefSampleRate, channels)
	if err != nil {
		return nil, err
	}
	return pw, nil
}

const (
	signalAmp    = math.MaxInt16 / 4
	signalAmpMin = signalAmp - signalAmp/4 // TODO: why it's so low?
	signalAmpMax = signalAmp + signalAmp/10
)

func (p *Participant) SendSignal(ctx context.Context, n int, val int) error {
	signal := make(media.PCM16Sample, rtp.DefPacketDur)
	audiotest.GenSignal(signal, []audiotest.Wave{{Ind: val, Amp: signalAmp}})
	p.t.Log("sending signal", "len", len(signal), "n", n, "sig", val)

	ticker := time.NewTicker(rtp.DefSampleDur)
	defer ticker.Stop()
	i := 0
	for {
		if n > 0 && i >= n {
			break
		}
		select {
		case <-ctx.Done():
			if n <= 0 {
				p.t.Log("stopping signal", "n", i, "sig", val)
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}

		if err := p.AudioOut.WriteSample(signal); err != nil {
			return err
		}
		i++
	}
	return nil
}

func (c *Participant) WaitSignals(ctx context.Context, vals []int, w io.WriteCloser) error {
	var ws media.PCM16WriteCloser
	if w != nil {
		ws = webmm.NewPCM16Writer(w, rtp.DefSampleRate, rtp.DefSampleDur)
		defer ws.Close()
	}
	lastLog := time.Now()
	buf := make(media.PCM16Sample, rtp.DefPacketDur)
	for {
		n, err := c.AudioIn.ReadSample(buf)
		if err != nil {
			c.t.Log("cannot read rtp packet", "err", err)
			return err
		}
		decoded := buf[:n]
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if ws != nil {
			if err = ws.WriteSample(decoded); err != nil {
				return err
			}
		}
		if !slices.ContainsFunc(decoded, func(v int16) bool { return v != 0 }) {
			continue // Ignore silence.
		}
		out := audiotest.FindSignal(decoded)
		if len(out) >= len(vals) {
			// Only consider first N strongest signals.
			out = out[:len(vals)]
			// Sort them again by index, so it's easier to compare.
			slices.SortFunc(out, func(a, b audiotest.Wave) int {
				return a.Ind - b.Ind
			})
			ok := true
			for i := range vals {
				// All signals must match the frequency and have around the same amplitude.
				if out[i].Ind != vals[i] || out[i].Amp < signalAmpMin || out[i].Amp > signalAmpMax {
					ok = false
					break
				}
			}
			if ok {
				c.t.Log("signal found", "sig", vals)
				return nil
			}
		}
		// Remove most other components from the logs.
		if len(out) > len(vals)*2 {
			out = out[:len(vals)*2]
		}
		if time.Since(lastLog) > time.Second {
			lastLog = time.Now()
			c.t.Log("skipping signal", "len", len(decoded), "signals", out)
		}
	}
}

type ParticipantInfo struct {
	Identity string
	Kind     livekit.ParticipantInfo_Kind
}

func (lk *LiveKit) ExpectParticipants(t testing.TB, ctx context.Context, room string, participants []ParticipantInfo) {
	var list []*livekit.ParticipantInfo
	ticker := time.NewTicker(time.Second / 4)
	defer ticker.Stop()
wait:
	for {
		list = lk.RoomParticipants(t, room)
		if len(list) == len(participants) {
			break
		}
		select {
		case <-ctx.Done():
			break wait
		case <-ticker.C:
		}
	}
	require.Len(t, list, len(participants))
	slices.SortFunc(participants, func(a, b ParticipantInfo) int {
		return strings.Compare(a.Identity, b.Identity)
	})
	slices.SortFunc(list, func(a, b *livekit.ParticipantInfo) int {
		return strings.Compare(a.Identity, b.Identity)
	})
	for i := range participants {
		exp, got := participants[i], list[i]
		require.Equal(t, exp.Identity, got.Identity)
		//require.Equal(t, exp.Kind, got.Kind) // FIXME
	}
}

func (lk *LiveKit) waitRooms(t testing.TB, ctx context.Context, none bool) []*livekit.Room {
	var rooms []*livekit.Room
	ticker := time.NewTicker(time.Second / 4)
	defer ticker.Stop()
	for {
		rooms = lk.ListRooms(t)
		if !none {
			if len(rooms) >= 1 {
				return rooms
			}
		} else {
			if len(rooms) == 0 {
				return rooms
			}
		}
		select {
		case <-ctx.Done():
			return rooms
		case <-ticker.C:
		}
	}
}

func (lk *LiveKit) ExpectRoomWithParticipants(t testing.TB, ctx context.Context, room string, participants []ParticipantInfo) {
	rooms := lk.waitRooms(t, ctx, len(participants) == 0)
	if len(participants) == 0 && len(rooms) == 0 {
		return
	}
	require.Len(t, rooms, 1)
	require.Equal(t, room, rooms[0].Name)

	lk.ExpectParticipants(t, ctx, room, participants)
}

func (lk *LiveKit) ExpectRoomPrefWithParticipants(t testing.TB, ctx context.Context, pref, number string, participants []ParticipantInfo) {
	rooms := lk.waitRooms(t, ctx, len(participants) == 0)
	require.Len(t, rooms, 1)
	require.NotEqual(t, pref, rooms[0].Name)
	require.True(t, strings.HasPrefix(rooms[0].Name, pref+"_"+number+"_"))
	t.Log("Room:", rooms[0].Name)

	lk.ExpectParticipants(t, ctx, rooms[0].Name, participants)
}

var livekitLast uint32

func runLiveKit(t testing.TB) *LiveKit {
	redis := runRedis(t)

	_, port, err := net.SplitHostPort(redis.Address)
	if err != nil {
		t.Fatal(err)
	}

	c, err := Docker.RunWithOptions(&dockertest.RunOptions{
		Name:       fmt.Sprintf("siptest-livekit-%d", atomic.AddUint32(&livekitLast, 1)),
		Repository: "livekit/livekit-server", Tag: "master",
		Cmd: []string{
			"--dev",
			// TODO: We use Docker bridge IP here instead of the host IP.
			//       Maybe run on the host network instead? We might need it for RTP anyway.
			"--redis-host", dockerBridgeIP + ":" + port,
			"--bind", "0.0.0.0",
		},
		ExposedPorts: []string{"7880/tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = Docker.Purge(c)
	})
	wsaddr := c.GetHostPort("7880/tcp")
	waitTCPPort(t, wsaddr)
	wsurl := "ws://" + wsaddr

	t.Log("LiveKit WS URL:", wsurl)

	lk := &LiveKit{
		Redis:     redis,
		ApiKey:    "devkey",
		ApiSecret: "secret",
		WsUrl:     wsurl,
	}
	lk.Rooms = lksdk.NewRoomServiceClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret)
	lk.SIP = lksdk.NewSIPClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret)

	err = Docker.Retry(func() error {
		ctx := context.Background()
		_, err := lk.Rooms.ListRooms(ctx, &livekit.ListRoomsRequest{})
		if err != nil {
			t.Log(err)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return lk
}
