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
	WriteSample(sample T) error
}

type WriteCloser[T any] interface {
	Writer[T]
	Close() error
}

type WriterFunc[T any] func(in T) error

func (fnc WriterFunc[T]) WriteSample(in T) error {
	return fnc(in)
}

type SwitchWriter[T any] struct {
	ptr atomic.Pointer[Writer[T]]
}

func (s *SwitchWriter[T]) Get() Writer[T] {
	ptr := s.ptr.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

func (s *SwitchWriter[T]) Set(w Writer[T]) {
	if w == nil {
		s.ptr.Store(nil)
	} else {
		s.ptr.Store(&w)
	}
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
