// Copyright 2023 LiveKit, Inc.
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

package mixer

import (
	"sync"
	"time"

	"github.com/frostbyte73/core"

	"github.com/livekit/sip/pkg/internal/ringbuf"
	"github.com/livekit/sip/pkg/media"
)

// inputBufferFrames sets max number of frames that each mixer input will allow.
// Sending more frames to the input will cause old one to be dropped.
const inputBufferFrames = 5

type Input struct {
	mu  sync.Mutex
	buf *ringbuf.Buffer[int16]
}

type Mixer struct {
	out media.Writer[media.PCM16Sample]

	mu     sync.Mutex
	inputs []*Input

	ticker *time.Ticker
	mixBuf []int32           // mix result buffer
	mixTmp media.PCM16Sample // temp buffer for reading input buffers

	stopped core.Fuse
}

func NewMixer(out media.Writer[media.PCM16Sample], bufferDur time.Duration, sampleRate int) *Mixer {
	mixSize := int(time.Duration(sampleRate) * bufferDur / time.Second)
	m := newMixer(out, mixSize)
	m.ticker = time.NewTicker(bufferDur)

	go m.start()

	return m
}

func newMixer(out media.Writer[media.PCM16Sample], mixSize int) *Mixer {
	return &Mixer{
		out:     out,
		mixBuf:  make([]int32, mixSize),
		mixTmp:  make(media.PCM16Sample, mixSize),
		stopped: core.NewFuse(),
	}
}

func (m *Mixer) mixInputs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inp := range m.inputs {
		n, _ := inp.readSample(m.mixTmp[:len(m.mixBuf)])
		m.mixTmp = m.mixTmp[:n]
		if n == 0 {
			continue
		}
		for j, v := range m.mixTmp {
			// Add the samples. This can potentially lead to overflow, but is unlikely and dividing by the source
			// count would cause the volume to drop every time somebody joins
			m.mixBuf[j] += int32(v)
		}
	}
}

func (m *Mixer) reset() {
	for i := range m.mixBuf {
		m.mixBuf[i] = 0
	}
}

func (m *Mixer) doMix() {
	m.reset()
	m.mixInputs()

	out := make(media.PCM16Sample, len(m.mixBuf))
	for i, v := range m.mixBuf {
		if v > 0x7FFF {
			v = 0x7FFF
		}
		if v < -0x7FFF {
			v = -0x7FFF
		}
		out[i] = int16(v)
	}
	_ = m.out.WriteSample(out)
}

func (m *Mixer) start() {
	defer m.ticker.Stop()
	for {
		select {
		case <-m.ticker.C:
			m.doMix()
		case <-m.stopped.Watch():
			return
		}
	}
}

func (m *Mixer) Stop() {
	m.stopped.Break()
}

func (m *Mixer) NewInput() *Input {
	m.mu.Lock()
	defer m.mu.Unlock()

	inp := &Input{
		buf: ringbuf.New[int16](len(m.mixBuf) * inputBufferFrames),
	}
	m.inputs = append(m.inputs, inp)
	return inp
}

func (m *Mixer) RemoveInput(inp *Input) {
	if m == nil || inp == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, cur := range m.inputs {
		if cur == inp {
			m.inputs = append(m.inputs[:i], m.inputs[i+1:]...)
			break
		}
	}
}

func (i *Input) readSample(out media.PCM16Sample) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.buf.Read(out)
}

func (i *Input) WriteSample(sample media.PCM16Sample) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, err := i.buf.Write(sample)
	return err
}
