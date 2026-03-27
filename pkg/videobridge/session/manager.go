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

package session

import (
	"fmt"
	"sync"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/config"
	"github.com/livekit/sip/pkg/videobridge/signaling"
	"github.com/livekit/sip/pkg/videobridge/stats"
)

// RoomResolver maps an inbound SIP call to a LiveKit room name.
// Return empty string to reject the call.
type RoomResolver func(call *signaling.InboundCall) string

// Manager tracks all active bridging sessions and enforces concurrency limits.
type Manager struct {
	log  logger.Logger
	conf *config.Config

	resolver RoomResolver

	mu       sync.RWMutex
	sessions map[string]*Session // keyed by CallID
}

// NewManager creates a new session manager.
func NewManager(log logger.Logger, conf *config.Config) *Manager {
	return &Manager{
		log:      log,
		conf:     conf,
		sessions: make(map[string]*Session),
	}
}

// SetRoomResolver sets the function used to map SIP calls to LiveKit rooms.
func (m *Manager) SetRoomResolver(r RoomResolver) {
	m.resolver = r
}

// CreateSession creates and registers a new session for the given inbound call.
// Returns an error if the session limit is reached or the call is rejected.
func (m *Manager) CreateSession(call *signaling.InboundCall) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicate call
	if _, exists := m.sessions[call.CallID]; exists {
		return nil, fmt.Errorf("session already exists for call %s", call.CallID)
	}

	// Check concurrency limit
	maxSessions := m.conf.Transcode.MaxConcurrent
	if maxSessions > 0 && len(m.sessions) >= maxSessions {
		stats.SessionErrors.WithLabelValues("limit_reached").Inc()
		return nil, fmt.Errorf("session limit reached (%d/%d)", len(m.sessions), maxSessions)
	}

	// Resolve room name
	roomName := ""
	if m.resolver != nil {
		roomName = m.resolver(call)
	}
	if roomName == "" {
		// Default: use a room name derived from the called number
		roomName = fmt.Sprintf("sip-video-%s", call.CallID)
	}

	sess, err := NewSession(m.log, m.conf, call, roomName)
	if err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}

	m.sessions[call.CallID] = sess

	// Auto-cleanup when session closes
	go func() {
		<-sess.Closed()
		m.mu.Lock()
		delete(m.sessions, call.CallID)
		m.mu.Unlock()
		m.log.Infow("session removed from manager", "callID", call.CallID, "activeSessions", m.ActiveCount())
	}()

	m.log.Infow("session created",
		"callID", call.CallID,
		"room", roomName,
		"activeSessions", len(m.sessions),
	)

	return sess, nil
}

// GetSession returns the session for the given call ID, if it exists.
func (m *Manager) GetSession(callID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[callID]
	return s, ok
}

// RemoveSession terminates and removes the session for the given call ID.
func (m *Manager) RemoveSession(callID string) {
	m.mu.RLock()
	sess, ok := m.sessions[callID]
	m.mu.RUnlock()

	if ok {
		sess.Close()
	}
}

// ActiveCount returns the number of active sessions.
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CloseAll terminates all active sessions.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}

	m.log.Infow("all sessions closed")
}

// ListSessions returns info about all active sessions.
func (m *Manager) ListSessions() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		infos = append(infos, SessionInfo{
			ID:       s.ID,
			CallID:   s.CallID,
			RoomName: s.RoomName,
			FromURI:  s.FromURI,
			ToURI:    s.ToURI,
			State:    s.State().String(),
		})
	}
	return infos
}

// SessionInfo holds summary information about a session.
type SessionInfo struct {
	ID       string `json:"id"`
	CallID   string `json:"call_id"`
	RoomName string `json:"room_name"`
	FromURI  string `json:"from_uri"`
	ToURI    string `json:"to_uri"`
	State    string `json:"state"`
}
