// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
)

func newTestAlertMgr(webhookURL string, cooldown time.Duration) *AlertManager {
	return NewAlertManager(logger.GetLogger(), AlertManagerConfig{
		Enabled:        true,
		WebhookURL:     webhookURL,
		CooldownPeriod: cooldown,
		NodeID:         "test-node",
	})
}

func TestAlertManager_Disabled(t *testing.T) {
	am := NewAlertManager(logger.GetLogger(), AlertManagerConfig{Enabled: false})
	// Should not panic or fire
	am.Fire(Alert{Name: "test", Severity: SeverityCritical, Message: "boom"})
	stats := am.Stats()
	if stats.TotalFired != 0 {
		t.Errorf("expected 0 fires when disabled, got %d", stats.TotalFired)
	}
}

func TestAlertManager_FireAndWebhook(t *testing.T) {
	var received []Alert
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var alert Alert
		json.NewDecoder(r.Body).Decode(&alert)
		mu.Lock()
		received = append(received, alert)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	am := newTestAlertMgr(srv.URL, time.Millisecond)
	am.Fire(Alert{
		Name:     "test_alert",
		Severity: SeverityCritical,
		Message:  "something broke",
		Labels:   map[string]string{"env": "test"},
	})

	// Wait for async webhook
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 webhook call, got %d", len(received))
	}
	if received[0].Name != "test_alert" {
		t.Errorf("expected name test_alert, got %s", received[0].Name)
	}
	if received[0].NodeID != "test-node" {
		t.Errorf("expected node_id test-node, got %s", received[0].NodeID)
	}
	if received[0].Severity != SeverityCritical {
		t.Errorf("expected critical severity, got %s", received[0].Severity)
	}
}

func TestAlertManager_CooldownSuppression(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	am := newTestAlertMgr(srv.URL, 5*time.Second)

	// Fire same alert twice rapidly
	am.Fire(Alert{Name: "dup_alert", Severity: SeverityWarning, Message: "first"})
	am.Fire(Alert{Name: "dup_alert", Severity: SeverityWarning, Message: "second"})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("expected 1 webhook call (second suppressed), got %d", callCount)
	}

	stats := am.Stats()
	if stats.TotalFired != 1 {
		t.Errorf("expected 1 fired, got %d", stats.TotalFired)
	}
	if stats.TotalSuppressed != 1 {
		t.Errorf("expected 1 suppressed, got %d", stats.TotalSuppressed)
	}
}

func TestAlertManager_DifferentAlertsNotSuppressed(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	am := newTestAlertMgr(srv.URL, 5*time.Second)

	am.Fire(Alert{Name: "alert_a", Severity: SeverityCritical, Message: "a"})
	am.Fire(Alert{Name: "alert_b", Severity: SeverityCritical, Message: "b"})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Errorf("expected 2 webhook calls (different names), got %d", callCount)
	}
}

func TestAlertManager_CooldownExpiry(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	am := newTestAlertMgr(srv.URL, 100*time.Millisecond)

	am.Fire(Alert{Name: "cooldown_test", Severity: SeverityWarning, Message: "first"})
	time.Sleep(200 * time.Millisecond)
	am.Fire(Alert{Name: "cooldown_test", Severity: SeverityWarning, Message: "second after cooldown"})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Errorf("expected 2 webhook calls after cooldown, got %d", callCount)
	}
}

func TestAlertManager_FireCritical(t *testing.T) {
	am := newTestAlertMgr("", time.Second)
	am.FireCritical("test_crit", "critical event", map[string]string{"k": "v"})
	stats := am.Stats()
	if stats.TotalFired != 1 {
		t.Errorf("expected 1 fire, got %d", stats.TotalFired)
	}
}

func TestAlertManager_FireWarning(t *testing.T) {
	am := newTestAlertMgr("", time.Second)
	am.FireWarning("test_warn", "warning event", nil)
	stats := am.Stats()
	if stats.TotalFired != 1 {
		t.Errorf("expected 1 fire, got %d", stats.TotalFired)
	}
}

func TestAlertManager_NoWebhookURL(t *testing.T) {
	am := newTestAlertMgr("", time.Millisecond)
	// Should not panic when webhook URL is empty
	am.Fire(Alert{Name: "no_url", Severity: SeverityInfo, Message: "test"})
	stats := am.Stats()
	if stats.TotalFired != 1 {
		t.Errorf("expected 1 fire, got %d", stats.TotalFired)
	}
}

func TestAlertManager_Stats(t *testing.T) {
	am := newTestAlertMgr("", 5*time.Second)
	am.Fire(Alert{Name: "s1", Severity: SeverityInfo, Message: "a"})
	am.Fire(Alert{Name: "s1", Severity: SeverityInfo, Message: "b"}) // suppressed
	am.Fire(Alert{Name: "s2", Severity: SeverityInfo, Message: "c"})

	stats := am.Stats()
	if !stats.Enabled {
		t.Error("expected enabled=true")
	}
	if stats.TotalFired != 2 {
		t.Errorf("expected 2 fired, got %d", stats.TotalFired)
	}
	if stats.TotalSuppressed != 1 {
		t.Errorf("expected 1 suppressed, got %d", stats.TotalSuppressed)
	}
}
