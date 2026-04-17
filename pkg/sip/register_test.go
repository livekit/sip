package sip

import (
	"context"
	"testing"
	"time"

	"github.com/icholy/digest"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/sip"
)

func TestEnsureRegisteredHandlesDigestChallenge(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected test SIP client to be created")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- client.ensureRegistered(ctx, sipOutboundConfig{
			address:   "registrar.example.com:5060",
			transport: 0,
			user:      "alice",
			pass:      "secret",
		})
	}()

	firstTx := waitTransaction(t, sipClient)
	require.Equal(t, sip.REGISTER, firstTx.req.Method)
	require.Nil(t, firstTx.req.GetHeader("Authorization"))

	challenge := digest.Challenge{
		Realm: "registrar.example.com",
		Nonce: "nonce-1",
	}
	unauthorized := sip.NewResponseFromRequest(firstTx.req, sip.StatusUnauthorized, "Unauthorized", nil)
	unauthorized.AppendHeader(sip.NewHeader("WWW-Authenticate", challenge.String()))
	require.NoError(t, firstTx.transaction.SendResponse(unauthorized))

	secondTx := waitTransaction(t, sipClient)
	require.Equal(t, sip.REGISTER, secondTx.req.Method)
	authHeader := secondTx.req.GetHeader("Authorization")
	require.NotNil(t, authHeader)
	require.Contains(t, authHeader.Value(), `username="alice"`)

	ok := sip.NewResponseFromRequest(secondTx.req, sip.StatusOK, "OK", nil)
	require.NoError(t, secondTx.transaction.SendResponse(ok))
	require.NoError(t, <-done)
}

func TestEnsureRegisteredSkipsUnsupportedProviders(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})

	var sipClient *testSIPClient
	select {
	case sipClient = <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected test SIP client to be created")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- client.ensureRegistered(ctx, sipOutboundConfig{
			address: "carrier.example.com:5060",
			user:    "alice",
			pass:    "secret",
		})
	}()

	tx := waitTransaction(t, sipClient)
	require.Equal(t, sip.REGISTER, tx.req.Method)

	resp := sip.NewResponseFromRequest(tx.req, sip.StatusMethodNotAllowed, "Method Not Allowed", nil)
	require.NoError(t, tx.transaction.SendResponse(resp))
	require.NoError(t, <-done)
}

func waitTransaction(t *testing.T, sipClient *testSIPClient) *transactionRequest {
	t.Helper()
	select {
	case tx := <-sipClient.transactions:
		t.Cleanup(func() { tx.transaction.Terminate() })
		return tx
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected SIP transaction")
		return nil
	}
}
