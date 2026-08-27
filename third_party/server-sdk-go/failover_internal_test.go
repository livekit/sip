// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lksdk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestFailoverAttempts(t *testing.T) {
	cases := []struct {
		cfg  failoverConfig
		host string
		want int
	}{
		// Enabled (the default): only *.livekit.cloud project domains fail over.
		{failoverConfig{enabled: true}, "myproject.livekit.cloud", failoverMaxAttempts},
		{failoverConfig{enabled: true}, "myproject.region.livekit.cloud", failoverMaxAttempts},
		{failoverConfig{enabled: true}, "myproject.livekit.io", 1},
		{failoverConfig{enabled: true}, "example.com", 1},
		{failoverConfig{enabled: true}, "127.0.0.1", 1},
		{failoverConfig{enabled: true}, "notlivekit.cloud", 1},
		// force bypasses the cloud-host check; disabled never fails over.
		{failoverConfig{enabled: true, force: true}, "127.0.0.1", failoverMaxAttempts},
		{failoverConfig{enabled: false, force: true}, "myproject.livekit.cloud", 1},
		{failoverConfig{enabled: false}, "myproject.livekit.cloud", 1},
	}
	for _, c := range cases {
		if got := c.cfg.attempts(c.host); got != c.want {
			t.Errorf("attempts(cfg=%+v, host=%q) = %v, want %v", c.cfg, c.host, got, c.want)
		}
	}
}

func TestPinRingingTimeout(t *testing.T) {
	// Unset falls back to the default ring window, so the request carries an
	// explicit value rather than relying on the server default.
	if got := pinRingingTimeout(nil); got == nil || got.AsDuration() != defaultRingingTimeout {
		t.Errorf("pinRingingTimeout(nil) = %v, want %v", got, defaultRingingTimeout)
	}
	// A caller-supplied value is preserved as-is.
	set := durationpb.New(15 * time.Second)
	if got := pinRingingTimeout(set); got != set {
		t.Errorf("pinRingingTimeout(set) = %v, want the caller value", got)
	}
}

func TestDialContext(t *testing.T) {
	ring := func(d time.Duration) *durationpb.Duration { return durationpb.New(d) }

	assertBudget := func(t *testing.T, ctx context.Context, want time.Duration) {
		t.Helper()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected a deadline")
		}
		// time.Until is at most want (a little elapsed); allow slack for scheduling.
		if got := time.Until(dl); got > want || got < want-time.Second {
			t.Errorf("budget = %v, want ~%v", got, want)
		}
	}

	// No caller deadline and no ringing_timeout: fall back to the default ring
	// window plus the margin (so the request outlasts the default ring too).
	t.Run("no deadline uses default ringing plus margin", func(t *testing.T) {
		ctx, cancel := dialContext(context.Background(), nil)
		defer cancel()
		assertBudget(t, ctx, defaultRingingTimeout+ringingTimeoutMargin)
	})
	t.Run("no deadline raised above ringing", func(t *testing.T) {
		ctx, cancel := dialContext(context.Background(), ring(40*time.Second))
		defer cancel()
		assertBudget(t, ctx, 40*time.Second+ringingTimeoutMargin)
	})

	// A caller deadline shorter than the dial budget is extended: a
	// too-short deadline must not abort the request before the call is answered.
	t.Run("short caller deadline is extended", func(t *testing.T) {
		parent, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer pcancel()
		ctx, cancel := dialContext(parent, ring(40*time.Second))
		defer cancel()
		assertBudget(t, ctx, 40*time.Second+ringingTimeoutMargin)
	})

	// A caller deadline longer than the dial budget is honored as-is.
	t.Run("long caller deadline is honored", func(t *testing.T) {
		parent, pcancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer pcancel()
		ctx, cancel := dialContext(parent, ring(40*time.Second))
		defer cancel()
		assertBudget(t, ctx, 90*time.Second)
	})

	// Even when the deadline is extended, explicit caller cancellation propagates.
	t.Run("extended context still forwards cancellation", func(t *testing.T) {
		parent, pcancel := context.WithCancel(context.Background())
		ctx, cancel := dialContext(parent, ring(40*time.Second))
		defer cancel()
		pcancel()
		select {
		case <-ctx.Done():
			if !errors.Is(context.Cause(ctx), context.Canceled) {
				t.Errorf("cause = %v, want context.Canceled", context.Cause(ctx))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("caller cancellation was not forwarded to the extended context")
		}
	})
}

type stubAttempt struct {
	at       time.Time
	deadline time.Time
	hasDL    bool
}

// stubRoundTripper records the context deadline of each attempt and delegates
// the response/error to behave.
type stubRoundTripper struct {
	mu       sync.Mutex
	attempts []stubAttempt
	behave   func(attempt int, ctx context.Context) (*http.Response, error)
}

func (s *stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	i := len(s.attempts)
	dl, ok := r.Context().Deadline()
	s.attempts = append(s.attempts, stubAttempt{at: time.Now(), deadline: dl, hasDL: ok})
	s.mu.Unlock()
	return s.behave(i, r.Context())
}

func (s *stubRoundTripper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attempts)
}

func stubResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}
}

// newStubTransport wires a failoverTransport to a stub base and a region cache
// pre-populated with two fallback regions, so failover has somewhere to go.
func newStubTransport(stub http.RoundTripper, host string) *failoverTransport {
	rc := newRegionCache()
	rc.cache[strings.ToLower(host)] = &regionCacheEntry{
		settings: &livekit.RegionSettings{Regions: []*livekit.RegionInfo{
			{Url: "http://" + host},
			{Url: "http://r1.example.com"},
			{Url: "http://r2.example.com"},
		}},
		fetchedAt: time.Now(),
		ttl:       time.Hour,
	}
	return &failoverTransport{base: stub, regions: rc}
}

func stubRequest(ctx context.Context, host string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+host+"/twirp/livekit.RoomService/CreateRoom", strings.NewReader("{}"))
	return req
}

// headerRecordingRoundTripper records the request-id header seen on each attempt.
type headerRecordingRoundTripper struct {
	mu     sync.Mutex
	ids    []string
	behave func(attempt int) (*http.Response, error)
}

func (h *headerRecordingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	i := len(h.ids)
	h.ids = append(h.ids, r.Header.Get(requestIDHeader))
	h.mu.Unlock()
	return h.behave(i)
}

// prepareContext must stamp a request id so the server can dedup retries.
func TestPrepareContextSetsRequestID(t *testing.T) {
	ab := authBase{token: "tok"}
	ctx, err := ab.prepareContext(context.Background(), withVideoGrant{RoomCreate: true})
	if err != nil {
		t.Fatalf("prepareContext: %v", err)
	}
	h, ok := twirp.HTTPRequestHeaders(ctx)
	if !ok {
		t.Fatal("no twirp headers on context")
	}
	if h.Get(requestIDHeader) == "" {
		t.Fatal("request id header not set")
	}
}

// A caller-supplied request id must be preserved (their higher-level retries
// should dedup too).
func TestPrepareContextRespectsCallerRequestID(t *testing.T) {
	ab := authBase{token: "tok"}
	inCtx, err := twirp.WithHTTPRequestHeaders(context.Background(),
		http.Header{requestIDHeader: []string{"caller-123"}})
	if err != nil {
		t.Fatalf("WithHTTPRequestHeaders: %v", err)
	}
	ctx, err := ab.prepareContext(inCtx, withVideoGrant{RoomCreate: true})
	if err != nil {
		t.Fatalf("prepareContext: %v", err)
	}
	h, _ := twirp.HTTPRequestHeaders(ctx)
	if got := h.Get(requestIDHeader); got != "caller-123" {
		t.Fatalf("caller request id overwritten: got %q", got)
	}
}

// The id is generated once per logical call, so every failover attempt must
// carry the same value — never regenerated per attempt in the transport.
func TestFailoverPreservesRequestIDAcrossAttempts(t *testing.T) {
	const host = "primary.example.com"
	setMinFailoverTimeout(t, time.Millisecond)

	rt := &headerRecordingRoundTripper{behave: func(attempt int) (*http.Response, error) {
		if attempt < 2 {
			return stubResponse(http.StatusServiceUnavailable), nil // 5xx -> fail over
		}
		return stubResponse(http.StatusOK), nil
	}}
	tr := newStubTransport(rt, host)

	ctx := withFailoverForce(context.Background(), time.Millisecond)
	req := stubRequest(ctx, host)
	req.Header.Set(requestIDHeader, "req_fixed")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if len(rt.ids) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(rt.ids))
	}
	for i, id := range rt.ids {
		if id != "req_fixed" {
			t.Errorf("attempt %d: request id = %q, want req_fixed", i, id)
		}
	}
}

// setMinFailoverTimeout lowers the thundering-herd threshold so tests can use
// short timeouts and still exercise failover.
func setMinFailoverTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := minFailoverTimeout
	minFailoverTimeout = d
	t.Cleanup(func() { minFailoverTimeout = orig })
}

// Each retry must get the caller's full timeout budget, not the shrinking
// remainder of a single deadline.
func TestFailoverResetsTimeoutPerAttempt(t *testing.T) {
	const host = "primary.example.com"
	const budget = 200 * time.Millisecond
	setMinFailoverTimeout(t, 10*time.Millisecond) // allow failover at this short budget

	stub := &stubRoundTripper{behave: func(attempt int, _ context.Context) (*http.Response, error) {
		if attempt < 2 {
			return stubResponse(http.StatusServiceUnavailable), nil // 5xx -> fail over
		}
		return stubResponse(http.StatusOK), nil
	}}
	tr := newStubTransport(stub, host)

	// force enables failover for the non-cloud host; 30ms backoff makes the
	// shrinking-budget bug obvious if the timeout weren't reset.
	ctx := withFailoverForce(context.Background(), 30*time.Millisecond)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resp, err := tr.RoundTrip(stubRequest(ctx, host))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if stub.count() != 3 {
		t.Fatalf("expected 3 attempts, got %d", stub.count())
	}
	for i, a := range stub.attempts {
		if !a.hasDL {
			t.Fatalf("attempt %d had no deadline", i)
		}
		if remaining := a.deadline.Sub(a.at); remaining < budget*3/4 {
			t.Errorf("attempt %d started with only %v of the %v budget; timeout was not reset", i, remaining, budget)
		}
	}
}

// An unresponsive region (per-attempt timeout) is retried against the next
// region, with a fresh budget.
func TestFailoverRetriesOnTimeout(t *testing.T) {
	const host = "primary.example.com"
	setMinFailoverTimeout(t, 10*time.Millisecond) // allow failover at this short budget

	stub := &stubRoundTripper{behave: func(attempt int, ctx context.Context) (*http.Response, error) {
		if attempt == 0 {
			// Region 0 is unresponsive: block until the attempt's deadline fires.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return stubResponse(http.StatusOK), nil
	}}
	tr := newStubTransport(stub, host)

	ctx := withFailoverForce(context.Background(), time.Millisecond)
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	resp, err := tr.RoundTrip(stubRequest(ctx, host))
	if err != nil {
		t.Fatalf("a timed-out region should fail over, got error: %v", err)
	}
	_ = resp.Body.Close()
	if n := stub.count(); n != 2 {
		t.Fatalf("expected 2 attempts (timeout then success), got %d", n)
	}
	// The retry must get its own fresh budget, not the remainder after the first
	// attempt already consumed the whole 50ms.
	if a := stub.attempts[1]; a.hasDL && a.deadline.Sub(a.at) < 40*time.Millisecond {
		t.Errorf("retry after timeout got only %v budget; not reset", a.deadline.Sub(a.at))
	}
}

// A short timeout (< minFailoverTimeout) must not fail over, to avoid
// thundering-herd retries across regions.
func TestFailoverShortTimeoutDoesNotRetry(t *testing.T) {
	const host = "primary.example.com"
	setMinFailoverTimeout(t, 5*time.Second)

	stub := &stubRoundTripper{behave: func(_ int, _ context.Context) (*http.Response, error) {
		return stubResponse(http.StatusServiceUnavailable), nil // 5xx would normally fail over
	}}
	tr := newStubTransport(stub, host)

	ctx := withFailoverForce(context.Background(), time.Millisecond) // failover otherwise forced on
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)    // but budget < 5s
	defer cancel()

	resp, err := tr.RoundTrip(stubRequest(ctx, host))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if n := stub.count(); n != 1 {
		t.Fatalf("a short-timeout request must not fail over; made %d attempts", n)
	}
}

// Explicit caller cancellation is terminal — we stop rather than failing over.
func TestFailoverDoesNotRetryOnCancel(t *testing.T) {
	const host = "primary.example.com"

	ctx, cancel := context.WithCancel(withFailoverForce(context.Background(), time.Millisecond))
	defer cancel()

	stub := &stubRoundTripper{behave: func(_ int, attemptCtx context.Context) (*http.Response, error) {
		cancel() // caller gives up mid-flight
		<-attemptCtx.Done()
		return nil, attemptCtx.Err()
	}}
	tr := newStubTransport(stub, host)

	_, err := tr.RoundTrip(stubRequest(ctx, host))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Canceled, got %v", err)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("a cancelled request must not be retried; made %d attempts", n)
	}
}

// A 5xx within the budget is still retried (sanity check that the timeout
// changes didn't disable failover).
func TestFailoverRetriesOn5xxWithinBudget(t *testing.T) {
	const host = "primary.example.com"

	stub := &stubRoundTripper{behave: func(attempt int, _ context.Context) (*http.Response, error) {
		if attempt == 0 {
			return stubResponse(http.StatusBadGateway), nil
		}
		return stubResponse(http.StatusOK), nil
	}}
	tr := newStubTransport(stub, host)

	ctx := withFailoverForce(context.Background(), time.Millisecond)
	resp, err := tr.RoundTrip(stubRequest(ctx, host))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d", resp.StatusCode)
	}
	if n := stub.count(); n != 2 {
		t.Fatalf("expected 2 attempts (5xx then success), got %d", n)
	}
}
