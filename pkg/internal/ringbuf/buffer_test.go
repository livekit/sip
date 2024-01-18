package ringbuf

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	_ io.ReadWriter = &Buffer[byte]{}
)

func TestBuffer(t *testing.T) {
	t.Run("push and pop", func(t *testing.T) {
		t.Run("empty return zero", func(t *testing.T) {
			b := New[int](3)
			require.Equal(t, int(0), b.Peek())
			v, ok := b.TryPop()
			require.False(t, ok)
			require.Equal(t, int(0), v)
			require.Equal(t, 0, b.Len())
		})
		t.Run("populate and drain", func(t *testing.T) {
			const size = 3
			b := New[int](size)
			for i := 0; i < size; i++ {
				ok := b.TryPush(i + 1)
				require.True(t, ok)
				require.Equal(t, i+1, b.Len())
			}
			ok := b.TryPush(-1)
			require.False(t, ok)
			for i := 0; i < size; i++ {
				v, ok := b.TryPop()
				require.True(t, ok)
				require.Equal(t, i+1, v)
				require.Equal(t, size-(i+1), b.Len())
			}
			v, ok := b.TryPop()
			require.False(t, ok)
			require.Equal(t, int(0), v)
			require.Equal(t, 0, b.Len())
		})
		t.Run("populate and drain no check", func(t *testing.T) {
			const size = 3
			b := New[int](size)
			for i := 0; i < size; i++ {
				b.Push(i + 1)
				require.Equal(t, i+1, b.Len())
			}
			ok := b.TryPush(-1)
			require.False(t, ok)
			for i := 0; i < size; i++ {
				v := b.Pop()
				require.Equal(t, i+1, v)
				require.Equal(t, size-(i+1), b.Len())
			}
			v := b.Pop()
			require.Equal(t, int(0), v)
			require.Equal(t, 0, b.Len())
		})
	})
	t.Run("write and read", func(t *testing.T) {
		t.Run("empty return EOF", func(t *testing.T) {
			b := New[int](5)
			buf := make([]int, 6)
			n, err := b.Read(buf)
			require.Equal(t, io.EOF, err)
			require.Equal(t, 0, n)
		})
		t.Run("full", func(t *testing.T) {
			b := New[int](5)
			n, err := b.Write([]int{1, 2, 3, 4, 5})
			require.NoError(t, err)
			require.Equal(t, 5, n)
			require.Equal(t, 5, b.Len())

			buf := make([]int, 6)
			n, err = b.Read(buf)
			require.NoError(t, err)
			require.Equal(t, []int{1, 2, 3, 4, 5}, buf[:n])
		})
		t.Run("full via parts", func(t *testing.T) {
			b := New[int](5)
			n, err := b.Write([]int{1, 2, 3})
			require.NoError(t, err)
			require.Equal(t, 3, n)
			n, err = b.Write([]int{4, 5})
			require.NoError(t, err)
			require.Equal(t, 2, n)
			require.Equal(t, 5, b.Len())

			buf := make([]int, 6)
			n, err = b.Read(buf[:2])
			require.NoError(t, err)
			require.Equal(t, 2, n)
			n, err = b.Read(buf[2:])
			require.NoError(t, err)
			require.Equal(t, 3, n)
			require.Equal(t, []int{1, 2, 3, 4, 5}, buf[:5])
		})
		t.Run("too large", func(t *testing.T) {
			b := New[int](5)
			n, err := b.Write([]int{1, 2, 3, 4, 5, 6, 7})
			require.NoError(t, err)
			require.Equal(t, 7, n)
			require.Equal(t, 5, b.Len())

			buf := make([]int, 6)
			n, err = b.Read(buf)
			require.NoError(t, err)
			require.Equal(t, []int{3, 4, 5, 6, 7}, buf[:n])
		})
		t.Run("too large with existing", func(t *testing.T) {
			b := New[int](5)
			n, err := b.Write([]int{1, 2, 3})
			require.NoError(t, err)
			require.Equal(t, 3, n)
			n, err = b.Write([]int{4, 5, 6, 7})
			require.NoError(t, err)
			require.Equal(t, 4, n)
			require.Equal(t, 5, b.Len())

			buf := make([]int, 6)
			n, err = b.Read(buf)
			require.NoError(t, err)
			require.Equal(t, []int{3, 4, 5, 6, 7}, buf[:n])
		})
		t.Run("interleaved", func(t *testing.T) {
			b := New[int](6)
			buf := make([]int, 6)

			n, err := b.Write([]int{1, 2, 3, 4})
			require.NoError(t, err)
			require.Equal(t, 4, n)

			n, err = b.Read(buf[:3])
			require.NoError(t, err)
			require.Equal(t, []int{1, 2, 3}, buf[:n])

			n, err = b.Write([]int{5, 6, 10, 11, 12})
			require.NoError(t, err)
			require.Equal(t, 5, n)

			n, err = b.Read(buf)
			require.NoError(t, err)
			require.Equal(t, []int{4, 5, 6, 10, 11, 12}, buf[:n])
		})
		t.Run("interleaved drop", func(t *testing.T) {
			b := New[int](6)
			buf := make([]int, 6)

			n, err := b.Write([]int{1, 2, 3, 4})
			require.NoError(t, err)
			require.Equal(t, 4, n)

			n, err = b.Read(buf[:2])
			require.NoError(t, err)
			require.Equal(t, []int{1, 2}, buf[:n])

			n, err = b.Write([]int{5, 6, 10, 11, 12})
			require.NoError(t, err)
			require.Equal(t, 5, n)

			n, err = b.Read(buf)
			require.NoError(t, err)
			require.Equal(t, []int{4, 5, 6, 10, 11, 12}, buf[:n])
		})
	})
}
