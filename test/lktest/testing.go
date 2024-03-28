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

package lktest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
)

const testing = false // do not allow import of testing package

// TB mirrors testing.TB interface without including the actual testing package.
type TB interface {
	Cleanup(func())
	Error(args ...any)
	Errorf(format string, args ...any)
	FailNow()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Log(args ...any)
	Logf(format string, args ...any)
	Skip(args ...any)
	Skipf(format string, args ...any)
}

func TestMain(run func(ctx context.Context, t TB)) {
	defer func() {
		switch r := recover().(type) {
		case fatal:
			os.Exit(1)
		default:
			panic(r)
		case nil, skip:
			// exit 0
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	t := &testingImpl{}
	defer t.doCleanup()

	run(ctx, t)

	if t.err != nil {
		slog.Error("test failed", "error", t.err)
		os.Exit(1)
	}
}

type fatal struct{}
type skip struct{}

type testingImpl struct {
	mu      sync.Mutex
	err     error
	cleanup []func()
}

func (t *testingImpl) doCleanup() {
	t.mu.Lock()
	cleanup := t.cleanup
	t.cleanup = nil
	t.mu.Unlock()
	for i := len(cleanup) - 1; i >= 0; i-- {
		cleanup[i]()
	}
}

func (t *testingImpl) Cleanup(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanup = append(t.cleanup, f)
}

func (t *testingImpl) setError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

func (t *testingImpl) Error(args ...any) {
	msg := fmt.Sprint(args...)
	t.setError(errors.New(msg))
	slog.Error(msg)
}

func (t *testingImpl) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	t.setError(errors.New(msg))
	slog.Error(msg)
}

func (t *testingImpl) FailNow() {
	panic(fatal{})
}

func (t *testingImpl) Fatal(args ...any) {
	t.Error(args...)
	t.FailNow()
}

func (t *testingImpl) Fatalf(format string, args ...any) {
	t.Errorf(format, args...)
	t.FailNow()
}

func (t *testingImpl) Log(args ...any) {
	msg := fmt.Sprint(args...)
	slog.Info(msg)
}

func (t *testingImpl) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	slog.Info(msg)
}

func (t *testingImpl) Skip(args ...any) {
	t.Log(args...)
	panic(skip{})
}

func (t *testingImpl) Skipf(format string, args ...any) {
	t.Logf(format, args...)
	panic(skip{})
}
