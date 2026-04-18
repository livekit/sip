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
	sipClient := getCreatedSIPClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.ensureRegistered(ctx, sipOutboundConfig{
			address: "registrar.example.com:5060",
			user:    "alice",
			pass:    "secret",
			featureFlags: map[string]string{
				registrationEnabledFeatureFlag:   "true",
				registrationRegistrarFeatureFlag: "sip:registrar.example.com",
			},
		})
		done <- err
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

func TestEnsureRegisteredUnknownProviderSkipsRegister(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})
	sipClient := getCreatedSIPClient(t)

	profile, err := client.ensureRegistered(context.Background(), sipOutboundConfig{
		address: "carrier.example.com:5060",
		user:    "alice",
		pass:    "secret",
	})
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.Equal(t, "generic", profile.ProfileName)
	require.False(t, profile.Enabled)

	select {
	case tx := <-sipClient.transactions:
		t.Fatalf("unexpected REGISTER transaction: %v", tx.req.Method)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEnsureRegisteredUISUsesNormalizedAuthURI(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})
	sipClient := getCreatedSIPClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.ensureRegistered(ctx, sipOutboundConfig{
			address: "pbx.uiscom.ru:5060",
			user:    "0526470",
			pass:    "secret",
		})
		done <- err
	}()

	firstTx := waitTransaction(t, sipClient)
	require.Equal(t, "pbx.uiscom.ru", firstTx.req.To().Address.Host)
	require.Equal(t, "pbx.uiscom.ru", firstTx.req.From().Address.Host)
	require.Equal(t, "0526470", firstTx.req.To().Address.User)

	challenge := digest.Challenge{
		Realm: "sipve-prod.uis.st",
		Nonce: "nonce-1",
	}
	unauthorized := sip.NewResponseFromRequest(firstTx.req, sip.StatusUnauthorized, "Unauthorized", nil)
	unauthorized.AppendHeader(sip.NewHeader("WWW-Authenticate", challenge.String()))
	require.NoError(t, firstTx.transaction.SendResponse(unauthorized))

	secondTx := waitTransaction(t, sipClient)
	authHeader := secondTx.req.GetHeader("Authorization")
	require.NotNil(t, authHeader)
	require.Contains(t, authHeader.Value(), `uri="sip:pbx.uiscom.ru"`)
	require.NotContains(t, authHeader.Value(), `;transport=udp`)
	require.NotContains(t, authHeader.Value(), `:5060`)

	ok := sip.NewResponseFromRequest(secondTx.req, sip.StatusOK, "OK", nil)
	require.NoError(t, secondTx.transaction.SendResponse(ok))
	require.NoError(t, <-done)
}

func TestEnsureRegisteredCachesSuccessfulRegistration(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})
	sipClient := getCreatedSIPClient(t)

	done := make(chan error, 1)
	go func() {
		_, err := client.ensureRegistered(context.Background(), sipOutboundConfig{
			address: "pbx.uiscom.ru:5060",
			user:    "0526470",
			pass:    "secret",
		})
		done <- err
	}()
	tx := waitTransaction(t, sipClient)
	ok := sip.NewResponseFromRequest(tx.req, sip.StatusOK, "OK", nil)
	require.NoError(t, tx.transaction.SendResponse(ok))
	require.NoError(t, <-done)

	profile, err := client.ensureRegistered(context.Background(), sipOutboundConfig{
		address: "pbx.uiscom.ru:5060",
		user:    "0526470",
		pass:    "secret",
	})
	require.NoError(t, err)
	require.NotNil(t, profile)

	select {
	case tx = <-sipClient.transactions:
		t.Fatalf("unexpected extra REGISTER transaction: %v", tx.req.Method)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEnsureRegisteredUISAlwaysRefreshesBeforeInvite(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})
	sipClient := getCreatedSIPClient(t)

	done := make(chan error, 1)
	go func() {
		_, err := client.ensureRegistered(context.Background(), sipOutboundConfig{
			address: "pbx.uiscom.ru:5060",
			user:    "0526470",
			pass:    "secret",
		})
		done <- err
	}()
	tx := waitTransaction(t, sipClient)
	ok := sip.NewResponseFromRequest(tx.req, sip.StatusOK, "OK", nil)
	require.NoError(t, tx.transaction.SendResponse(ok))
	require.NoError(t, <-done)

	done = make(chan error, 1)
	go func() {
		_, err := client.ensureRegistered(context.Background(), sipOutboundConfig{
			address: "pbx.uiscom.ru:5060",
			user:    "0526470",
			pass:    "secret",
		})
		done <- err
	}()
	tx = waitTransaction(t, sipClient)
	ok = sip.NewResponseFromRequest(tx.req, sip.StatusOK, "OK", nil)
	require.NoError(t, tx.transaction.SendResponse(ok))
	require.NoError(t, <-done)
}

func TestEnsureRegisteredFeatureFlagOverrideWins(t *testing.T) {
	profile, err := resolveRegistrationProfile(sipOutboundConfig{
		address: "pbx.uiscom.ru:5060",
		user:    "alice",
		featureFlags: map[string]string{
			registrationProfileFeatureFlag:     "sipuni",
			registrationRegistrarFeatureFlag:   "sip:custom.sipuni.example",
			registrationAuthURIFeatureFlag:     "sip:auth.sipuni.example",
			registrationAuthUserFeatureFlag:    "auth-user",
			registrationAORUserFeatureFlag:     "aor-user",
			registrationContactUserFeatureFlag: "contact-user",
			registrationFromDomainFeatureFlag:  "from.example",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.Equal(t, "sipuni", profile.ProfileName)
	require.Equal(t, "sip:custom.sipuni.example", profile.RegistrarURI)
	require.Equal(t, "sip:auth.sipuni.example", profile.AuthURI)
	require.Equal(t, "auth-user", profile.AuthUsername)
	require.Equal(t, "aor-user", profile.AORUser)
	require.Equal(t, "contact-user", profile.ContactUser)
	require.Equal(t, "from.example", profile.FromDomain)
}

func TestEnsureRegisteredEnabledProfileDoesNotTreat405AsSkip(t *testing.T) {
	client := NewOutboundTestClient(t, TestClientConfig{})
	sipClient := getCreatedSIPClient(t)

	done := make(chan error, 1)
	go func() {
		_, err := client.ensureRegistered(context.Background(), sipOutboundConfig{
			address: "pbx.uiscom.ru:5060",
			user:    "0526470",
			pass:    "secret",
		})
		done <- err
	}()

	tx := waitTransaction(t, sipClient)
	resp := sip.NewResponseFromRequest(tx.req, sip.StatusMethodNotAllowed, "Method Not Allowed", nil)
	require.NoError(t, tx.transaction.SendResponse(resp))
	require.Error(t, <-done)
}

func getCreatedSIPClient(t *testing.T) *testSIPClient {
	t.Helper()
	select {
	case sipClient := <-createdClients:
		t.Cleanup(func() { _ = sipClient.Close() })
		return sipClient
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected test SIP client to be created")
		return nil
	}
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
