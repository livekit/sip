package integration

import (
	"log"
	"net"
	"os"
	"testing"

	"github.com/ory/dockertest/v3"
)

const dockerBridgeIP = "172.17.0.1"

var Docker *dockertest.Pool

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not construct pool: %s", err)
	}

	// uses pool to try to connect to Docker
	err = pool.Client.Ping()
	if err != nil {
		log.Fatalf("Could not connect to Docker: %s", err)
	}
	Docker = pool

	code := m.Run()
	os.Exit(code)
}

func waitTCPPort(t testing.TB, addr string) {
	if err := Docker.Retry(func() error {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Log(err)
			return err
		}
		_ = conn.Close()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
