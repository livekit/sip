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
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/frostbyte73/core"

	"github.com/livekit/sip/pkg/internal/ringbuf"
	"github.com/livekit/sip/pkg/media"
)

const (
	// inputBufferFrames sets max number of frames that each mixer input will allow.
	// Sending more frames to the input will cause old one to be dropped.
	inputBufferFrames = 5

	// inputBufferMin is the minimal number of buffered frames required to start mixing.
	// It affects inputs initially, or after they start to starve.
	inputBufferMin = inputBufferFrames/2 + 1
)

type Input struct {
	m          *Mixer
	sampleRate int
	mu         sync.Mutex
	buf        *ringbuf.Buffer[int16]
	buffering  bool
}

type Mixer struct {
	out        media.Writer[media.PCM16Sample]
	sampleRate int

	mu     sync.Mutex
	inputs []*Input

	tickerDur time.Duration
	ticker    *time.Ticker
	mixBuf    []int32           // mix result buffer
	mixTmp    media.PCM16Sample // temp buffer for reading input buffers

	lastMixEndTs time.Time
	stopped      core.Fuse
	mixCnt       uint
	dump         *os.File
}

func NewMixer(out media.Writer[media.PCM16Sample], bufferDur time.Duration) *Mixer {
	mixSize := int(time.Duration(out.SampleRate()) * bufferDur / time.Second)
	m := newMixer(out, mixSize)
	m.tickerDur = bufferDur
	m.ticker = time.NewTicker(bufferDur)

	go m.start()

	return m
}

func newMixer(out media.Writer[media.PCM16Sample], mixSize int) *Mixer {
	dump, _ := os.Create("mix.raw")

	return &Mixer{
		out:        out,
		sampleRate: out.SampleRate(),
		mixBuf:     make([]int32, mixSize),
		mixTmp:     make(media.PCM16Sample, mixSize),
		dump:       dump,
	}
}

func (m *Mixer) mixInputs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Keep at least half of the samples buffered.
	bufMin := inputBufferMin * len(m.mixBuf)
	for _, inp := range m.inputs {
		n, _ := inp.readSample(bufMin, m.mixTmp[:len(m.mixBuf)])
		if n == 0 {
			continue
		}
		// TODO do not shorten

		if n < len(m.mixBuf) {
			fmt.Println("SHORT READ", n, len(m.mixBuf))
		}
		m.mixTmp = m.mixTmp[:n]
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

func (m *Mixer) mixOnce() {
	m.mixCnt++
	m.reset()
	now := time.Now()
	m.mixInputs()

	if time.Now().Sub(now) > 5*time.Millisecond {
		fmt.Println("SOURCE WAIT")
	}

	// TODO: if we can guarantee that WriteSample won't store the sample, we can avoid allocation
	out := make(media.PCM16Sample, len(m.mixBuf))
	for i, v := range m.mixBuf {
		if v > 0x7FFF {
			v = 0x7FFF
			fmt.Println("CLAMP +")
		}
		if v < -0x7FFF {
			v = -0x7FFF
			fmt.Println("CLAMP -")
		}
		out[i] = int16(v)
	}
	now = time.Now()
	_ = m.out.WriteSample(out)

	if time.Now().Sub(now) > 30*time.Millisecond {
		fmt.Println("DEST WAIT")
	}

}

func (m *Mixer) mixUpdate() {
	n := 0
	now := time.Now()

	if m.lastMixEndTs.IsZero() {
		m.lastMixEndTs = now
	} else {
		// In case scheduler stops us for too long, we will detect it and run mix multiple times.
		// This happens if we get scheduled by OS/K8S on a lot of CPUs, but for a very short time.
		if dt := now.Sub(m.lastMixEndTs); dt > 0 {
			n = int(dt / m.tickerDur)
			m.lastMixEndTs = m.lastMixEndTs.Add(time.Duration(n) * m.tickerDur)
		} else {
			fmt.Println("NO MIX")
		}
	}

	if n > 1 {
		fmt.Println("N", n, time.Now())
	}

	if n > inputBufferFrames {
		n = inputBufferFrames
		// reset
		m.lastMixEndTs = now
	}

	for i := 0; i < n; i++ {
		m.mixOnce()
	}
}

func (m *Mixer) start() {
	defer m.ticker.Stop()
	for {
		select {
		case <-m.ticker.C:
			m.mixUpdate()
		case <-m.stopped.Watch():
			return
		}
	}
}

func (m *Mixer) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped.Break()
}

func (m *Mixer) NewInput() *Input {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped.IsBroken() {
		return nil
	}

	inp := &Input{
		m:          m,
		sampleRate: m.sampleRate,
		buf:        ringbuf.New[int16](len(m.mixBuf) * inputBufferFrames),
		buffering:  true, // buffer some data initially
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

func (m *Mixer) String() string {
	return fmt.Sprintf("Mixer(%d) -> %s", len(m.inputs), m.out.String())
}

func (m *Mixer) SampleRate() int {
	return m.sampleRate
}

func (i *Input) readSample(bufMin int, out media.PCM16Sample) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.buffering {
		if i.buf.Len() < bufMin {
			fmt.Println("KEEP BUFFERING")
			return 0, nil // keep buffering
		}
		// buffered enough data - start playing as usual
		i.buffering = false
	}
	n, err := i.buf.Read(out)
	if n == 0 {
		fmt.Println("BUFFERING")
		i.buffering = true // starving; pause the input and start buffering again
	}
	return n, err
}

func (i *Input) String() string {
	return fmt.Sprintf("MixInput(%d) -> %s", i.sampleRate, i.m.String())
}

func (i *Input) SampleRate() int {
	return i.sampleRate
}

func (i *Input) Close() error {
	if i == nil {
		return nil
	}
	i.m.RemoveInput(i)
	return nil
}

func (i *Input) WriteSample(sample media.PCM16Sample) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	binary.Write(i.m.dump, binary.LittleEndian, []int16(sample))
	_, err := i.buf.Write(sample)
	return err
}
