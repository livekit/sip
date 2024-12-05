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

package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

// PlayAudio into a given writer. It assumes that frames are already at the writer's sample rate.
func PlayAudio[T any](ctx context.Context, w Writer[T], sampleDur time.Duration, frames []T) error {
	if len(frames) == 0 {
		return nil
	}
	tick := time.NewTicker(sampleDur)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		samples := frames[0]
		frames = frames[1:]
		if err := w.WriteSample(samples); err != nil {
			return err
		}
		if len(frames) == 0 {
			return nil
		}
	}
}

func NewPCM16FrameWriter(buf *[]PCM16Sample, sampleRate int) PCM16Writer {
	return &frameWriterPCM16{
		buf:        buf,
		sampleRate: sampleRate,
	}
}

type frameWriterPCM16 struct {
	buf        *[]PCM16Sample
	sampleRate int
}

func (b *frameWriterPCM16) String() string {
	return fmt.Sprintf("Frames(%d)", b.sampleRate)
}

func (b *frameWriterPCM16) SampleRate() int {
	return b.sampleRate
}

func (b *frameWriterPCM16) Close() error {
	return nil
}

func (b *frameWriterPCM16) WriteSample(data PCM16Sample) error {
	*b.buf = append(*b.buf, slices.Clone(data))
	return nil
}

func NewPCM16BufferWriter(buf *PCM16Sample, sampleRate int) PCM16Writer {
	if buf == nil {
		panic("buffer must be set")
	}
	return &bufferWriterPCM16{
		buf:        buf,
		sampleRate: sampleRate,
	}
}

type bufferWriterPCM16 struct {
	mu         sync.Mutex
	buf        *PCM16Sample
	sampleRate int
}

func (b *bufferWriterPCM16) String() string {
	return fmt.Sprintf("Buffer(%d)", b.sampleRate)
}

func (b *bufferWriterPCM16) SampleRate() int {
	return b.sampleRate
}

func (b *bufferWriterPCM16) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = nil
	return nil
}

func (b *bufferWriterPCM16) WriteSample(data PCM16Sample) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf != nil {
		*b.buf = append(*b.buf, data...)
	}
	return nil
}

func NewPCM16BufferReader(buf PCM16Sample) Reader[PCM16Sample] {
	return &bufferReaderPCM16{
		buf: buf,
	}
}

type bufferReaderPCM16 struct {
	buf        PCM16Sample
	sampleRate int
}

func (b *bufferReaderPCM16) ReadSample(data PCM16Sample) (int, error) {
	n := copy(data, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

var _ Frame = PCM16Sample{}

type PCM16Sample []int16

func (s PCM16Sample) Size() int {
	return len(s) * 2
}

func (s PCM16Sample) CopyTo(dst []byte) (int, error) {
	if len(dst) < len(s)*2 {
		return 0, io.ErrShortBuffer
	}
	for i, v := range s {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(v))
	}
	return len(s) * 2, nil
}

func (s PCM16Sample) Clear() {
	for i := range s {
		s[i] = 0
	}
}

func (s *PCM16Sample) WriteSample(data PCM16Sample) error {
	*s = append(*s, data...)
	return nil
}

type PCM16Writer = WriteCloser[PCM16Sample]

type MediaSampleWriter interface {
	WriteSample(sample media.Sample) error
}

func FromSampleWriter[T ~[]byte](w MediaSampleWriter, sampleRate int, sampleDur time.Duration) WriteCloser[T] {
	return &sampleWriter[T]{
		w:          w,
		sampleRate: sampleRate,
		sampleDur:  sampleDur,
	}
}

type sampleWriter[T ~[]byte] struct {
	w          MediaSampleWriter
	sampleRate int
	sampleDur  time.Duration
}

func (w *sampleWriter[T]) String() string {
	return fmt.Sprintf("LKSamples(%d)", w.sampleRate)
}

func (w *sampleWriter[T]) SampleRate() int {
	return w.sampleRate
}

func (w *sampleWriter[T]) Close() error {
	return nil
}

func (w *sampleWriter[T]) WriteSample(in T) error {
	data := make([]byte, len(in))
	copy(data, in)
	return w.w.WriteSample(media.Sample{Data: data, Duration: w.sampleDur})
}
