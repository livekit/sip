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
	"container/list"
	"sync"
	"time"

	"github.com/frostbyte73/core"
)

const (
	defaultBufferSize = 5 // Samples required for a input before we start mixing

	mixerTickDuration = time.Millisecond * 20 // How ofter we generate mixes
)

type Input struct {
	mu               sync.Mutex
	samples          list.List // linked list of [mixSize]byte
	ignoredLastPrune bool

	hasBuffered bool
	bufferSize  int
	mixSize     int
}

type Mixer struct {
	mu sync.Mutex

	onSample func([]byte)
	inputs   map[*Input]struct{}

	ticker  *time.Ticker
	mixSize int

	stopped core.Fuse
}

func NewMixer(onSample func([]byte), sampleRate int) *Mixer {
	m := newMixer(onSample, sampleRate)

	go m.start()

	return m
}

func newMixer(onSample func([]byte), sampleRate int) *Mixer {
	m := &Mixer{
		onSample: onSample,
		ticker:   time.NewTicker(mixerTickDuration),
		mixSize:  int(time.Duration(sampleRate) * mixerTickDuration / time.Second),
		stopped:  core.NewFuse(),
		inputs:   make(map[*Input]struct{}),
	}

	return m
}

func (m *Mixer) doMix() {
	mixed := make([]int32, m.mixSize)

	for input, _ := range m.inputs {
		input.mu.Lock()

		if !input.hasBuffered || input.samples.Len() == 0 {
			input.mu.Unlock()
			continue
		}

		for j := 0; j < m.mixSize; j++ {
			// Add the samples. This can potentially lead to overfow, but is unlikely and dividing by the source
			// count would cause the volume to drop ever time somebody joins

			samples := input.samples.Front().Value.([]int16)
			mixed[j] += int32(samples[j])
		}

		samplesToPrune := 1

		if input.samples.Len() < input.bufferSize && input.samples.Len()%2 == 1 && !input.ignoredLastPrune {
			samplesToPrune = 0
		} else if input.samples.Len() > input.bufferSize && input.bufferSize != 0 {
			samplesToPrune = input.samples.Len() / input.bufferSize
		}

		for i := 0; i < samplesToPrune; i++ {
			input.samples.Remove(input.samples.Front())
		}
		input.ignoredLastPrune = samplesToPrune == 0

		input.mu.Unlock()
	}

	out := make([]byte, 2*m.mixSize)
	for i, sample := range mixed {
		if sample > 0x7FFF {
			sample = 0x7FFF
		}
		if sample < -0x7FFF {
			sample = -0x7FFF
		}

		// Encoder expects little endian data (???)
		out[2*i] = byte(sample & 0xFF)
		out[2*i+1] = byte(sample >> 8)
	}

	m.onSample(out)
}

func (m *Mixer) start() {
loop:
	for {
		select {
		case <-m.ticker.C:
			m.mu.Lock()
			m.doMix()
			m.mu.Unlock()
		case <-m.stopped.Watch():
			break loop
		}
	}

	m.ticker.Stop()
}

func (m *Mixer) Stop() {
	m.stopped.Break()
}

func (m *Mixer) AddInput() *Input {
	m.mu.Lock()
	defer m.mu.Unlock()

	i := &Input{
		bufferSize: defaultBufferSize,
		mixSize:    m.mixSize,
	}
	m.inputs[i] = struct{}{}

	return i
}

func (m *Mixer) RemoveInput(i *Input) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.inputs, i)
}

func (i *Input) Push(sample []int16) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Zero pad the sample if it's shorter than mixSize
	copiedSample := make([]int16, i.mixSize)
	copy(copiedSample, sample)

	i.samples.PushBack(copiedSample)

	if i.samples.Len() >= i.bufferSize {
		i.hasBuffered = true
	}
}
