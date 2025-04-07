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
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// DefFrameDur is a default duration of an audio frame.
	DefFrameDur = 20 * time.Millisecond
	// DefFramesPerSec is a default number of audio frames per second.
	DefFramesPerSec = int(time.Second / DefFrameDur)
)

type Frame interface {
	// Size of the frame in bytes.
	Size() int
	// CopyTo copies the frame content to the destination bytes slice.
	// It returns io.ErrShortBuffer is the buffer size is less than frame's Size.
	CopyTo(dst []byte) (int, error)
}

type Reader[T any] interface {
	ReadSample(buf T) (int, error)
}

type ReadCloser[T any] interface {
	Reader[T]
	Close() error
}

type Writer[T any] interface {
	String() string
	SampleRate() int
	WriteSample(sample T) error
}

type WriteCloser[T any] interface {
	Writer[T]
	Close() error
}

type writeCloser[T any] struct {
	Writer[T]
}

func (*writeCloser[T]) Close() error {
	return nil
}

func NopCloser[T any](w Writer[T]) WriteCloser[T] {
	return &writeCloser[T]{w}
}

// NewSwitchWriter 创建一个SwitchWriter
// 参数:
// - sampleRate: 采样率
// 返回:
// - *SwitchWriter: 返回一个SwitchWriter
func NewSwitchWriter(sampleRate int) *SwitchWriter {
	if sampleRate <= 0 {
		panic("invalid sample rate")
	}
	return &SwitchWriter{
		sampleRate: sampleRate,
	}
}

// SwitchWriter 是一个可以切换底层写入器的结构体
// 它包含一个采样率和一个指向PCM16Writer的指针
type SwitchWriter struct {
	sampleRate int
	ptr        atomic.Pointer[PCM16Writer]
}

func (s *SwitchWriter) Get() PCM16Writer {
	ptr := s.ptr.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// Swap sets an underlying writer and returns the old one.
// Caller is responsible for closing the old writer.
// 交换底层写入器并返回旧的写入器。
// 调用者负责关闭旧的写入器。
func (s *SwitchWriter) Swap(w PCM16Writer) PCM16Writer {
	var old *PCM16Writer
	if w == nil {
		old = s.ptr.Swap(nil)
	} else {
		if w.SampleRate() != s.sampleRate {
			w = ResampleWriter(w, s.sampleRate)
		}
		old = s.ptr.Swap(&w)
	}
	if old == nil {
		return nil
	}
	return *old
}

func (s *SwitchWriter) String() string {
	w := s.Get()
	return fmt.Sprintf("Switch(%d) -> %v", s.sampleRate, w)
}

func (s *SwitchWriter) SampleRate() int {
	if s.sampleRate == 0 {
		panic("switch writer not initialized")
	}
	return s.sampleRate
}

func (s *SwitchWriter) Close() error {
	ptr := s.ptr.Swap(nil)
	if ptr == nil {
		return nil
	}
	return (*ptr).Close()
}

func (s *SwitchWriter) WriteSample(sample PCM16Sample) error {
	w := s.Get()
	if w == nil {
		return nil
	}
	return w.WriteSample(sample)
}

type MultiWriter[T any] []WriteCloser[T]

func (s MultiWriter[T]) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "MultiWriter(%d,%d)", len(s), s.SampleRate())
	for i, w := range s {
		fmt.Fprintf(&buf, "; $%d-> %s", i+1, w.String())
	}
	return buf.String()
}

func (s MultiWriter[T]) SampleRate() int {
	if len(s) == 0 {
		return 0
	}
	return s[0].SampleRate()
}

func (s MultiWriter[T]) WriteSample(sample T) error {
	var last error
	for _, w := range s {
		if err := w.WriteSample(sample); err != nil {
			last = err
		}
	}
	return last
}

func (s MultiWriter[T]) Close() error {
	var last error
	for _, w := range s {
		if err := w.Close(); err != nil {
			last = err
		}
	}
	return last
}

func NewFileWriter[T Frame](w io.WriteCloser, sampleRate int) WriteCloser[T] {
	return &fileWriter[T]{
		w:          w,
		bw:         bufio.NewWriter(w),
		sampleRate: sampleRate,
	}
}

type fileWriter[T Frame] struct {
	w          io.WriteCloser
	bw         *bufio.Writer
	sampleRate int
	buf        []byte
}

func (w *fileWriter[T]) String() string {
	return fmt.Sprintf("RawFile(%d)", w.sampleRate)
}

func (w *fileWriter[T]) SampleRate() int {
	return w.sampleRate
}

func (w *fileWriter[T]) WriteSample(sample T) error {
	if sz := sample.Size(); cap(w.buf) < sz {
		w.buf = make([]byte, sz)
	} else {
		w.buf = w.buf[:sz]
	}
	n, err := sample.CopyTo(w.buf)
	if err != nil {
		return err
	}
	_, err = w.bw.Write(w.buf[:n])
	return err
}

func (w *fileWriter[T]) Close() error {
	if err := w.bw.Flush(); err != nil {
		_ = w.w.Close()
		return err
	}
	if err := w.w.Close(); err != nil {
		return err
	}
	return nil
}
