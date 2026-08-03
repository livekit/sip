package sip

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

type captureServerTx struct {
	resp *sip.Response
}

func (t *captureServerTx) Respond(res *sip.Response) error {
	t.resp = res
	return nil
}
func (t *captureServerTx) Terminate()             {}
func (t *captureServerTx) Done() <-chan struct{}  { return nil }
func (t *captureServerTx) Err() error             { return nil }
func (t *captureServerTx) Acks() <-chan *sip.Request {
	return nil
}
func (t *captureServerTx) Cancels() <-chan *sip.Request {
	return nil
}

func TestOnOptionsHealth(t *testing.T) {
	// MaxCpuUtilization=1.0 disables the under-load path so the test is stable on busy hosts.
	cfg := &config.Config{MaxCpuUtilization: 1.0, NodeID: "test-options"}
	mon, err := stats.NewMonitor(cfg)
	require.NoError(t, err)
	require.NoError(t, mon.Start(cfg))
	t.Cleanup(mon.Stop)

	s := &Server{mon: mon}
	log := slog.Default()
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{Host: "sip.test", Port: 5060})

	t.Run("healthy returns 200", func(t *testing.T) {
		require.Equal(t, stats.HealthOK, mon.Health())
		tx := &captureServerTx{}
		s.onOptions(log, req, tx)
		require.NotNil(t, tx.resp)
		require.Equal(t, sip.StatusCode(200), tx.resp.StatusCode)
	})

	t.Run("shutdown returns 503", func(t *testing.T) {
		mon.Shutdown()
		require.Equal(t, stats.HealthStopped, mon.Health())
		tx := &captureServerTx{}
		s.onOptions(log, req, tx)
		require.NotNil(t, tx.resp)
		require.Equal(t, sip.StatusCode(503), tx.resp.StatusCode)
	})
}

func TestOnOptionsNotStarted(t *testing.T) {
	mon, err := stats.NewMonitor(&config.Config{MaxCpuUtilization: 0.9, NodeID: "test-ns"})
	require.NoError(t, err)
	// Intentionally do not Start — HealthNotStarted.
	require.Equal(t, stats.HealthNotStarted, mon.Health())

	s := &Server{mon: mon}
	tx := &captureServerTx{}
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{Host: "sip.test", Port: 5060})
	s.onOptions(slog.Default(), req, tx)
	require.NotNil(t, tx.resp)
	require.Equal(t, sip.StatusCode(503), tx.resp.StatusCode)
}
