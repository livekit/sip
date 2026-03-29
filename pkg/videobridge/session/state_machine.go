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
	"sync/atomic"
	"time"
)

// State represents the lifecycle state of a session.
// Transitions are strictly enforced via CompareAndSwap.
//
//	              ┌─────────────────────────────────────────┐
//	              │                                         ▼
//	INIT → READY → STREAMING ⇄ DEGRADED → CLOSING → CLOSED
//	  │                │           │          ▲
//	  └────────────────┴───────────┴──────────┘
//	         (any non-terminal → CLOSING)
type State int32

const (
	StateInit      State = 0 // created, no components wired
	StateReady     State = 1 // components wired, SDP negotiated, waiting for media
	StateStreaming State = 2 // first media packet received, fully operational
	StateDegraded  State = 3 // quality reduced (audio-only, low bitrate) but still alive
	StateClosing   State = 4 // teardown in progress, no new media accepted
	StateClosed    State = 5 // all resources released, terminal
)

func (s State) String() string {
	switch s {
	case StateInit:
		return "INIT"
	case StateReady:
		return "READY"
	case StateStreaming:
		return "STREAMING"
	case StateDegraded:
		return "DEGRADED"
	case StateClosing:
		return "CLOSING"
	case StateClosed:
		return "CLOSED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(s))
	}
}

// validTransitions defines every allowed state transition.
// Any transition not in this map is rejected.
var validTransitions = map[State][]State{
	StateInit:      {StateReady, StateClosed},      // setup or early abort
	StateReady:     {StateStreaming, StateClosing}, // first media or timeout
	StateStreaming: {StateDegraded, StateClosing},  // quality drop or teardown
	StateDegraded:  {StateStreaming, StateClosing}, // recovery or teardown
	StateClosing:   {StateClosed},                  // final cleanup
}

// TransitionLog records a single state transition for debugging.
type TransitionLog struct {
	From State     `json:"from"`
	To   State     `json:"to"`
	At   time.Time `json:"at"`
}

// StateMachine provides atomic state management for a session.
// All methods are safe for concurrent use. Every transition is logged.
type StateMachine struct {
	state   atomic.Int32
	onEvent func(from, to State) // optional callback, called after every successful transition

	// Transition history (last N for debugging)
	history    [8]TransitionLog
	historyIdx atomic.Int32
}

// NewStateMachine creates a state machine in INIT state.
func NewStateMachine() *StateMachine {
	sm := &StateMachine{}
	sm.state.Store(int32(StateInit))
	return sm
}

// SetOnTransition sets a callback invoked after every successful transition.
// Typically used for logging. Must be set before Start.
func (sm *StateMachine) SetOnTransition(fn func(from, to State)) {
	sm.onEvent = fn
}

// Current returns the current state (lock-free).
func (sm *StateMachine) Current() State {
	return State(sm.state.Load())
}

// IsActive returns true if the session should process media.
func (sm *StateMachine) IsActive() bool {
	s := sm.Current()
	return s == StateReady || s == StateStreaming || s == StateDegraded
}

// IsStreaming returns true if fully operational media flow.
func (sm *StateMachine) IsStreaming() bool {
	return sm.Current() == StateStreaming
}

// IsDegraded returns true if the session is in degraded mode.
func (sm *StateMachine) IsDegraded() bool {
	return sm.Current() == StateDegraded
}

// IsTerminal returns true if the state is CLOSING or CLOSED.
func (sm *StateMachine) IsTerminal() bool {
	s := sm.Current()
	return s == StateClosing || s == StateClosed
}

// Transition atomically moves from `from` to `to`.
// Returns an error if the transition is not allowed or lost a CAS race.
func (sm *StateMachine) Transition(from, to State) error {
	if !isValidTransition(from, to) {
		return fmt.Errorf("invalid state transition: %s → %s", from, to)
	}
	if !sm.state.CompareAndSwap(int32(from), int32(to)) {
		actual := State(sm.state.Load())
		return fmt.Errorf("state transition race: expected %s, actual %s, target %s", from, actual, to)
	}
	sm.recordTransition(from, to)
	return nil
}

// MarkStreaming transitions READY → STREAMING on first media packet.
// Safe to call repeatedly — only the first call succeeds.
func (sm *StateMachine) MarkStreaming() bool {
	if sm.state.CompareAndSwap(int32(StateReady), int32(StateStreaming)) {
		sm.recordTransition(StateReady, StateStreaming)
		return true
	}
	return false
}

// MarkDegraded transitions STREAMING → DEGRADED when quality drops.
func (sm *StateMachine) MarkDegraded() bool {
	if sm.state.CompareAndSwap(int32(StateStreaming), int32(StateDegraded)) {
		sm.recordTransition(StateStreaming, StateDegraded)
		return true
	}
	return false
}

// MarkRecovered transitions DEGRADED → STREAMING when quality recovers.
func (sm *StateMachine) MarkRecovered() bool {
	if sm.state.CompareAndSwap(int32(StateDegraded), int32(StateStreaming)) {
		sm.recordTransition(StateDegraded, StateStreaming)
		return true
	}
	return false
}

// ForceClosing transitions to CLOSING from any non-terminal state.
// Used during teardown when strict transitions aren't practical.
func (sm *StateMachine) ForceClosing() {
	for {
		cur := sm.Current()
		if cur == StateClosed || cur == StateClosing {
			return
		}
		if sm.state.CompareAndSwap(int32(cur), int32(StateClosing)) {
			sm.recordTransition(cur, StateClosing)
			return
		}
	}
}

// ForceClosed sets the state to CLOSED unconditionally.
func (sm *StateMachine) ForceClosed() {
	prev := State(sm.state.Swap(int32(StateClosed)))
	if prev != StateClosed {
		sm.recordTransition(prev, StateClosed)
	}
}

// History returns the recent transition log (up to 8 entries).
func (sm *StateMachine) History() []TransitionLog {
	idx := int(sm.historyIdx.Load())
	n := idx
	if n > len(sm.history) {
		n = len(sm.history)
	}
	result := make([]TransitionLog, n)
	for i := 0; i < n; i++ {
		result[i] = sm.history[(idx-n+i)%len(sm.history)]
	}
	return result
}

func (sm *StateMachine) recordTransition(from, to State) {
	entry := TransitionLog{From: from, To: to, At: time.Now()}
	idx := sm.historyIdx.Add(1) - 1
	sm.history[idx%int32(len(sm.history))] = entry
	if sm.onEvent != nil {
		sm.onEvent(from, to)
	}
}

func isValidTransition(from, to State) bool {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
