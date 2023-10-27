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
	defaultMixSize = 160

	mixerTickDuration = time.Millisecond * 20 // How ofter we gener

	bufferSize = 5 // Samples required for a input before we start mixing
)

type Mixer struct {
	mu sync.Mutex

	onSample func([]byte)

	samples [][]int16

	ticker  *time.Ticker
	mixSize int

	inputCount uint64
	stopped    atomic.Bool
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
	for i := range m.samples {
		for j := range m.samples[i] {
			mixed[j] += m.samples[i][j]
		}
	}
	m.samples = [][]int16{}

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

		if atomic.LoadUint64(&m.inputCount) == 0 {
			m.onSample(make([]byte, m.mixSize*2))
		}
	}

	m.ticker.Stop()
}

func (m *Mixer) Stop() {
	m.stopped.Store(true)
}

func (m *Mixer) AddInput() {
	atomic.AddUint64(&m.inputCount, 1)
}

func (m *Mixer) RemoveInput() {
	atomic.AddUint64(&m.inputCount, ^uint64(0))
}

func (m *Mixer) Push(sample []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := append([]int16{}, sample...)
	inputCount := int16(atomic.LoadUint64(&m.inputCount))

	for i := range copied {
		copied[i] /= inputCount
	}
	m.samples = append(m.samples, copied)

	if len(m.samples) == int(inputCount) {
		m.doMix()
	}
}
