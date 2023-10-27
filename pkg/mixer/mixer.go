// Copyright 2023 LiveKit, Inc.
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

package mixer

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMixSize    = 160
	defaultBufferSize = 5 // Samples required for a input before we start mixing

	mixerTickDuration = time.Millisecond * 20 // How ofter we generate mixes
)

type Input struct {
	mu      sync.Mutex
	samples [][]int16

	hasBuffered atomic.Bool
	bufferSize  int
}

type Mixer struct {
	mu sync.Mutex

	onSample func([]byte)
	inputs   []*Input

	ticker  *time.Ticker
	mixSize int

	stopped atomic.Bool
}

func NewMixer(onSample func([]byte)) *Mixer {
	m := &Mixer{
		onSample: onSample,
		ticker:   time.NewTicker(mixerTickDuration),
		mixSize:  defaultMixSize,
	}

	go m.start()

	return m
}

func (m *Mixer) doMix() {
	mixed := make([]int16, m.mixSize)

	for _, input := range m.inputs {
		if !input.hasBuffered.Load() {
			continue
		}

		input.mu.Lock()
		for j := 0; j < m.mixSize; j++ {
			mixed[j] += input.samples[0][j] / int16(len(m.inputs))
		}
		input.mu.Unlock()
	}

	out := []byte{}
	for _, sample := range mixed {
		out = append(out, byte(sample&0xff))
		out = append(out, byte(sample>>8))
	}

	m.onSample(out)
}

func (m *Mixer) start() {
	for range m.ticker.C {
		if m.stopped.Load() {
			break
		}

		m.mu.Lock()
		m.doMix()
		m.mu.Unlock()
	}

	m.ticker.Stop()
}

func (m *Mixer) Stop() {
	m.stopped.Store(true)
}

func (m *Mixer) AddInput() *Input {
	m.mu.Lock()
	defer m.mu.Unlock()

	i := &Input{bufferSize: defaultBufferSize}
	m.inputs = append(m.inputs, i)

	return i
}

func (m *Mixer) RemoveInput(i *Input) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newInputs := []*Input{}
	for _, input := range m.inputs {
		if i != input {
			newInputs = append(newInputs, input)
		}
	}
	m.inputs = newInputs
}

func (i *Input) Push(sample []int16) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.samples = append(i.samples, append([]int16{}, sample...))

	if len(i.samples) >= i.bufferSize {
		i.hasBuffered.Store(true)
	}
}
