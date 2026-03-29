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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// AlertSeverity indicates alert urgency.
type AlertSeverity string

const (
	SeverityCritical AlertSeverity = "critical"
	SeverityWarning  AlertSeverity = "warning"
	SeverityInfo     AlertSeverity = "info"
)

// Alert represents a single alert event.
type Alert struct {
	Timestamp time.Time     `json:"timestamp"`
	Severity  AlertSeverity `json:"severity"`
	Name      string        `json:"name"`
	Message   string        `json:"message"`
	NodeID    string        `json:"node_id"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// AlertManagerConfig configures the alert manager.
type AlertManagerConfig struct {
	Enabled        bool
	WebhookURL     string
	CooldownPeriod time.Duration // min interval between alerts with the same name
	NodeID         string
}

// AlertManager sends alerts to external systems via webhooks.
// Supports cooldown deduplication to avoid alert storms.
type AlertManager struct {
	log    logger.Logger
	config AlertManagerConfig
	client *http.Client

	mu       sync.Mutex
	lastFire map[string]time.Time // alert name → last fire time

	// Stats
	totalFired    int64
	totalSuppressed int64
}

// NewAlertManager creates a new alert manager.
func NewAlertManager(log logger.Logger, cfg AlertManagerConfig) *AlertManager {
	if cfg.CooldownPeriod <= 0 {
		cfg.CooldownPeriod = 5 * time.Minute
	}

	return &AlertManager{
		log:    log.WithValues("component", "alerting"),
		config: cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		lastFire: make(map[string]time.Time),
	}
}

// Fire sends an alert. The alert is suppressed if another alert with the
// same name was fired within the cooldown period.
func (am *AlertManager) Fire(alert Alert) {
	if !am.config.Enabled {
		return
	}

	alert.NodeID = am.config.NodeID
	if alert.Timestamp.IsZero() {
		alert.Timestamp = time.Now()
	}

	// Cooldown check
	am.mu.Lock()
	last, exists := am.lastFire[alert.Name]
	if exists && time.Since(last) < am.config.CooldownPeriod {
		am.totalSuppressed++
		am.mu.Unlock()
		am.log.Debugw("alert suppressed (cooldown)", "name", alert.Name)
		return
	}
	am.lastFire[alert.Name] = time.Now()
	am.totalFired++
	am.mu.Unlock()

	am.log.Infow("firing alert",
		"name", alert.Name,
		"severity", alert.Severity,
		"message", alert.Message,
	)

	// Send webhook asynchronously
	go am.sendWebhook(alert)
}

// FireCritical is a convenience method for critical alerts.
func (am *AlertManager) FireCritical(name, message string, labels map[string]string) {
	am.Fire(Alert{
		Severity: SeverityCritical,
		Name:     name,
		Message:  message,
		Labels:   labels,
	})
}

// FireWarning is a convenience method for warning alerts.
func (am *AlertManager) FireWarning(name, message string, labels map[string]string) {
	am.Fire(Alert{
		Severity: SeverityWarning,
		Name:     name,
		Message:  message,
		Labels:   labels,
	})
}

// Stats returns alert manager statistics.
func (am *AlertManager) Stats() AlertManagerStats {
	am.mu.Lock()
	defer am.mu.Unlock()
	return AlertManagerStats{
		Enabled:         am.config.Enabled,
		TotalFired:      am.totalFired,
		TotalSuppressed: am.totalSuppressed,
	}
}

// AlertManagerStats holds alert statistics.
type AlertManagerStats struct {
	Enabled         bool  `json:"enabled"`
	TotalFired      int64 `json:"total_fired"`
	TotalSuppressed int64 `json:"total_suppressed"`
}

func (am *AlertManager) sendWebhook(alert Alert) {
	if am.config.WebhookURL == "" {
		return
	}

	body, err := json.Marshal(alert)
	if err != nil {
		am.log.Warnw("failed to marshal alert", err)
		return
	}

	resp, err := am.client.Post(am.config.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		am.log.Warnw("failed to send alert webhook", err,
			"url", am.config.WebhookURL,
			"alert", alert.Name,
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		am.log.Warnw("alert webhook returned non-2xx",
			fmt.Errorf("status %d", resp.StatusCode),
			"url", am.config.WebhookURL,
			"alert", alert.Name,
		)
	}
}

// --- Predefined alert names ---

const (
	AlertCircuitBreakerTrip = "circuit_breaker_trip"
	AlertKillSwitchActivated = "kill_switch_activated"
	AlertHighErrorRate       = "high_error_rate"
	AlertCapacityNearLimit   = "capacity_near_limit"
	AlertSessionTimeout      = "session_timeout"
	AlertTranscodeOverload   = "transcode_overload"
)
