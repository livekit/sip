package sip

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientCertificateFunc(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		fn := clientCertificateFunc(nil)
		cert, err := fn(&tls.CertificateRequestInfo{})
		require.NoError(t, err)
		require.Nil(t, cert)
	})

	t.Run("presents first cert even when CA filter would exclude it", func(t *testing.T) {
		// Minimal placeholder certificate; GetClientCertificate must return it
		// regardless of CertificateRequestInfo.AcceptableCAs.
		placeholder := tls.Certificate{Certificate: [][]byte{{0x30}}}
		fn := clientCertificateFunc([]tls.Certificate{placeholder})
		cri := &tls.CertificateRequestInfo{
			AcceptableCAs: [][]byte{[]byte("cn=unrelated-ca")},
		}
		cert, err := fn(cri)
		require.NoError(t, err)
		require.NotNil(t, cert)
		require.Equal(t, placeholder.Certificate, cert.Certificate)
	})
}

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
