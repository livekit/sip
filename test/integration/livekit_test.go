package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/redis"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/test/lktest"
)

var debugLKServer = os.Getenv("DEBUG_LK_SERVER") != ""

var redisLast uint32

// normalizeHostPort replaces localhost with 127.0.0.1 to force IPv4,
// which is more reliable in CI environments where IPv6 may not be properly configured.
func normalizeHostPort(addr string) string {
	if strings.HasPrefix(addr, "localhost:") {
		return strings.Replace(addr, "localhost:", "127.0.0.1:", 1)
	}
	if strings.HasPrefix(addr, "[::1]:") {
		// Extract port from [::1]:port format
		// Find the last colon which separates the port
		lastColon := strings.LastIndex(addr, ":")
		if lastColon > 0 {
			port := addr[lastColon+1:]
			return "127.0.0.1:" + port
		}
	}
	return addr
}

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
	addr := normalizeHostPort(c.GetHostPort("6379/tcp"))
	waitTCPPort(t, addr)

	t.Log("Redis running on", addr)
	return &redis.RedisConfig{Address: addr}
}

type LiveKit struct {
	Redis *redis.RedisConfig
	*lktest.LiveKit
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
			"--rm",
		},
		ExposedPorts: []string{"7880/tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = Docker.Purge(c)
	})
	if debugLKServer {
		go Docker.Client.Logs(docker.LogsOptions{
			Context:      lctx,
			Container:    c.Container.ID,
			OutputStream: os.Stderr,
			ErrorStream:  os.Stderr,
			Follow:       true,
			Stdout:       true,
			Stderr:       true,
		})
	}
	wsaddr := normalizeHostPort(c.GetHostPort("7880/tcp"))
	waitTCPPort(t, wsaddr)
	wsurl := "ws://" + wsaddr

	t.Log("LiveKit WS URL:", wsurl)

	lk := &LiveKit{
		LiveKit: lktest.New(wsurl, "devkey", "secret"),
		Redis:   redis,
	}
	lk.Rooms = lksdk.NewRoomServiceClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret)
	lk.SIP = lksdk.NewSIPClient(lk.WsUrl, lk.ApiKey, lk.ApiSecret)

	// Increase timeout for API readiness check as the server may need more time
	// to fully initialize after the port is open
	originalMaxWait := Docker.MaxWait
	Docker.MaxWait = 2 * time.Minute
	defer func() {
		Docker.MaxWait = originalMaxWait
	}()

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
