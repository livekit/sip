package cloudagents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/livekit/protocol/logger"
)

func TestGetPushTargetSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"proxy_host":"agents.livekit.io","name":"livekit","tag":"v1"}`)
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		agentsURL:  server.URL,
	}

	target, err := client.GetPushTarget(context.Background(), "test-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.ProxyHost != "agents.livekit.io" || target.Name != "livekit" || target.Tag != "v1" {
		t.Fatalf("unexpected push target: %+v", target)
	}
}

func TestGetPushTargetNonOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "oops")
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		agentsURL:  server.URL,
	}

	_, err := client.GetPushTarget(context.Background(), "test-agent")
	if err == nil {
		t.Fatal("expected error when push-target returns non-OK status")
	}
	if !strings.Contains(err.Error(), "push-target returned 400") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGetPushTargetDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "invalid-json")
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		agentsURL:  server.URL,
	}

	_, err := client.GetPushTarget(context.Background(), "test-agent")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "failed to decode push-target response") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
