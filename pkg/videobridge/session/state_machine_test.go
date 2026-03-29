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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateMachine_InitialState(t *testing.T) {
	sm := NewStateMachine()
	assert.Equal(t, StateInit, sm.Current())
	assert.False(t, sm.IsActive())
	assert.False(t, sm.IsStreaming())
	assert.False(t, sm.IsDegraded())
	assert.False(t, sm.IsTerminal())
}

func TestStateMachine_FullHappyPath(t *testing.T) {
	sm := NewStateMachine()

	// INIT → READY
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.Equal(t, StateReady, sm.Current())
	assert.True(t, sm.IsActive())

	// READY → STREAMING (first media)
	assert.True(t, sm.MarkStreaming())
	assert.Equal(t, StateStreaming, sm.Current())
	assert.True(t, sm.IsActive())
	assert.True(t, sm.IsStreaming())

	// Double MarkStreaming is no-op
	assert.False(t, sm.MarkStreaming())

	// STREAMING → DEGRADED (quality drop)
	assert.True(t, sm.MarkDegraded())
	assert.Equal(t, StateDegraded, sm.Current())
	assert.True(t, sm.IsActive())
	assert.True(t, sm.IsDegraded())

	// DEGRADED → STREAMING (recovered)
	assert.True(t, sm.MarkRecovered())
	assert.Equal(t, StateStreaming, sm.Current())

	// STREAMING → CLOSING
	require.NoError(t, sm.Transition(StateStreaming, StateClosing))
	assert.Equal(t, StateClosing, sm.Current())
	assert.False(t, sm.IsActive())
	assert.True(t, sm.IsTerminal())

	// CLOSING → CLOSED
	require.NoError(t, sm.Transition(StateClosing, StateClosed))
	assert.Equal(t, StateClosed, sm.Current())
}

func TestStateMachine_InvalidTransition_Rejected(t *testing.T) {
	sm := NewStateMachine()

	// INIT → STREAMING (skip READY)
	err := sm.Transition(StateInit, StateStreaming)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid state transition")
	assert.Equal(t, StateInit, sm.Current())

	// INIT → CLOSING (not allowed directly)
	err = sm.Transition(StateInit, StateClosing)
	assert.Error(t, err)
	assert.Equal(t, StateInit, sm.Current())
}

func TestStateMachine_WrongCurrentState_Rejected(t *testing.T) {
	sm := NewStateMachine()

	// Try READY → STREAMING but we're in INIT
	err := sm.Transition(StateReady, StateStreaming)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "race")
	assert.Equal(t, StateInit, sm.Current())
}

func TestStateMachine_BackwardTransition_Rejected(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())

	// STREAMING → INIT (backward)
	err := sm.Transition(StateStreaming, StateInit)
	assert.Error(t, err)
	assert.Equal(t, StateStreaming, sm.Current())

	// STREAMING → READY (backward)
	err = sm.Transition(StateStreaming, StateReady)
	assert.Error(t, err)
	assert.Equal(t, StateStreaming, sm.Current())
}

func TestStateMachine_DoubleTransition_Rejected(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))

	err := sm.Transition(StateInit, StateReady)
	assert.Error(t, err)
}

func TestStateMachine_EarlyAbort(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateClosed))
	assert.Equal(t, StateClosed, sm.Current())
}

func TestStateMachine_ReadyTimeout(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	// Media never arrived → CLOSING
	require.NoError(t, sm.Transition(StateReady, StateClosing))
	assert.Equal(t, StateClosing, sm.Current())
}

func TestStateMachine_DegradedToClosing(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())
	assert.True(t, sm.MarkDegraded())
	require.NoError(t, sm.Transition(StateDegraded, StateClosing))
	assert.Equal(t, StateClosing, sm.Current())
}

func TestStateMachine_MarkDegraded_OnlyFromStreaming(t *testing.T) {
	sm := NewStateMachine()
	assert.False(t, sm.MarkDegraded()) // INIT

	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.False(t, sm.MarkDegraded()) // READY

	assert.True(t, sm.MarkStreaming())
	assert.True(t, sm.MarkDegraded())  // STREAMING → DEGRADED ✓
	assert.False(t, sm.MarkDegraded()) // already DEGRADED
}

func TestStateMachine_MarkRecovered_OnlyFromDegraded(t *testing.T) {
	sm := NewStateMachine()
	assert.False(t, sm.MarkRecovered()) // INIT

	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())
	assert.False(t, sm.MarkRecovered()) // STREAMING (not degraded)

	assert.True(t, sm.MarkDegraded())
	assert.True(t, sm.MarkRecovered()) // DEGRADED → STREAMING ✓
}

func TestStateMachine_ForceClosing(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())
	sm.ForceClosing()
	assert.Equal(t, StateClosing, sm.Current())
}

func TestStateMachine_ForceClosing_FromInit(t *testing.T) {
	sm := NewStateMachine()
	sm.ForceClosing()
	assert.Equal(t, StateClosing, sm.Current())
}

func TestStateMachine_ForceClosing_AlreadyClosed(t *testing.T) {
	sm := NewStateMachine()
	sm.ForceClosed()
	sm.ForceClosing()
	assert.Equal(t, StateClosed, sm.Current())
}

func TestStateMachine_ForceClosed(t *testing.T) {
	sm := NewStateMachine()
	sm.ForceClosed()
	assert.Equal(t, StateClosed, sm.Current())
}

func TestStateMachine_ConcurrentTransitions(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	wins := make(chan bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			err := sm.Transition(StateStreaming, StateClosing)
			wins <- (err == nil)
		}()
	}

	wg.Wait()
	close(wins)

	winCount := 0
	for won := range wins {
		if won {
			winCount++
		}
	}
	assert.Equal(t, 1, winCount, "exactly one goroutine should win the CAS race")
	assert.Equal(t, StateClosing, sm.Current())
}

func TestStateMachine_ConcurrentIsActive(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())

	const readers = 50
	var wg sync.WaitGroup
	wg.Add(readers + 1)

	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			_ = sm.IsActive()
		}()
	}

	go func() {
		defer wg.Done()
		sm.ForceClosing()
	}()

	wg.Wait()
	assert.True(t, sm.IsTerminal())
}

func TestStateMachine_OnTransitionCallback(t *testing.T) {
	sm := NewStateMachine()
	var logged []string
	sm.SetOnTransition(func(from, to State) {
		logged = append(logged, from.String()+"→"+to.String())
	})

	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())
	assert.True(t, sm.MarkDegraded())
	assert.True(t, sm.MarkRecovered())
	sm.ForceClosing()

	assert.Equal(t, []string{
		"INIT→READY",
		"READY→STREAMING",
		"STREAMING→DEGRADED",
		"DEGRADED→STREAMING",
		"STREAMING→CLOSING",
	}, logged)
}

func TestStateMachine_History(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())
	sm.ForceClosing()

	history := sm.History()
	require.Len(t, history, 3)
	assert.Equal(t, StateInit, history[0].From)
	assert.Equal(t, StateReady, history[0].To)
	assert.Equal(t, StateReady, history[1].From)
	assert.Equal(t, StateStreaming, history[1].To)
	assert.Equal(t, StateStreaming, history[2].From)
	assert.Equal(t, StateClosing, history[2].To)
}

func TestStateMachine_MarkStreaming_OnlyFromReady(t *testing.T) {
	sm := NewStateMachine()
	assert.False(t, sm.MarkStreaming()) // INIT

	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())  // READY → STREAMING ✓
	assert.False(t, sm.MarkStreaming()) // already STREAMING
}

func TestStateMachine_ConcurrentDegradation(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StateInit, StateReady))
	assert.True(t, sm.MarkStreaming())

	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(50)
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			if sm.MarkDegraded() {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
	assert.Equal(t, StateDegraded, sm.Current())
}

func TestState_String(t *testing.T) {
	assert.Equal(t, "INIT", StateInit.String())
	assert.Equal(t, "READY", StateReady.String())
	assert.Equal(t, "STREAMING", StateStreaming.String())
	assert.Equal(t, "DEGRADED", StateDegraded.String())
	assert.Equal(t, "CLOSING", StateClosing.String())
	assert.Equal(t, "CLOSED", StateClosed.String())
	assert.Equal(t, "UNKNOWN(99)", State(99).String())
}
