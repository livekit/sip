package cloudagents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
)

func TestStreamLogs_WriterClosesEarly(t *testing.T) {
	_, pw := io.Pipe()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "log line %d\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		projectURL: server.URL,
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		pw.Close()
	}()

	err := client.StreamLogs(context.Background(), "deploy", "test-agent", "", pw, "us-west")

	if err == nil {
		t.Fatal("expected error when writer closes, got nil")
	}
	if !strings.Contains(err.Error(), "failed to write log line") {
		t.Errorf("expected 'failed to write log line' error, got: %v", err)
	}
}

func TestStreamLogs_ContextCanceledDuringWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, "log line %d\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		projectURL: server.URL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := client.StreamLogs(ctx, "deploy", "test-agent", "", &buf, "us-west")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}
}

type failingWriter struct {
	failAfter int
	written   int
}

func (fw *failingWriter) Write(p []byte) (n int, err error) {
	fw.written++
	if fw.written > fw.failAfter {
		return 0, fmt.Errorf("writer closed")
	}
	return len(p), nil
}

func TestStreamLogs_WriterReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "log line %d\n", i)
		}
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		projectURL: server.URL,
	}

	writer := &failingWriter{failAfter: 2}
	err := client.StreamLogs(context.Background(), "deploy", "test-agent", "", writer, "us-west")

	if err == nil {
		t.Fatal("expected error from failing writer, got nil")
	}
	if !strings.Contains(err.Error(), "failed to write log line") {
		t.Errorf("expected 'failed to write log line' error, got: %v", err)
	}
}

func TestStreamLogs_NonOKResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(APIError{
			Message: "bad request",
			Meta:    map[string]string{"foo": "bar"},
		})
	}))
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		projectURL: server.URL,
	}

	err := client.StreamLogs(context.Background(), "deploy", "test-agent", "", &bytes.Buffer{}, "us-west")
	if err == nil {
		t.Fatal("expected error when server responds with non-200 status")
	}
	if !strings.Contains(err.Error(), "failed to get logs") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStreamLogs_ServerClosesConnection(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintln(w, "log line 1")
		flusher.Flush()
		<-done
	}))
	defer server.Close()
	defer close(done)

	go func() {
		time.Sleep(50 * time.Millisecond)
		server.CloseClientConnections()
	}()

	client := &Client{
		httpClient: server.Client(),
		logger:     logger.GetLogger(),
		apiKey:     "test-api-key",
		apiSecret:  "test-api-secret",
		projectURL: server.URL,
	}

	err := client.StreamLogs(context.Background(), "deploy", "test-agent", "", &bytes.Buffer{}, "us-west")
	if err == nil {
		t.Fatal("expected error when server closes connection mid-stream")
	}
	if !strings.Contains(err.Error(), "scanner error") && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("unexpected error when server disconnects: %v", err)
	}
}
