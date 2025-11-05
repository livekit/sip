// Copyright 2025 LiveKit, Inc.
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

package sip

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/livekit/protocol/logger"
)

const (
	// RFC 4028 minimum session interval
	minSessionExpiresRFC = 90

	// Default session interval (30 minutes)
	defaultSessionExpires = 1800

	// Extension name for Supported/Require headers
	timerExtension = "timer"
)

// RefresherRole indicates which party is responsible for refreshing the session
type RefresherRole int

const (
	RefresherNone RefresherRole = iota
	RefresherUAC
	RefresherUAS
)

func (r RefresherRole) String() string {
	switch r {
	case RefresherUAC:
		return "uac"
	case RefresherUAS:
		return "uas"
	default:
		return "none"
	}
}

func parseRefresherRole(s string) RefresherRole {
	switch strings.ToLower(s) {
	case "uac":
		return RefresherUAC
	case "uas":
		return RefresherUAS
	default:
		return RefresherNone
	}
}

// SessionTimerConfig holds configuration for session timers
type SessionTimerConfig struct {
	DefaultExpires  int           // Default session interval in seconds
	MinSE           int           // Minimum session interval in seconds
	PreferRefresher RefresherRole // Preferred refresher role
	UseUpdate       bool          // Use UPDATE instead of re-INVITE for refresh
}

// DefaultSessionTimerConfig returns the default session timer configuration
func DefaultSessionTimerConfig() SessionTimerConfig {
	return SessionTimerConfig{
		DefaultExpires:  defaultSessionExpires,
		MinSE:           minSessionExpiresRFC,
		PreferRefresher: RefresherUAC,
		UseUpdate:       false,
	}
}

// SessionTimer manages RFC 4028 session timers for a SIP dialog
type SessionTimer struct {
	mu sync.Mutex

	config SessionTimerConfig
	log    logger.Logger

	// Negotiated parameters
	sessionExpires int           // Negotiated session interval in seconds
	refresher      RefresherRole // Who is responsible for refresh
	isUAC          bool          // Are we the UAC in this dialog?

	// Timers
	refreshTimer   *time.Timer // Timer for sending refresh
	expiryTimer    *time.Timer // Timer for session expiry
	expiryGeneration uint64     // Generation counter to invalidate old expiry timers
	lastRefresh    time.Time   // Timestamp of last refresh

	// Callbacks
	onRefresh func(ctx context.Context) error // Callback to send refresh request
	onExpiry  func(ctx context.Context) error // Callback to handle session expiry

	// State
	started bool
	stopped bool
	ctx     context.Context
}

// NewSessionTimer creates a new session timer
func NewSessionTimer(config SessionTimerConfig, isUAC bool, log logger.Logger) *SessionTimer {
	if log == nil {
		log = logger.GetLogger()
	}

	return &SessionTimer{
		config:         config,
		log:            log,
		isUAC:          isUAC,
		sessionExpires: config.DefaultExpires,
		refresher:      RefresherNone,
	}
}

// SetContext sets the context for the session timer
func (st *SessionTimer) SetContext(ctx context.Context) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ctx = ctx
}

// SetCallbacks sets the refresh and expiry callbacks
func (st *SessionTimer) SetCallbacks(onRefresh, onExpiry func(ctx context.Context) error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onRefresh = onRefresh
	st.onExpiry = onExpiry
}

// NegotiateInvite negotiates session timer parameters from an incoming INVITE request
// Returns the negotiated values and any error (including 422 rejection)
func (st *SessionTimer) NegotiateInvite(req *sip.Request) (sessionExpires int, minSE int, refresher RefresherRole, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Check for Session-Expires header
	seHeader := req.GetHeader("Session-Expires")
	if seHeader == nil {
		// No session timer requested
		return 0, 0, RefresherNone, nil
	}

	// Parse Session-Expires header: "1800;refresher=uac"
	parts := strings.Split(seHeader.Value(), ";")
	requestedExpires, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		st.log.Warnw("Invalid Session-Expires header", err, "value", seHeader.Value())
		return 0, 0, RefresherNone, fmt.Errorf("invalid Session-Expires value")
	}

	// Parse refresher parameter if present
	requestedRefresher := RefresherNone
	for _, part := range parts[1:] {
		kv := strings.Split(part, "=")
		if len(kv) == 2 && strings.TrimSpace(strings.ToLower(kv[0])) == "refresher" {
			requestedRefresher = parseRefresherRole(strings.TrimSpace(kv[1]))
		}
	}

	// Check Min-SE header
	minSEHeader := req.GetHeader("Min-SE")
	requestedMinSE := st.config.MinSE
	if minSEHeader != nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(minSEHeader.Value())); err == nil {
			if parsed > requestedMinSE {
				requestedMinSE = parsed
			}
		}
	}

	// Enforce our minimum
	if requestedExpires < st.config.MinSE {
		st.log.Infow("Session interval too small, rejecting with 422",
			"requested", requestedExpires,
			"minSE", st.config.MinSE)
		return 0, st.config.MinSE, RefresherNone, fmt.Errorf("session interval too small: %d < %d", requestedExpires, st.config.MinSE)
	}

	// Accept the requested interval
	negotiatedExpires := requestedExpires

	// Determine refresher role
	// UAS (us) decides the final refresher role
	negotiatedRefresher := requestedRefresher
	if negotiatedRefresher == RefresherNone {
		// If not specified, use our preference
		negotiatedRefresher = st.config.PreferRefresher
		if negotiatedRefresher == RefresherNone {
			// Default to UAC if still unspecified
			negotiatedRefresher = RefresherUAC
		}
	}

	st.sessionExpires = negotiatedExpires
	st.refresher = negotiatedRefresher

	st.log.Infow("Negotiated session timer from INVITE",
		"sessionExpires", negotiatedExpires,
		"minSE", requestedMinSE,
		"refresher", negotiatedRefresher.String())

	return negotiatedExpires, requestedMinSE, negotiatedRefresher, nil
}

// NegotiateResponse negotiates session timer parameters from a response (for UAC)
// This is called when we receive a 2xx response to our INVITE
func (st *SessionTimer) NegotiateResponse(res *sip.Response) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Check for Session-Expires header in response
	seHeader := res.GetHeader("Session-Expires")
	if seHeader == nil {
		// UAS doesn't support session timers
		st.log.Infow("UAS doesn't support session timers (no Session-Expires in response)")
		return nil
	}

	// Parse Session-Expires header
	parts := strings.Split(seHeader.Value(), ";")
	negotiatedExpires, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		st.log.Warnw("Invalid Session-Expires in response", err, "value", seHeader.Value())
		return fmt.Errorf("invalid Session-Expires value")
	}

	// Parse refresher parameter
	negotiatedRefresher := RefresherNone
	for _, part := range parts[1:] {
		kv := strings.Split(part, "=")
		if len(kv) == 2 && strings.TrimSpace(strings.ToLower(kv[0])) == "refresher" {
			negotiatedRefresher = parseRefresherRole(strings.TrimSpace(kv[1]))
		}
	}

	if negotiatedRefresher == RefresherNone {
		// If not specified, default to UAC (us)
		negotiatedRefresher = RefresherUAC
	}

	st.sessionExpires = negotiatedExpires
	st.refresher = negotiatedRefresher

	st.log.Infow("Negotiated session timer from response",
		"sessionExpires", negotiatedExpires,
		"refresher", negotiatedRefresher.String())

	return nil
}

// AddHeadersToRequest adds session timer headers to an outgoing INVITE request
func (st *SessionTimer) AddHeadersToRequest(req *sip.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Add Supported: timer
	req.AppendHeader(sip.NewHeader("Supported", timerExtension))

	// Add Session-Expires header
	sessionExpires := st.config.DefaultExpires
	refresher := st.config.PreferRefresher
	if refresher == RefresherNone {
		refresher = RefresherUAC // Default to UAC
	}

	seValue := fmt.Sprintf("%d;refresher=%s", sessionExpires, refresher.String())
	req.AppendHeader(sip.NewHeader("Session-Expires", seValue))

	// Add Min-SE header
	req.AppendHeader(sip.NewHeader("Min-SE", strconv.Itoa(st.config.MinSE)))

	st.log.Debugw("Added session timer headers to INVITE",
		"sessionExpires", sessionExpires,
		"minSE", st.config.MinSE,
		"refresher", refresher.String())
}

// AddHeadersToResponse adds session timer headers to a response
func (st *SessionTimer) AddHeadersToResponse(res *sip.Response, sessionExpires int, refresher RefresherRole) {
	if sessionExpires == 0 {
		return
	}

	// Add Require: timer (to indicate timer support is required)
	res.AppendHeader(sip.NewHeader("Require", timerExtension))

	// Add Session-Expires header
	seValue := fmt.Sprintf("%d;refresher=%s", sessionExpires, refresher.String())
	res.AppendHeader(sip.NewHeader("Session-Expires", seValue))

	st.log.Debugw("Added session timer headers to response",
		"sessionExpires", sessionExpires,
		"refresher", refresher.String())
}

// Start starts the session timer
func (st *SessionTimer) Start() {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.started || st.sessionExpires == 0 {
		return
	}

	st.started = true
	st.lastRefresh = time.Now()

	if st.ctx == nil {
		st.log.Warnw("Session timer started without context", nil)
		return
	}

	// Determine if we are the refresher
	weAreRefresher := (st.isUAC && st.refresher == RefresherUAC) || (!st.isUAC && st.refresher == RefresherUAS)

	if weAreRefresher {
		// We are responsible for refreshing
		// Refresh at half the session interval (per RFC 4028)
		refreshInterval := time.Duration(st.sessionExpires/2) * time.Second
		st.refreshTimer = time.AfterFunc(refreshInterval, func() {
			st.handleRefresh()
		})

		st.log.Infow("Started session timer as refresher",
			"sessionExpires", st.sessionExpires,
			"refreshIn", refreshInterval)
	}

	// Always set expiry timer (both refresher and non-refresher)
	// Expiry warning at: expires - min(32, expires/3) seconds
	st.expiryGeneration++
	currentGen := st.expiryGeneration
	expiryWarning := st.sessionExpires - min(32, st.sessionExpires/3)
	expiryDuration := time.Duration(expiryWarning) * time.Second
	st.expiryTimer = time.AfterFunc(expiryDuration, func() {
		st.handleExpiry(currentGen)
	})

	st.log.Infow("Started session timer",
		"sessionExpires", st.sessionExpires,
		"expiryWarning", expiryDuration,
		"weAreRefresher", weAreRefresher)
}

// Stop stops the session timer
func (st *SessionTimer) Stop() {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.stopped {
		return
	}

	st.stopped = true

	if st.refreshTimer != nil {
		st.refreshTimer.Stop()
		st.refreshTimer = nil
	}

	if st.expiryTimer != nil {
		st.expiryTimer.Stop()
		st.expiryTimer = nil
	}

	st.log.Infow("Stopped session timer")
}

// OnRefreshReceived should be called when a session refresh request is received
// This resets the expiry timer
func (st *SessionTimer) OnRefreshReceived() {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.started || st.stopped {
		return
	}

	st.lastRefresh = time.Now()

	// Reset expiry timer
	if st.expiryTimer != nil {
		st.expiryTimer.Stop()
	}

	st.expiryGeneration++
	currentGen := st.expiryGeneration
	expiryWarning := st.sessionExpires - min(32, st.sessionExpires/3)
	expiryDuration := time.Duration(expiryWarning) * time.Second
	st.expiryTimer = time.AfterFunc(expiryDuration, func() {
		st.handleExpiry(currentGen)
	})

	st.log.Infow("Session refresh received, reset expiry timer",
		"sessionExpires", st.sessionExpires,
		"nextExpiry", expiryDuration)
}

// handleRefresh is called when it's time to send a session refresh
func (st *SessionTimer) handleRefresh() {
	st.mu.Lock()
	if st.stopped || st.ctx == nil {
		st.mu.Unlock()
		return
	}

	ctx := st.ctx
	onRefresh := st.onRefresh
	st.mu.Unlock()

	if onRefresh == nil {
		st.log.Warnw("No refresh callback registered", nil)
		return
	}

	st.log.Infow("Sending session refresh")

	err := onRefresh(ctx)
	if err != nil {
		st.log.Errorw("Failed to send session refresh", err)
		// Don't reschedule on error - let expiry timer handle it
		return
	}

	// Reschedule next refresh
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.stopped {
		return
	}

	st.lastRefresh = time.Now()

	refreshInterval := time.Duration(st.sessionExpires/2) * time.Second
	st.refreshTimer = time.AfterFunc(refreshInterval, func() {
		st.handleRefresh()
	})

	st.log.Infow("Session refresh sent, scheduled next refresh",
		"nextRefresh", refreshInterval)
}

// handleExpiry is called when the session expires without refresh
func (st *SessionTimer) handleExpiry(generation uint64) {
	st.mu.Lock()
	// Check if this timer is stale (a newer timer was created)
	if generation != st.expiryGeneration {
		st.mu.Unlock()
		return
	}
	if st.stopped || st.ctx == nil {
		st.mu.Unlock()
		return
	}

	ctx := st.ctx
	onExpiry := st.onExpiry
	st.mu.Unlock()

	if onExpiry == nil {
		st.log.Warnw("No expiry callback registered", nil)
		return
	}

	st.log.Warnw("Session timer expired, terminating call", nil,
		"sessionExpires", st.sessionExpires,
		"lastRefresh", st.lastRefresh)

	err := onExpiry(ctx)
	if err != nil {
		st.log.Errorw("Failed to handle session expiry", err)
	}

	st.Stop()
}

// GetSessionExpires returns the negotiated session expires value
func (st *SessionTimer) GetSessionExpires() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessionExpires
}

// GetRefresher returns the negotiated refresher role
func (st *SessionTimer) GetRefresher() RefresherRole {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.refresher
}

// IsStarted returns whether the timer is started
func (st *SessionTimer) IsStarted() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.started
}

