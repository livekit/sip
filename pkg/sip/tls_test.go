package sip

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTLSALPNProtocols(t *testing.T) {
	t.Run("nil returns default sip", func(t *testing.T) {
		protos := tlsALPNProtocols(nil)
		require.Equal(t, []string{"sip"}, protos)
	})

	t.Run("empty slice disables ALPN", func(t *testing.T) {
		empty := []string{}
		protos := tlsALPNProtocols(empty)
		require.Empty(t, protos)
	})

	t.Run("custom protocols", func(t *testing.T) {
		custom := []string{"h2", "http/1.1"}
		protos := tlsALPNProtocols(custom)
		require.Equal(t, []string{"h2", "http/1.1"}, protos)
	})

	t.Run("single custom protocol", func(t *testing.T) {
		custom := []string{"sip"}
		protos := tlsALPNProtocols(custom)
		require.Equal(t, []string{"sip"}, protos)
	})
}
