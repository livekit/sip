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
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/sipgo/sip"
	"github.com/livekit/protocol/logger"
)

func TestSessionTimerNegotiateInvite(t *testing.T) {
	tests := []struct {
		name                string
		config              SessionTimerConfig
		sessionExpiresValue string
		minSEValue          string
		expectError         bool
		expectedExpires     int
		expectedRefresher   RefresherRole
	}{
		{
			name: "valid session timer with refresher=uac",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAC,
			},
			sessionExpiresValue: "1800;refresher=uac",
			minSEValue:          "90",
			expectError:         false,
			expectedExpires:     1800,
			expectedRefresher:   RefresherUAC,
		},
		{
			name: "valid session timer with refresher=uas",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAS,
			},
			sessionExpiresValue: "1800;refresher=uas",
			minSEValue:          "90",
			expectError:         false,
			expectedExpires:     1800,
			expectedRefresher:   RefresherUAS,
		},
		{
			name: "session interval too small",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAC,
			},
			sessionExpiresValue: "60",
			minSEValue:          "90",
			expectError:         true,
		},
		{
			name: "no session expires header",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAC,
			},
			sessionExpiresValue: "",
			expectError:         false,
			expectedExpires:     0,
			expectedRefresher:   RefresherNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := logger.GetLogger()
			st := NewSessionTimer(tt.config, false, log)

			// Create a mock INVITE request
			req := sip.NewRequest(sip.INVITE, sip.Uri{User: "test", Host: "example.com"})
			if tt.sessionExpiresValue != "" {
				req.AppendHeader(sip.NewHeader("Session-Expires", tt.sessionExpiresValue))
			}
			if tt.minSEValue != "" {
				req.AppendHeader(sip.NewHeader("Min-SE", tt.minSEValue))
			}

			sessionExpires, _, refresher, err := st.NegotiateInvite(req)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if sessionExpires != tt.expectedExpires {
					t.Errorf("Expected sessionExpires=%d, got %d", tt.expectedExpires, sessionExpires)
				}
				if refresher != tt.expectedRefresher {
					t.Errorf("Expected refresher=%v, got %v", tt.expectedRefresher, refresher)
				}
			}
		})
	}
}

func TestSessionTimerNegotiateResponse(t *testing.T) {
	tests := []struct {
		name                string
		config              SessionTimerConfig
		sessionExpiresValue string
		expectedExpires     int
		expectedRefresher   RefresherRole
	}{
		{
			name: "valid response with refresher=uac",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAC,
			},
			sessionExpiresValue: "1800;refresher=uac",
			expectedExpires:     1800,
			expectedRefresher:   RefresherUAC,
		},
		{
			name: "valid response with refresher=uas",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAS,
			},
			sessionExpiresValue: "1800;refresher=uas",
			expectedExpires:     1800,
			expectedRefresher:   RefresherUAS,
		},
		{
			name: "response without refresher defaults to uac",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAC,
			},
			sessionExpiresValue: "1800",
			expectedExpires:     1800,
			expectedRefresher:   RefresherUAC,
		},
		{
			name: "no session expires in response",
			config: SessionTimerConfig{
				Enabled:         true,
				DefaultExpires:  1800,
				MinSE:           90,
				PreferRefresher: RefresherUAC,
			},
			sessionExpiresValue: "",
			expectedExpires:     1800, // Should remain at default
			expectedRefresher:   RefresherNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := logger.GetLogger()
			st := NewSessionTimer(tt.config, true, log)

			// Create a mock 200 OK response
			req := sip.NewRequest(sip.INVITE, sip.Uri{User: "test", Host: "example.com"})
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			if tt.sessionExpiresValue != "" {
				res.AppendHeader(sip.NewHeader("Session-Expires", tt.sessionExpiresValue))
			}

			err := st.NegotiateResponse(res)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			actualExpires := st.GetSessionExpires()
			actualRefresher := st.GetRefresher()

			if actualExpires != tt.expectedExpires {
				t.Errorf("Expected sessionExpires=%d, got %d", tt.expectedExpires, actualExpires)
			}
			if actualRefresher != tt.expectedRefresher {
				t.Errorf("Expected refresher=%v, got %v", tt.expectedRefresher, actualRefresher)
			}
		})
	}
}

func TestSessionTimerAddHeadersToRequest(t *testing.T) {
	config := SessionTimerConfig{
		Enabled:         true,
		DefaultExpires:  1800,
		MinSE:           90,
		PreferRefresher: RefresherUAC,
	}

	log := logger.GetLogger()
	st := NewSessionTimer(config, true, log)

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "test", Host: "example.com"})
	st.AddHeadersToRequest(req)

	// Check for Supported header
	supportedHeader := req.GetHeader("Supported")
	if supportedHeader == nil || supportedHeader.Value() != "timer" {
		t.Errorf("Expected Supported: timer header")
	}

	// Check for Session-Expires header
	sessionExpiresHeader := req.GetHeader("Session-Expires")
	if sessionExpiresHeader == nil {
		t.Errorf("Expected Session-Expires header")
	}

	// Check for Min-SE header
	minSEHeader := req.GetHeader("Min-SE")
	if minSEHeader == nil || minSEHeader.Value() != "90" {
		t.Errorf("Expected Min-SE: 90 header")
	}
}

func TestSessionTimerAddHeadersToResponse(t *testing.T) {
	config := SessionTimerConfig{
		Enabled:         true,
		DefaultExpires:  1800,
		MinSE:           90,
		PreferRefresher: RefresherUAS,
	}

	log := logger.GetLogger()
	st := NewSessionTimer(config, false, log)

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "test", Host: "example.com"})
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)

	st.AddHeadersToResponse(res, 1800, RefresherUAS)

	// Check for Require header
	requireHeader := res.GetHeader("Require")
	if requireHeader == nil || requireHeader.Value() != "timer" {
		t.Errorf("Expected Require: timer header")
	}

	// Check for Session-Expires header
	sessionExpiresHeader := res.GetHeader("Session-Expires")
	if sessionExpiresHeader == nil {
		t.Errorf("Expected Session-Expires header")
	}
}

func TestSessionTimerRefreshCallback(t *testing.T) {
	config := SessionTimerConfig{
		Enabled:         true,
		DefaultExpires:  1, // 1 second for fast testing
		MinSE:           1,
		PreferRefresher: RefresherUAC,
	}

	log := logger.GetLogger()
	st := NewSessionTimer(config, true, log)
	st.sessionExpires = 1
	st.refresher = RefresherUAC

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st.SetContext(ctx)

	var refreshCalled atomic.Bool
	st.SetCallbacks(
		func(ctx context.Context) error {
			refreshCalled.Store(true)
			return nil
		},
		func(ctx context.Context) error {
			return nil
		},
	)

	st.Start()

	// Wait for refresh callback to be called (should happen at half interval = 0.5s)
	time.Sleep(750 * time.Millisecond)

	if !refreshCalled.Load() {
		t.Errorf("Refresh callback was not called")
	}

	st.Stop()
}

func TestSessionTimerExpiryCallback(t *testing.T) {
	config := SessionTimerConfig{
		Enabled:         true,
		DefaultExpires:  2, // 2 seconds for testing
		MinSE:           1,
		PreferRefresher: RefresherNone, // We are not the refresher
	}

	log := logger.GetLogger()
	st := NewSessionTimer(config, false, log)
	st.sessionExpires = 2
	st.refresher = RefresherUAC // Remote is refresher, but they won't refresh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st.SetContext(ctx)

	var expiryCalled atomic.Bool
	st.SetCallbacks(
		func(ctx context.Context) error {
			return nil
		},
		func(ctx context.Context) error {
			expiryCalled.Store(true)
			return nil
		},
	)

	st.Start()

	// Wait for expiry callback to be called
	// Expiry happens at: expires - min(32, expires/3) = 2 - min(32, 0) = 2 seconds
	time.Sleep(2500 * time.Millisecond)

	if !expiryCalled.Load() {
		t.Errorf("Expiry callback was not called")
	}

	st.Stop()
}

func TestSessionTimerOnRefreshReceived(t *testing.T) {
	config := SessionTimerConfig{
		Enabled:         true,
		DefaultExpires:  2,
		MinSE:           1,
		PreferRefresher: RefresherNone,
	}

	log := logger.GetLogger()
	st := NewSessionTimer(config, false, log)
	st.sessionExpires = 2
	st.refresher = RefresherUAC

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st.SetContext(ctx)

	var expiryCalled atomic.Bool
	st.SetCallbacks(
		func(ctx context.Context) error {
			return nil
		},
		func(ctx context.Context) error {
			expiryCalled.Store(true)
			return nil
		},
	)

	st.Start()

	// Wait a bit
	time.Sleep(500 * time.Millisecond)

	// Receive a refresh - this should reset the expiry timer
	st.OnRefreshReceived()

	// Wait for the original expiry time (should not expire because we refreshed)
	time.Sleep(2000 * time.Millisecond)

	if expiryCalled.Load() {
		t.Errorf("Expiry callback was called despite receiving refresh")
	}

	st.Stop()
}

func TestSessionTimerStop(t *testing.T) {
	config := SessionTimerConfig{
		Enabled:         true,
		DefaultExpires:  1,
		MinSE:           1,
		PreferRefresher: RefresherUAC,
	}

	log := logger.GetLogger()
	st := NewSessionTimer(config, true, log)
	st.sessionExpires = 1
	st.refresher = RefresherUAC

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st.SetContext(ctx)

	var refreshCalled atomic.Bool
	st.SetCallbacks(
		func(ctx context.Context) error {
			refreshCalled.Store(true)
			return nil
		},
		func(ctx context.Context) error {
			return nil
		},
	)

	st.Start()

	// Stop immediately
	st.Stop()

	// Wait to ensure callbacks are not called
	time.Sleep(1500 * time.Millisecond)

	if refreshCalled.Load() {
		t.Errorf("Refresh callback was called after Stop()")
	}
}
