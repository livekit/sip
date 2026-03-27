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

package resilience

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// AuditEventType categorizes audit events.
type AuditEventType string

const (
	AuditSessionStart    AuditEventType = "session.start"
	AuditSessionEnd      AuditEventType = "session.end"
	AuditSessionDegraded AuditEventType = "session.degraded"
	AuditSessionRecovered AuditEventType = "session.recovered"
	AuditFlagChanged     AuditEventType = "flag.changed"
	AuditFlagKillSwitch  AuditEventType = "flag.kill_switch"
	AuditFlagRevive      AuditEventType = "flag.revive"
	AuditFailure         AuditEventType = "failure"
	AuditCircuitTripped  AuditEventType = "circuit.tripped"
	AuditCircuitRecovered AuditEventType = "circuit.recovered"
	AuditGuardRejected   AuditEventType = "guard.rejected"
	AuditRolloutChanged  AuditEventType = "rollout.changed"
)

// AuditEvent is a single audit log entry.
type AuditEvent struct {
	Timestamp time.Time      `json:"ts"`
	Type      AuditEventType `json:"type"`
	SessionID string         `json:"session_id,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	NodeID    string         `json:"node_id,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// AuditLogger provides structured audit logging for compliance and debugging.
// All events are written to the structured logger AND stored in a ring buffer
// for the /audit API endpoint.
type AuditLogger struct {
	log    logger.Logger
	nodeID string

	mu      sync.Mutex
	ring    []AuditEvent
	ringIdx int
	ringCap int
}

// NewAuditLogger creates an audit logger with a ring buffer of the given capacity.
func NewAuditLogger(log logger.Logger, nodeID string, capacity int) *AuditLogger {
	if capacity <= 0 {
		capacity = 1000
	}
	return &AuditLogger{
		log:     log.WithValues("component", "audit"),
		nodeID:  nodeID,
		ring:    make([]AuditEvent, capacity),
		ringCap: capacity,
	}
}

// Log records an audit event.
func (a *AuditLogger) Log(event AuditEvent) {
	event.Timestamp = time.Now()
	event.NodeID = a.nodeID

	// Structured log output
	a.log.Infow("audit",
		"type", string(event.Type),
		"sessionID", event.SessionID,
		"callID", event.CallID,
		"detail", event.Detail,
		"meta", event.Meta,
	)

	// Ring buffer
	a.mu.Lock()
	a.ring[a.ringIdx%a.ringCap] = event
	a.ringIdx++
	a.mu.Unlock()
}

// --- Convenience methods ---

func (a *AuditLogger) SessionStart(sessionID, callID, room, from string) {
	a.Log(AuditEvent{
		Type:      AuditSessionStart,
		SessionID: sessionID,
		CallID:    callID,
		Meta:      map[string]any{"room": room, "from": from},
	})
}

func (a *AuditLogger) SessionEnd(sessionID, callID, reason string, durationSec int, stats map[string]any) {
	a.Log(AuditEvent{
		Type:      AuditSessionEnd,
		SessionID: sessionID,
		CallID:    callID,
		Detail:    reason,
		Meta:      mergeMap(map[string]any{"duration_sec": durationSec}, stats),
	})
}

func (a *AuditLogger) FlagChanged(flag string, enabled bool, changedBy string) {
	a.Log(AuditEvent{
		Type:   AuditFlagChanged,
		Detail: flag,
		Meta:   map[string]any{"enabled": enabled, "changed_by": changedBy},
	})
}

func (a *AuditLogger) KillSwitch(triggeredBy string) {
	a.Log(AuditEvent{
		Type:   AuditFlagKillSwitch,
		Detail: "all flags disabled",
		Meta:   map[string]any{"triggered_by": triggeredBy},
	})
}

func (a *AuditLogger) Revive(triggeredBy string) {
	a.Log(AuditEvent{
		Type:   AuditFlagRevive,
		Detail: "all flags re-enabled",
		Meta:   map[string]any{"triggered_by": triggeredBy},
	})
}

func (a *AuditLogger) Failure(sessionID, callID, component, detail string) {
	a.Log(AuditEvent{
		Type:      AuditFailure,
		SessionID: sessionID,
		CallID:    callID,
		Detail:    detail,
		Meta:      map[string]any{"component": component},
	})
}

func (a *AuditLogger) CircuitTripped(name string) {
	a.Log(AuditEvent{
		Type:   AuditCircuitTripped,
		Detail: name,
	})
}

func (a *AuditLogger) GuardRejected(callerID, reason string) {
	a.Log(AuditEvent{
		Type:   AuditGuardRejected,
		Detail: reason,
		Meta:   map[string]any{"caller": callerID},
	})
}

// Recent returns the last N audit events (most recent first).
func (a *AuditLogger) Recent(n int) []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()

	total := a.ringIdx
	if total > a.ringCap {
		total = a.ringCap
	}
	if n > total {
		n = total
	}
	if n <= 0 {
		return nil
	}

	result := make([]AuditEvent, n)
	for i := 0; i < n; i++ {
		idx := (a.ringIdx - 1 - i)
		if idx < 0 {
			idx += a.ringCap
		}
		result[i] = a.ring[idx%a.ringCap]
	}
	return result
}

// MarshalJSON returns the last 100 events as JSON.
func (a *AuditLogger) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.Recent(100))
}

func mergeMap(base, extra map[string]any) map[string]any {
	if extra == nil {
		return base
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}
