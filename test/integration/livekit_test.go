package integration

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

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

func (lk *LiveKit) ExpectParticipants(t testing.TB, ctx context.Context, room string, participants []Participant) {
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
	slices.SortFunc(participants, func(a, b Participant) int {
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

func (lk *LiveKit) ExpectRoomWithParticipants(t testing.TB, ctx context.Context, room string, participants []Participant) {
	rooms := lk.waitRooms(t, ctx, len(participants) == 0)
	if len(participants) == 0 && len(rooms) == 0 {
		return
	}
	require.Len(t, rooms, 1)
	require.Equal(t, room, rooms[0].Name)

	lk.ExpectParticipants(t, ctx, room, participants)
}

func (lk *LiveKit) ExpectRoomPrefWithParticipants(t testing.TB, ctx context.Context, pref, number string, participants []Participant) {
	rooms := lk.waitRooms(t, ctx, len(participants) == 0)
	require.Len(t, rooms, 1)
	require.NotEqual(t, pref, rooms[0].Name)
	require.True(t, strings.HasPrefix(rooms[0].Name, pref+"_"+number+"_"))
	t.Log("Room:", rooms[0].Name)

	lk.ExpectParticipants(t, ctx, rooms[0].Name, participants)
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
