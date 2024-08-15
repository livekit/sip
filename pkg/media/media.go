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
	"sync/atomic"
)

type Reader[T any] interface {
	ReadSample(buf T) (int, error)
}

type ReadCloser[T any] interface {
	Reader[T]
	Close() error
}

type Writer[T any] interface {
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

func NewSwitchWriter[T any](sampleRate int) *SwitchWriter[T] {
	return &SwitchWriter[T]{
		sampleRate: sampleRate,
	}
}

type SwitchWriter[T any] struct {
	sampleRate int
	ptr        atomic.Pointer[WriteCloser[T]]
}

func (s *SwitchWriter[T]) Get() WriteCloser[T] {
	ptr := s.ptr.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// Swap sets an underlying writer and returns the old one.
// Caller is responsible for closing the old writer.
func (s *SwitchWriter[T]) Swap(w WriteCloser[T]) WriteCloser[T] {
	var old *WriteCloser[T]
	if w == nil {
		old = s.ptr.Swap(nil)
	} else {
		old = s.ptr.Swap(&w)
	}
	if old == nil {
		return nil
	}
	return *old
}

func (s *SwitchWriter[T]) SampleRate() int {
	return s.sampleRate
}

func (s *SwitchWriter[T]) Close() error {
	ptr := s.ptr.Swap(nil)
	if ptr == nil {
		return nil
	}
	return (*ptr).Close()
}

func (s *SwitchWriter[T]) WriteSample(sample T) error {
	w := s.Get()
	if w == nil {
		return nil
	}
	return w.WriteSample(sample)
}

type MultiWriter[T any] []Writer[T]

func (s MultiWriter[T]) WriteSample(sample T) error {
	var last error
	for _, w := range s {
		if err := w.WriteSample(sample); err != nil {
			last = err
		}
	}
	return last
}
