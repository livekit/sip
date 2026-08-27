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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/server-sdk-go/v2/signalling"
)

// Total attempts (the original request plus fallback regions) and the base
// retry backoff are fixed, not user-configurable, so retries can't be tuned to
// values that could overwhelm the server.
const (
	failoverMaxAttempts = 3
	failoverBackoffBase = 200 * time.Millisecond
)

// minFailoverTimeout gates failover on the request timeout: a shorter budget
// gets a single attempt, since a retry is unlikely to help and risks
// thundering-herd retries across regions.
var minFailoverTimeout = 5 * time.Second

const (
	// defaultRequestTimeout bounds a request when the caller sets no deadline.
	defaultRequestTimeout = 10 * time.Second
	// defaultRingingTimeout is the ring window assumed for a phone-dialing call
	// (CreateSIPParticipant with WaitUntilAnswered, TransferSIPParticipant,
	// AcceptWhatsAppCall) when the request doesn't set ringing_timeout; it matches
	// the server default.
	defaultRingingTimeout = 30 * time.Second
	// ringingTimeoutMargin keeps a dialing request's deadline above the ringing
	// timeout, so it doesn't abort before the call can be answered.
	ringingTimeoutMargin = 2 * time.Second
)

// perAttemptTimeoutKey carries the caller's original timeout budget once its
// deadline has been detached (see withFailoverTimeout), so the transport can
// re-apply the full budget to each attempt instead of sharing one shrinking
// deadline across retries.
type perAttemptTimeoutKey struct{}

// failoverConfig is the resolved per-request region-failover configuration.
type failoverConfig struct {
	enabled     bool
	force       bool          // bypass the cloud-host check (test-only)
	backoffBase time.Duration // retry backoff base (test-only)
}

// attempts returns the total request attempts for a host; 1 means no failover.
// Failover engages only when enabled and the host is a LiveKit Cloud domain
// (or force is set).
func (c failoverConfig) attempts(hostname string) int {
	if c.enabled && (c.force || isCloud(hostname)) {
		return failoverMaxAttempts
	}
	return 1
}

type failoverEnabledKey struct{}
type failoverForceKey struct{}

// WithFailover returns a context that enables or disables region failover for
// API requests made with it (enabled by default). Failover only engages for
// LiveKit Cloud hosts. Pass the returned context to any service client method.
func WithFailover(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, failoverEnabledKey{}, enabled)
}

// withFailoverForce returns a context that forces failover on regardless of
// host and overrides the retry backoff. Internal, test-only.
func withFailoverForce(ctx context.Context, backoff time.Duration) context.Context {
	return context.WithValue(ctx, failoverForceKey{}, backoff)
}

func failoverConfigFromContext(ctx context.Context) failoverConfig {
	cfg := failoverConfig{enabled: true, backoffBase: failoverBackoffBase}
	if enabled, ok := ctx.Value(failoverEnabledKey{}).(bool); ok {
		cfg.enabled = enabled
	}
	if backoff, ok := ctx.Value(failoverForceKey{}).(time.Duration); ok {
		cfg.force = true
		cfg.backoffBase = backoff
	}
	return cfg
}

// withFailoverTimeout prepares a context for region failover. When the request
// has a deadline long enough to fail over (>= minFailoverTimeout) and failover
// is enabled, it detaches the deadline — so it isn't enforced across retries by
// twirp or the transport — and stashes the original budget for the transport to
// re-apply per attempt. Shorter or deadline-free requests are returned
// unchanged. Called once where the context enters the API client (prepareContext).
func withFailoverTimeout(ctx context.Context) context.Context {
	if !failoverConfigFromContext(ctx).enabled {
		return ctx
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx
	}
	if timeout := time.Until(deadline); timeout >= minFailoverTimeout {
		return context.WithValue(detachDeadline(ctx), perAttemptTimeoutKey{}, timeout)
	}
	return ctx
}

// detachDeadline returns a context with parent's values that propagates parent's
// explicit cancellation but not its deadline. The failover loop resets the
// timeout per attempt, so the original deadline must not fire mid-failover.
func detachDeadline(parent context.Context) context.Context {
	if parent.Done() == nil {
		return parent // nothing to detach or forward
	}
	detached, cancel := context.WithCancel(context.WithoutCancel(parent))
	go func() {
		<-parent.Done()
		// Forward explicit cancellation only; the original deadline is reset per
		// attempt, so let it lapse.
		if errors.Is(context.Cause(parent), context.DeadlineExceeded) {
			return
		}
		cancel()
	}()
	return detached
}

// perAttemptBudget is the per-attempt timeout for a request: the budget stashed
// by withFailoverTimeout, the remaining time on a raw deadline (e.g. when the
// transport is exercised directly), or the default when the caller set neither.
func perAttemptBudget(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(perAttemptTimeoutKey{}).(time.Duration); ok {
		return d
	}
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return defaultRequestTimeout
}

// hasPerAttemptTimeout reports whether withFailoverTimeout already detached the
// deadline and stashed the budget (so the transport needn't detach again).
func hasPerAttemptTimeout(ctx context.Context) bool {
	_, ok := ctx.Value(perAttemptTimeoutKey{}).(time.Duration)
	return ok
}

// newAPIHTTPClient returns the *http.Client used by every API service client.
// It wraps the default transport with region-failover retries.
func newAPIHTTPClient() *http.Client {
	return &http.Client{
		Transport: &failoverTransport{base: http.DefaultTransport, regions: sharedAPIRegions},
	}
}

// failoverTransport orchestrates region failover for a single API request. On
// a retryable failure (any transport error or HTTP 5xx) it discovers
// alternative regions via /settings/regions and replays the request — body and
// headers intact — against the next region, with exponential backoff. 4xx
// responses are returned immediately.
type failoverTransport struct {
	base    http.RoundTripper
	regions *regionCache
}

func (t *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)

	cfg := failoverConfigFromContext(req.Context())
	maxAttempts := cfg.attempts(req.URL.Hostname())

	// Each attempt gets the caller's full timeout budget, reset per attempt. A
	// budget shorter than minFailoverTimeout skips failover (thundering-herd
	// guard); a disabled or non-cloud host already has maxAttempts == 1.
	timeout := perAttemptBudget(req.Context())
	if timeout > 0 && timeout < minFailoverTimeout {
		maxAttempts = 1
	}

	// No failover: a single attempt. Re-apply the budget in case
	// withFailoverTimeout detached the deadline upstream (it can't know the host
	// is non-cloud), so the lone attempt is still bounded.
	if maxAttempts == 1 {
		ctx, cancel := withOptionalTimeout(req.Context(), timeout)
		resp, err := t.base.RoundTrip(req.WithContext(ctx))
		return terminate(resp, err, cancel)
	}

	return t.failover(req, maxAttempts, timeout, cfg.backoffBase)
}

// failover replays the request against successive regions, each with a fresh
// timeout budget and exponential backoff, until success, a non-retryable error,
// caller cancellation, or the attempts/regions are exhausted.
func (t *failoverTransport) failover(req *http.Request, maxAttempts int, timeout, backoffBase time.Duration) (*http.Response, error) {
	// Buffer the body so it can be re-sent to each region.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}

	// Attempts run on a deadline-free context (the deadline is reset per attempt).
	// withFailoverTimeout normally detaches it upstream; detach a raw deadline
	// that reaches the transport directly (e.g. in tests) too.
	base := req.Context()
	if !hasPerAttemptTimeout(base) {
		if _, ok := base.Deadline(); ok {
			base = detachDeadline(base)
		}
	}

	scheme, host := req.URL.Scheme, req.URL.Host
	tried := map[string]struct{}{strings.ToLower(host): {}}
	var regions *livekit.RegionSettings // discovered lazily on the first failure

	var resp *http.Response
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		attemptCtx, cancel := withOptionalTimeout(base, timeout)

		r := req.Clone(attemptCtx)
		r.URL.Scheme, r.URL.Host, r.Host = scheme, host, host
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}
		resp, err = t.base.RoundTrip(r)

		// Stop on success, a non-retryable 4xx, caller cancellation, or the last
		// attempt. terminate defers the per-attempt cancel to the body's Close.
		if !isRetryable(resp, err) || attempt == maxAttempts-1 {
			return terminate(resp, err, cancel)
		}

		if regions == nil {
			u := url.URL{Scheme: req.URL.Scheme, Host: req.URL.Host, Path: "/settings/regions"}
			regions, _ = t.regions.get(req.URL.Host, u.String(), req.Header, 0)
		}
		nextScheme, nextHost, ok := nextRegion(regions, tried)
		if !ok {
			return terminate(resp, err, cancel) // no untried region left
		}

		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		logger.Warnw("livekit API request failed, retrying with fallback url", err,
			"failedUrl", scheme+"://"+host, "fallbackUrl", nextScheme+"://"+nextHost,
			"attempt", attempt+1, "maxAttempts", maxAttempts, "status", status)

		drainResponse(resp)
		cancel() // this attempt's body is drained; release its timer
		if !sleepCtx(base, backoffBase<<uint(attempt)) {
			return nil, base.Err() // caller cancelled during backoff
		}
		scheme, host = nextScheme, nextHost
		tried[strings.ToLower(host)] = struct{}{}
	}
	return resp, err // unreachable: the loop returns on the final attempt
}

// withOptionalTimeout applies a per-attempt timeout when one is set; otherwise
// it returns ctx unchanged with a no-op cancel.
func withOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// terminate hands a response back to the caller, deferring context cleanup to
// the response body's Close so the body stays readable. With no body it cleans
// up immediately.
func terminate(resp *http.Response, err error, cleanup func()) (*http.Response, error) {
	if resp != nil && resp.Body != nil {
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cleanup: cleanup}
	} else {
		cleanup()
	}
	return resp, err
}

// cancelOnClose runs cleanup once the wrapped response body is closed, releasing
// the per-attempt context's timer without truncating the body early.
type cancelOnClose struct {
	io.ReadCloser
	cleanup func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cleanup()
	return err
}

// isRetryable reports whether a region failover should be attempted: a transport
// error, an HTTP 5xx response, or a per-attempt timeout (an unresponsive region
// is worth retrying against another region, with a fresh budget). 4xx and
// success are terminal, as is caller cancellation — if the caller gave up, we
// stop rather than failing over.
func isRetryable(resp *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled)
	}
	return resp != nil && resp.StatusCode >= 500
}

// nextRegion returns the first region whose host has not yet been attempted.
func nextRegion(settings *livekit.RegionSettings, attempted map[string]struct{}) (scheme, host string, ok bool) {
	if settings == nil {
		return "", "", false
	}
	for _, region := range settings.Regions {
		u, err := url.Parse(signalling.ToHttpURL(region.Url))
		if err != nil || u.Host == "" {
			continue
		}
		if _, seen := attempted[strings.ToLower(u.Host)]; seen {
			continue
		}
		return u.Scheme, u.Host, true
	}
	return "", "", false
}

func drainResponse(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// sharedAPIRegions is the process-wide region cache used by the API failover
// path. The RTC signaling path has its own regionURLProvider over a separate
// cache, but both share the same regionCache discovery/fetch/TTL logic.
var sharedAPIRegions = newRegionCache()

// RegionError indicates the /settings/regions endpoint returned a non-200
// status while attempting region failover.
type RegionError struct {
	StatusCode int
}

func (e *RegionError) Error() string {
	return "failed to fetch region settings: status " + http.StatusText(e.StatusCode)
}

// TwirpRegionError is a deprecated alias for [RegionError].
//
// Deprecated: use RegionError.
type TwirpRegionError = RegionError
