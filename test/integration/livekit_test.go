package integration

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/redis"
	lksdk "github.com/livekit/server-sdk-go"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"
)

func runRedis(t testing.TB) *redis.RedisConfig {
	c, err := Docker.RunWithOptions(&dockertest.RunOptions{
		Name:       "siptest-redis",
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

type Participant struct {
	Identity string
	Kind     livekit.ParticipantInfo_Kind
}

func (lk *LiveKit) ExpectParticipants(t testing.TB, room string, participants []Participant) {
	list := lk.RoomParticipants(t, room)
	require.Len(t, list, len(participants))
	for i := range participants {
		exp, got := participants[i], list[i]
		require.Equal(t, exp.Identity, got.Identity)
		//require.Equal(t, exp.Kind, got.Kind) // FIXME
	}
}

func (lk *LiveKit) ExpectRoomWithParticipants(t testing.TB, room string, participants []Participant) {
	rooms := lk.ListRooms(t)
	require.Len(t, rooms, 1)
	require.Equal(t, room, rooms[0].Name)

	lk.ExpectParticipants(t, room, participants)
}

func (lk *LiveKit) ExpectRoomPrefWithParticipants(t testing.TB, pref, number string, participants []Participant) {
	rooms := lk.ListRooms(t)
	require.Len(t, rooms, 1)
	require.NotEqual(t, pref, rooms[0].Name)
	require.True(t, strings.HasPrefix(rooms[0].Name, pref+"_"+number+"_"))
	t.Log("Room:", rooms[0].Name)

	lk.ExpectParticipants(t, rooms[0].Name, participants)
}

func runLiveKit(t testing.TB) *LiveKit {
	redis := runRedis(t)

	_, port, err := net.SplitHostPort(redis.Address)
	if err != nil {
		t.Fatal(err)
	}

	c, err := Docker.RunWithOptions(&dockertest.RunOptions{
		Name:       "siptest-livekit",
		Repository: "livekit/livekit-server", Tag: "latest",
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
