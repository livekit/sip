package interceptor

import "sync"

type PacketPool struct {
	pools map[int]*sync.Pool // key: size
}

func NewPacketPool(size ...int) *PacketPool {
	pools := make(map[int]*sync.Pool)
	for _, s := range size {
		bufSize := s
		pools[bufSize] = &sync.Pool{
			New: func() interface{} {
				b := make([]byte, bufSize)
				return &b
			},
		}
	}
	return &PacketPool{pools: pools}
}

func (p *PacketPool) Get(size int) (*[]byte, *sync.Pool) {
	for s, pool := range p.pools {
		if s >= size {
			return pool.Get().(*[]byte), pool
		}
	}
	b := make([]byte, size)
	return &b, nil
}
