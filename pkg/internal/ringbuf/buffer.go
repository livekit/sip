package ringbuf

import "io"

// New creates a new ring buffer with s specified size.
func New[T any](sz int) *Buffer[T] {
	return &Buffer[T]{
		buf: make([]T, sz),
	}
}

type Buffer[T any] struct {
	buf   []T
	write int
	read  int
	full  bool
}

// Size returns underlying size of the buffer.
func (b *Buffer[T]) Size() int {
	return len(b.buf)
}

// Len returns a number of elements currently in the buffer.
func (b *Buffer[T]) Len() int {
	if b.read == b.write {
		if b.full {
			return len(b.buf)
		}
		return 0
	}
	if b.read < b.write {
		return b.write - b.read
	}
	return b.write - b.read + len(b.buf)
}

// TryPeek tries to access first buffered element without consuming it. Function returns false if the buffer is empty.
func (b *Buffer[T]) TryPeek() (T, bool) {
	if !b.full && b.read == b.write {
		var zero T
		return zero, false
	}
	return b.buf[b.read], true
}

// Peek returns first buffered element without consuming it. Function returns zero value if the buffer is empty.
func (b *Buffer[T]) Peek() T {
	v, _ := b.TryPeek()
	return v
}

// TryPop returns the first element and consumes it. Function returns false if the buffer is empty.
func (b *Buffer[T]) TryPop() (T, bool) {
	if !b.full && b.read == b.write {
		var zero T
		return zero, false
	}
	b.full = false
	v := b.buf[b.read]
	b.read = (b.read + 1) % len(b.buf)
	return v, true
}

// Pop returns the first element and consumes it. Function returns zero value if the buffer is empty.
func (b *Buffer[T]) Pop() T {
	v, _ := b.TryPop()
	return v
}

// TryPush attempts to add an element into the end of the buffer. It returns false if the buffer is full.
func (b *Buffer[T]) TryPush(v T) bool {
	if b.full {
		return false
	}
	b.buf[b.write] = v
	b.write = (b.write + 1) % len(b.buf)
	b.full = b.write == b.read
	return true
}

// Push adds an element into the end of the buffer. It discards the oldest element if the buffer is already full.
func (b *Buffer[T]) Push(v T) {
	if b.full {
		b.read = (b.read + 1) % len(b.buf) // drop old
	}
	b.buf[b.write] = v
	b.write = (b.write + 1) % len(b.buf)
	b.full = b.write == b.read
}

// Read a number of elements from the buffer. Function returns io.EOF if the buffer is empty.
func (b *Buffer[T]) Read(p []T) (int, error) {
	if len(p) == 0 {
		return 0, nil
	} else if b.Len() == 0 {
		return 0, io.EOF
	}
	b.full = false
	var n int

	end := len(b.buf)
	if b.read < b.write {
		end = b.write
	}
	dn := copy(p, b.buf[b.read:end])
	b.read = (b.read + dn) % len(b.buf)
	p = p[dn:]
	n += dn
	if len(p) == 0 || b.read == b.write {
		return n, nil
	}
	dn = copy(p, b.buf[b.read:b.write])
	b.read = (b.read + dn) % len(b.buf)
	n += dn
	return n, nil
}

// Write a number of elements into the buffer. Oldest elements will be discarded if the buffer cannot fit all elements.
func (b *Buffer[T]) Write(p []T) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	if dn := len(p) - len(b.buf); dn > 0 {
		n += dn
		p = p[dn:]
	}
	if dn := b.Len() + len(p) - b.Size(); dn >= 0 {
		if len(p) == len(b.buf) {
			n += copy(b.buf, p)
			b.read = 0
			b.write = 0
			b.full = true
			return n, nil
		}
		b.read = (b.read + dn) % len(b.buf) // drop old
	}
	dn := copy(b.buf[b.write:], p)
	b.write = (b.write + dn) % len(b.buf)
	p = p[dn:]
	n += dn
	if len(p) > 0 {
		dn = copy(b.buf[b.write:], p)
		b.write = (b.write + dn) % len(b.buf)
		n += dn
	}
	b.full = b.write == b.read
	return n, nil
}
