// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This is a copy of io/pipe.go adapted to media.
// The main change is that the types use generics, plus their methods are renamed for our needs:
// ReadSample/WriteSample instead of Read/Write, so that they match media Reader/Writer interfaces.

package media

import (
	"io"
	"sync"
)

var (
	_ ReadCloser[PCM16Sample]  = (*PipeReader[PCM16Sample, int16])(nil)
	_ WriteCloser[PCM16Sample] = (*PipeWriter[PCM16Sample, int16])(nil)
)

// onceError is an object that will only store an error once.
type onceError struct {
	sync.Mutex // guards following
	err        error
}

func (a *onceError) Store(err error) {
	a.Lock()
	defer a.Unlock()
	if a.err != nil {
		return
	}
	a.err = err
}
func (a *onceError) Load() error {
	a.Lock()
	defer a.Unlock()
	return a.err
}

// A pipe is the shared pipe structure underlying PipeReader and PipeWriter.
type pipe[T ~[]E, E comparable] struct {
	wrMu sync.Mutex // Serializes Write operations
	wrCh chan T
	rdCh chan int

	once sync.Once // Protects closing done
	done chan struct{}
	rerr onceError
	werr onceError
}

func (p *pipe[T, E]) read(b T) (n int, err error) {
	select {
	case <-p.done:
		return 0, p.readCloseError()
	default:
	}

	select {
	case bw := <-p.wrCh:
		nr := copy(b, bw)
		p.rdCh <- nr
		return nr, nil
	case <-p.done:
		return 0, p.readCloseError()
	}
}

func (p *pipe[T, E]) closeRead(err error) error {
	if err == nil {
		err = io.ErrClosedPipe
	}
	p.rerr.Store(err)
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *pipe[T, E]) write(b T) (n int, err error) {
	select {
	case <-p.done:
		return 0, p.writeCloseError()
	default:
		p.wrMu.Lock()
		defer p.wrMu.Unlock()
	}

	for once := true; once || len(b) > 0; once = false {
		select {
		case p.wrCh <- b:
			nw := <-p.rdCh
			b = b[nw:]
			n += nw
		case <-p.done:
			return n, p.writeCloseError()
		}
	}
	return n, nil
}

func (p *pipe[T, E]) closeWrite(err error) error {
	if err == nil {
		err = io.EOF
	}
	p.werr.Store(err)
	p.once.Do(func() { close(p.done) })
	return nil
}

// readCloseError is considered internal to the pipe type.
func (p *pipe[T, E]) readCloseError() error {
	rerr := p.rerr.Load()
	if werr := p.werr.Load(); rerr == nil && werr != nil {
		return werr
	}
	return io.ErrClosedPipe
}

// writeCloseError is considered internal to the pipe type.
func (p *pipe[T, E]) writeCloseError() error {
	werr := p.werr.Load()
	if rerr := p.rerr.Load(); werr == nil && rerr != nil {
		return rerr
	}
	return io.ErrClosedPipe
}

// A PipeReader is the read half of a pipe.
type PipeReader[T ~[]E, E comparable] struct{ pipe[T, E] }

// ReadSample implements the Reader interface.
func (r *PipeReader[T, E]) ReadSample(data T) (n int, err error) {
	return r.pipe.read(data)
}

// Close closes the reader; subsequent writes to the
// write half of the pipe will return the error [io.ErrClosedPipe].
func (r *PipeReader[T, E]) Close() error {
	return r.CloseWithError(nil)
}

// CloseWithError closes the reader; subsequent writes
// to the write half of the pipe will return the error err.
//
// CloseWithError never overwrites the previous error if it exists
// and always returns nil.
func (r *PipeReader[T, E]) CloseWithError(err error) error {
	return r.pipe.closeRead(err)
}

// A PipeWriter is the write half of a pipe.
type PipeWriter[T ~[]E, E comparable] struct{ r PipeReader[T, E] }

// WriteSample implements the Writer interface.
func (w *PipeWriter[T, E]) WriteSample(data T) (err error) {
	_, err = w.r.pipe.write(data)
	return err
}

// Close closes the writer; subsequent reads from the
// read half of the pipe will return no bytes and EOF.
func (w *PipeWriter[T, E]) Close() error {
	return w.CloseWithError(nil)
}

// CloseWithError closes the writer; subsequent reads from the
// read half of the pipe will return no bytes and the error err,
// or EOF if err is nil.
//
// CloseWithError never overwrites the previous error if it exists
// and always returns nil.
func (w *PipeWriter[T, E]) CloseWithError(err error) error {
	return w.r.pipe.closeWrite(err)
}

// Pipe creates a synchronous in-memory pipe.
// It can be used to connect code expecting a [Reader]
// with code expecting a [Writer].
func Pipe[T ~[]E, E comparable]() (*PipeReader[T, E], *PipeWriter[T, E]) {
	pw := &PipeWriter[T, E]{r: PipeReader[T, E]{pipe: pipe[T, E]{
		wrCh: make(chan T),
		rdCh: make(chan int),
		done: make(chan struct{}),
	}}}
	return &pw.r, pw
}
