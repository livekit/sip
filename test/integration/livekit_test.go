package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/redis"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/test/lktest"
)

var debugLKServer = os.Getenv("DEBUG_LK_SERVER") != ""

var redisLast uint32

func createTestNetwork(t testing.TB, name string) *dockertest.Network {
	t.Helper()
	existing, err := Docker.NetworksByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(existing) > 0 {
		t.Fatal("network already exists:", name)
	}
	network, err := Docker.CreateNetwork(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if info, err := Docker.Client.NetworkInfo(network.Network.ID); err == nil {
			network.Network = info
		}
		if err := Docker.RemoveNetwork(network); err != nil {
			t.Log("remove network", name, err)
		}
	})
	return network
}

func runRedis(t testing.TB, network *dockertest.Network) (*redis.RedisConfig, string) {
	name := fmt.Sprintf("siptest-redis-%d", atomic.AddUint32(&redisLast, 1))
	if _, ok := Docker.ContainerByName(name); ok {
		t.Fatal("Redis container already exists:", name)
	}
	c, err := Docker.RunWithOptions(
		&dockertest.RunOptions{
			Name:       name,
			Repository: "redis", Tag: "latest",
			Networks: []*dockertest.Network{network},
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Docker.Purge(c); err != nil {
			t.Log("purge", name, err)
		}
	})
	addr := c.GetHostPort("6379/tcp")
	waitTCPPort(t, addr)

	t.Log("Redis running on", addr)
	// addr: host-published (SIP service); name:6379: in-network (LiveKit container).
	return &redis.RedisConfig{Address: addr}, name
}

type LiveKit struct {
	Redis *redis.RedisConfig
	*lktest.LiveKit
}

var livekitLast uint32

func runLiveKit(t testing.TB) *LiveKit {
	id := atomic.AddUint32(&livekitLast, 1)

	// Shared network so LiveKit reaches Redis by name, avoiding a
	// container->host round-trip that some CI runners block.
	network := createTestNetwork(t, fmt.Sprintf("siptest-net-%d", id))

	redis, redisName := runRedis(t, network)

	name := fmt.Sprintf("siptest-livekit-%d", id)
	if _, ok := Docker.ContainerByName(name); ok {
		t.Fatal("Livekit-server container already exists:", name)
	}
	c, err := Docker.RunWithOptions(
		&dockertest.RunOptions{
			Name:       name,
			Repository: "livekit/livekit-server", Tag: "master",
			Cmd: []string{
				"--dev",
				"--redis-host", redisName + ":6379",
				"--bind", "0.0.0.0",
			},
			ExposedPorts: []string{"7880/tcp"},
			Networks:     []*dockertest.Network{network},
		})
	if err != nil {
		t.Fatal(err)
	}
	lctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if t.Failed() && debugLKServer {
			dumpLivekitServerLogs(t, c.Container.ID)
		}
		if err := Docker.Purge(c); err != nil {
			t.Log("purge", name, err)
		}
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
	wsaddr := c.GetHostPort("7880/tcp")
	if wsaddr == "" {
		t.Fatal("LiveKit WS address is empty")
	}
	waitTCPPort(t, wsaddr)
	wsurl := "ws://" + wsaddr

	t.Log("LiveKit WS URL:", wsurl)

	lk := &LiveKit{
		LiveKit: lktest.New(wsurl, "devkey", "secret"),
		Redis:   redis,
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

func dumpLivekitServerLogs(t testing.TB, containerID string) {
	t.Helper()
	var logBuffer bytes.Buffer
	if err := Docker.Client.Logs(docker.LogsOptions{
		Container:    containerID,
		OutputStream: &logBuffer,
		RawTerminal:  true,
	}); err != nil {
		t.Log("LiveKit logs:", err)
		return
	}
	livekitServerLogs(t, logBuffer.String(), 40)
}

func livekitServerLogs(t testing.TB, logs string, maxLines int) {
	type lineRecord struct {
		number int
		text   string
	}
	lines := strings.Split(logs, "\n")
	fatalLines := []*lineRecord{}
	errorLines := []*lineRecord{}
	tailLines := lines
	truncated := false
	if len(lines) > maxLines {
		tailLines = lines[len(lines)-maxLines:]
		truncated = true
	}
	for i, line := range lines {
		if strings.Contains(line, "fatal") || strings.Contains(line, "panic") {
			l := &lineRecord{number: i, text: line}
			fatalLines = append(fatalLines, l)
		} else if strings.Contains(line, "error") {
			l := &lineRecord{number: i, text: line}
			errorLines = append(errorLines, l)
		}
	}
	t.Logf("Found %d fatal lines, %d error lines", len(fatalLines), len(errorLines))
	for _, l := range fatalLines {
		t.Logf("Fatal line %d: %s", l.number, l.text)
	}
	for _, l := range errorLines {
		t.Logf("Error line %d: %s", l.number, l.text)
	}
	if len(lines) > 0 {
		t.Logf("Tail lines:")
		if truncated {
			t.Logf("... truncated ...")
		}
		for _, l := range tailLines {
			t.Log(l)
		}
	}
}
